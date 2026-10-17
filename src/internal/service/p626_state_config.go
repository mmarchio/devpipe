// P6-26 (WBS 6.36): the state-configuration service — the service half of
// the State Configuration API. It exposes state management within an
// editable state-machine definition: create state, update state (incl.
// disable), assign role, configure instructions, configure state files,
// configure the prompt-template reference — and protects historically
// referenced states from destructive deletion (T6.36.5).
//
// It composes the prior-task repositories (P6-05/P6-06 machine+states+
// transitions, P6-07 role assignments, P6-08 state instructions, P6-09
// state-file requirements) — no parallel assignment path. Every op
// resolves the project's machine family the same way as P6-25 (the
// family whose MAX-version row is highest), operates on that family's
// CURRENT version rows, and runs in ONE transaction when a pool is wired
// (WBS 2.21; nil-pool sequential fallback for unit scope).
package service

import (
	"context"
	"fmt"
	"overseer/internal/bounds"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// StateConfigService implements the WBS 6.36 state-configuration surface.
type StateConfigService struct {
	machines persistence.StateMachineRepository
	states   persistence.WorkflowStateRepository
	trans    persistence.StateTransitionRepository
	instr    persistence.StateInstructionRepository
	assigns  persistence.StateRoleAssignmentRepository
	files    persistence.StateFileRequirementRepository
	roles    persistence.RoleRepository
	history  persistence.TransitionHistoryRepository
	runtime  persistence.RuntimeStateRepository
	txPool   *pgxpool.Pool
}

// NewStateConfigService wires the service to its repositories.
func NewStateConfigService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	instructions persistence.StateInstructionRepository,
	assignments persistence.StateRoleAssignmentRepository,
	stateFiles persistence.StateFileRequirementRepository,
	roles persistence.RoleRepository,
	history persistence.TransitionHistoryRepository,
	runtimeState persistence.RuntimeStateRepository,
) *StateConfigService {
	return &StateConfigService{
		machines: machines, states: states, trans: transitions,
		instr: instructions, assigns: assignments, files: stateFiles,
		roles: roles, history: history, runtime: runtimeState,
	}
}

// WithTxPool wires the transaction pool for the atomic path (WBS 2.21);
// with a nil pool the ops run sequentially (single-goroutine unit scope).
func (s *StateConfigService) WithTxPool(pool *pgxpool.Pool) *StateConfigService {
	s.txPool = pool
	return s
}

// CreateStateInput is the payload for the create-state op (T6.36.1).
// Enabled defaults to true when nil (the P6-02 domain convention).
type CreateStateInput struct {
	Key            string
	Name           string
	Description    string
	PromptTemplate string
	Kind           string
	Terminal       bool
	Enabled        *bool
}

// StateConfigView is the service-level view of a configured state: the
// persisted definition plus its live configuration (assigned role keys in
// ordinal order, required state-file categories in ordinal order, and the
// current state-instruction version + hash when an instruction exists).
type StateConfigView struct {
	Key             string
	Name            string
	Description     string
	PromptTemplate  string
	Kind            string
	Terminal        bool
	Enabled         bool
	Roles           []string
	StateFiles      []string
	InstructionVer  int
	InstructionHash string
}

type p626Repos struct {
	machines persistence.StateMachineRepository
	states   persistence.WorkflowStateRepository
	trans    persistence.StateTransitionRepository
	instr    persistence.StateInstructionRepository
	assigns  persistence.StateRoleAssignmentRepository
	files    persistence.StateFileRequirementRepository
	roles    persistence.RoleRepository
	history  persistence.TransitionHistoryRepository
	runtime  persistence.RuntimeStateRepository
}

func (s *StateConfigService) bind(tx persistence.Transaction) p626Repos {
	if tx == nil {
		return p626Repos{
			machines: s.machines, states: s.states, trans: s.trans,
			instr: s.instr, assigns: s.assigns, files: s.files,
			roles: s.roles, history: s.history, runtime: s.runtime,
		}
	}
	return p626Repos{
		machines: s.machines.WithTx(tx), states: s.states.WithTx(tx),
		trans: s.trans.WithTx(tx), instr: s.instr.WithTx(tx),
		assigns: s.assigns.WithTx(tx), files: s.files.WithTx(tx),
		roles: s.roles.WithTx(tx), history: s.history.WithTx(tx),
		runtime: s.runtime.WithTx(tx),
	}
}

// runOnce executes fn inside one transaction when a pool is wired, else
// sequentially.
func (s *StateConfigService) runOnce(ctx context.Context, fn func(tx persistence.Transaction) error) error {
	if s.txPool == nil {
		return fn(nil)
	}
	return dbrepo.Run(ctx, s.txPool, fn)
}

// resolveCurrent returns the family's current machine record (the
// highest-version row of the resolved family) — the op target.
func (s *StateConfigService) resolveCurrent(ctx context.Context, r p626Repos, projectID string) (*persistence.MachineRecord, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	list, err := r.machines.ListByProject(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	var best *persistence.MachineRecord
	for _, m := range list {
		if best == nil ||
			m.Version > best.Version ||
			(m.Version == best.Version && m.FamilyID.String() < best.FamilyID.String()) {
			best = m
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// lookupState finds the current machine's state with the given key;
// unknown keys report ErrNotFound (404 on the REST surface).
func lookupState(ctx context.Context, r p626Repos, machine *persistence.MachineRecord, key string) (*persistence.WorkflowStateRecord, error) {
	states, err := r.states.ListByMachine(ctx, machine.ID, machine.Version)
	if err != nil {
		return nil, translate(err)
	}
	nk := state.NormalizeKey(key)
	for _, st := range states {
		if st.Key == nk {
			return st, nil
		}
	}
	return nil, ErrNotFound
}

// assembleView reads the state's live configuration (roles, files,
// instruction) for the returned view.
func assembleView(ctx context.Context, r p626Repos, st *persistence.WorkflowStateRecord) (*StateConfigView, error) {
	v := &StateConfigView{
		Key: st.Key, Name: st.Name, Description: st.Description,
		PromptTemplate: st.PromptTemplate, Kind: st.Kind,
		Terminal: st.Terminal, Enabled: st.Enabled,
		Roles:      []string{},
		StateFiles: []string{},
	}
	asgns, err := r.assigns.ListByState(ctx, st.ID)
	if err != nil {
		return nil, translate(err)
	}
	for _, a := range asgns {
		role, err := r.roles.Get(ctx, a.RoleID.String())
		if err != nil {
			return nil, translate(err)
		}
		v.Roles = append(v.Roles, role.Key)
	}
	files, err := r.files.ListByState(ctx, st.ID)
	if err != nil {
		return nil, translate(err)
	}
	for _, f := range files {
		v.StateFiles = append(v.StateFiles, f.Category)
	}
	cur, err := r.instr.GetCurrentVersion(ctx, st.ID)
	if err != nil {
		return nil, translate(err)
	}
	if cur > 0 {
		ver, err := r.instr.GetVersion(ctx, st.ID, cur)
		if err != nil {
			return nil, translate(err)
		}
		v.InstructionVer = cur
		v.InstructionHash = ver.ContentHash
	}
	return v, nil
}

// CreateState adds a new state to the current machine definition
// (T6.36.1): the row joins the current version (a state edit is not a
// full-definition version bump — WBS 6.36 operates WITHIN the editable
// definition), with domain-normalized key/template and validated kind.
// A duplicate key reports ErrConflict (409).
func (s *StateConfigService) CreateState(ctx context.Context, projectID string, in *CreateStateInput) (*StateConfigView, error) {
	if in == nil || in.Name == "" {
		return nil, ErrInvalidInput
	}
	if !state.StateKind(in.Kind).Known() {
		return nil, ErrInvalidInput
	}
	var out *StateConfigView
	err := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		rec := &persistence.WorkflowStateRecord{
			ID: uuid.New(), MachineID: machine.ID, Version: machine.Version,
			Key: state.NormalizeKey(in.Key), Name: in.Name,
			Description: in.Description, PromptTemplate: state.NormalizeKey(in.PromptTemplate),
			Kind: in.Kind, Terminal: in.Terminal, Enabled: enabled,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := r.states.Create(ctx, rec); err != nil {
			return translate(err)
		}
		view, err := assembleView(ctx, r, rec)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateStateInput carries the editable definition fields of an update.
// Pointer fields distinguish "not supplied" (keep stored value) from an
// explicit value; Enabled nil keeps the stored liveness.
type UpdateStateInput struct {
	Name           *string
	Description    *string
	PromptTemplate *string
	Kind           *string
	Terminal       *bool
	Enabled        *bool
}

// UpdateState mutates the state's definition (T6.36.2); a disable is an
// update with Enabled=false. Identity fields (key, machine, version) are
// immutable. Unknown keys report ErrNotFound.
func (s *StateConfigService) UpdateState(ctx context.Context, projectID, key string, in *UpdateStateInput) (*StateConfigView, error) {
	if in == nil {
		return nil, ErrInvalidInput
	}
	var out *StateConfigView
	err := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		st, err := lookupState(ctx, r, machine, key)
		if err != nil {
			return err
		}
		if in.Name != nil && *in.Name == "" {
			return ErrInvalidInput
		}
		rec := &persistence.WorkflowStateRecord{
			ID: st.ID, MachineID: st.MachineID, Version: st.Version,
			Key: st.Key, Name: st.Name, Description: st.Description,
			PromptTemplate: st.PromptTemplate, Kind: st.Kind,
			Terminal: st.Terminal, Enabled: st.Enabled,
			CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt,
		}
		if in.Name != nil {
			rec.Name = *in.Name
		}
		if in.Description != nil {
			rec.Description = *in.Description
		}
		if in.PromptTemplate != nil {
			rec.PromptTemplate = state.NormalizeKey(*in.PromptTemplate)
		}
		if in.Kind != nil {
			if !state.StateKind(*in.Kind).Known() {
				return ErrInvalidInput
			}
			rec.Kind = *in.Kind
		}
		if in.Terminal != nil {
			rec.Terminal = *in.Terminal
		}
		if in.Enabled != nil {
			rec.Enabled = *in.Enabled
		}
		upd, err := r.states.Update(ctx, rec)
		if err != nil {
			return translate(err)
		}
		view, err := assembleView(ctx, r, upd)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AssignRole appends the role to the state's ordered participating-role
// list (T6.36.3). The role must exist (404); a duplicate (state, role)
// reports ErrConflict (409). The cross-project rejection is the
// DB-level rule from P6-07.
func (s *StateConfigService) AssignRole(ctx context.Context, projectID, key, roleID string) (*StateConfigView, error) {
	rid, err := uuid.Parse(roleID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var out *StateConfigView
	e := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		if _, err := r.roles.Get(ctx, roleID); err != nil {
			return translate(err)
		}
		st, err := lookupState(ctx, r, machine, key)
		if err != nil {
			return err
		}
		asgns, err := r.assigns.ListByState(ctx, st.ID)
		if err != nil {
			return translate(err)
		}
		now := time.Now().UTC()
		rec := &persistence.StateRoleAssignmentRecord{
			ID: uuid.New(), StateID: st.ID, RoleID: rid, Ordinal: len(asgns),
			CreatedAt: now, UpdatedAt: now,
		}
		if err := r.assigns.Create(ctx, rec); err != nil {
			return translate(err)
		}
		view, err := assembleView(ctx, r, st)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// SetStateInstructions atomically replaces the state's instruction with
// the given content: one append-only version is added and it becomes the
// current one (the P6-08 repository op; historical versions are never
// overwritten, T6.5.3). Empty content is rejected.
func (s *StateConfigService) SetStateInstructions(ctx context.Context, projectID, key, content string) (*StateConfigView, error) {
	if err := bounds.Check(bounds.Instruction, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if content == "" {
		return nil, ErrInvalidInput
	}
	var out *StateConfigView
	err := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		st, err := lookupState(ctx, r, machine, key)
		if err != nil {
			return err
		}
		if _, err := r.instr.AppendVersion(ctx, st.ID, content); err != nil {
			return translate(err)
		}
		view, err := assembleView(ctx, r, st)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetStateFiles atomically replaces the state's required state-file
// categories with the given ordered list (the P6-09 repository; the P5-26
// set-replacement semantics: order is significant, a duplicate category is
// rejected, an empty list is rejected).
func (s *StateConfigService) SetStateFiles(ctx context.Context, projectID, key string, categories []string) (*StateConfigView, error) {
	if len(categories) == 0 {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(categories))
	for _, c := range categories {
		if _, ok := seen[c]; ok {
			return nil, ErrInvalidInput
		}
		seen[c] = struct{}{}
	}
	var out *StateConfigView
	err := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		st, err := lookupState(ctx, r, machine, key)
		if err != nil {
			return err
		}
		cur, err := r.files.ListByState(ctx, st.ID)
		if err != nil {
			return translate(err)
		}
		for _, f := range cur {
			if err := r.files.Remove(ctx, st.ID, f.Category); err != nil {
				return translate(err)
			}
		}
		for _, c := range categories {
			if _, err := r.files.Require(ctx, st.ID, c); err != nil {
				return translate(err)
			}
		}
		view, err := assembleView(ctx, r, st)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetPromptTemplate updates the state's referenced prompt-template key
// (T6.36.4 via the update surface; the template body itself is Phase 8
// data — the state stores the reference only). Empty is legal (clears the
// reference).
func (s *StateConfigService) SetPromptTemplate(ctx context.Context, projectID, key, template string) (*StateConfigView, error) {
	tpl := state.NormalizeKey(template)
	in := &UpdateStateInput{PromptTemplate: &tpl}
	return s.UpdateState(ctx, projectID, key, in)
}

// DeleteStateErr is the T6.36.5 protection rejection: the state is
// historically referenced (a current-version transition endpoint, the
// machine's initial state, live runtime state, or a transition-history
// row), so a destructive delete is a conflict carrying the protecting
// reference. It maps to 409 on the REST surface (its Is matches
// ErrConflict, so errors.Is chains work as for any conflict).
type DeleteStateErr struct {
	Reason string
}

func (e *DeleteStateErr) Error() string {
	return "state is historically referenced: " + e.Reason
}
func (e *DeleteStateErr) Is(target error) bool { return target == ErrConflict }

// referencedState reports whether the state is historically referenced and
// the protecting reference (initial-state / transition / runtime-state /
// transition-history).
func (s *StateConfigService) referencedState(ctx context.Context, r p626Repos, machine *persistence.MachineRecord, st *persistence.WorkflowStateRecord) (bool, string) {
	if machine.InitialStateID != nil && *machine.InitialStateID == st.ID {
		return true, "initial-state"
	}
	refs, err := r.trans.ReferencedByStates(ctx, machine.ID, machine.Version)
	if err != nil {
		return false, ""
	}
	if refs[st.ID] {
		return true, "transition"
	}
	rs, err := r.runtime.Get(ctx, machine.ProjectID)
	if err == nil && rs.CurrentStateID == st.ID {
		return true, "runtime-state"
	}
	hist, err := r.history.ListByStateID(ctx, st.ID)
	if err == nil && len(hist) > 0 {
		return true, "transition-history"
	}
	return false, ""
}

// DeleteState removes the state row (T6.36.5): first it rejects
// historically referenced states with *DeleteStateErr BEFORE any write,
// then the delete removes the row and its child configuration (role
// assignments, instruction history, file requirements) cascade at the DB
// level. Unknown keys report ErrNotFound.
func (s *StateConfigService) DeleteState(ctx context.Context, projectID, key string) error {
	return s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		st, err := lookupState(ctx, r, machine, key)
		if err != nil {
			return err
		}
		if ref, why := s.referencedState(ctx, r, machine, st); ref {
			return &DeleteStateErr{Reason: why}
		}
		if err := r.states.Delete(ctx, st.ID); err != nil {
			return translate(err)
		}
		return nil
	})
}

// ListStates returns the current machine's states with their live
// configuration (roles, state files, instruction version) — read-only
// (WBS 17.36 / 17.38).
func (s *StateConfigService) ListStates(ctx context.Context, projectID string) ([]*StateConfigView, error) {
	r := s.bind(nil)
	m, err := s.resolveCurrent(ctx, r, projectID)
	if err != nil {
		return nil, err
	}
	states, err := r.states.ListByMachine(ctx, m.ID, m.Version)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]*StateConfigView, 0, len(states))
	for _, st := range states {
		v, err := assembleView(ctx, r, st)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// GetState returns one state's live configuration with its current
// instruction content (read-only).
func (s *StateConfigService) GetState(ctx context.Context, projectID, key string) (*StateConfigView, string, error) {
	r := s.bind(nil)
	m, err := s.resolveCurrent(ctx, r, projectID)
	if err != nil {
		return nil, "", err
	}
	st, err := lookupState(ctx, r, m, key)
	if err != nil {
		return nil, "", err
	}
	v, err := assembleView(ctx, r, st)
	if err != nil {
		return nil, "", err
	}
	content := ""
	if v.InstructionVer > 0 {
		if ver, err := r.instr.GetVersion(ctx, st.ID, v.InstructionVer); err == nil {
			content = ver.Content
		}
	}
	return v, content, nil
}
