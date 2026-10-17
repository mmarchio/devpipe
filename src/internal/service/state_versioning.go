// P6-17 (WBS 6.20): state-machine versioning. Edits create new versions
// rather than silently altering the workflow governing active/historical
// execution. The versioning rule is the Phase 4/5 pattern applied to
// machines: an edit appends a NEW machine row (MAX(version)+1, computed
// under a per-machine advisory lock with a bounded retry on the unique
// constraint), fresh state/transition rows for the new version, and
// re-pointed initial state — so v1 and every intermediate version remain
// retrievable with its states/transitions reconstructable, and an active
// execution's (machine, version) binding is never mutated.
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// MachineStateDef is one state of a machine definition edit (WBS 6.20).
type MachineStateDef struct {
	Key  string
	Kind string
}

// MachineTransitionDef is one transition of a machine definition edit.
type MachineTransitionDef struct {
	From    string
	Outcome string
	To      string
}

// MachineEditInput is the full new definition an edit publishes as a new
// version: the non-empty state set, the transition set, and the initial
// state's key.
type MachineEditInput struct {
	InitialStateKey string
	States          []MachineStateDef
	Transitions     []MachineTransitionDef
}

// StateMachineVersioningService implements the edit-creates-a-version rule
// (WBS 6.20). It coordinates the machine, state, and transition
// repositories; no check logic is duplicated from P6-14/P6-15/P6-16.
type StateMachineVersioningService struct {
	machines    persistence.StateMachineRepository
	states      persistence.WorkflowStateRepository
	transitions persistence.StateTransitionRepository
	txPool      *pgxpool.Pool
}

// NewStateMachineVersioningService wires the versioning service to its
// repositories.
func NewStateMachineVersioningService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
) *StateMachineVersioningService {
	return &StateMachineVersioningService{
		machines:    machines,
		states:      states,
		transitions: transitions,
	}
}

// WithTxPool wires the transaction pool for the atomic edit path (WBS 6.20).
// When nil, the edit runs sequentially (single-goroutine test scope).
func (s *StateMachineVersioningService) WithTxPool(pool *pgxpool.Pool) *StateMachineVersioningService {
	s.txPool = pool
	return s
}

// Edit publishes in as a NEW version of the machine (WBS 6.20, T6.20.2). It
// returns the new version's machine row ID. Validation: the input must be a
// non-empty state set with a known, non-duplicate initial key and
// transitions referencing known states (service.ErrInvalidInput before any
// write). The machine family's next version number is computed under a
// machine-scoped advisory lock with a bounded retry on the UNIQUE
// (project_id, name, version) constraint, so concurrent edits receive
// distinct versions.
func (s *StateMachineVersioningService) Edit(ctx context.Context, machineID string, in *MachineEditInput) (string, error) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		return "", ErrInvalidInput
	}
	if err := validateEditInput(in); err != nil {
		return "", err
	}
	if s.txPool == nil {
		return s.editOnce(ctx, nil, id, in)
	}
	var out string
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			var ee error
			out, ee = s.editOnce(ctx, tx, id, in)
			return ee
		})
		if e == nil {
			return out, nil
		}
		if !errors.Is(e, persistence.ErrConflict) {
			return "", e
		}
		lastErr = e
	}
	return "", lastErr
}

func (s *StateMachineVersioningService) bind(tx persistence.Transaction) (
	persistence.StateMachineRepository,
	persistence.WorkflowStateRepository,
	persistence.StateTransitionRepository,
) {
	if tx == nil {
		return s.machines, s.states, s.transitions
	}
	return s.machines.WithTx(tx), s.states.WithTx(tx), s.transitions.WithTx(tx)
}

// editOnce performs one edit attempt against the given transaction (nil for
// the sequential path).
func (s *StateMachineVersioningService) editOnce(ctx context.Context, tx persistence.Transaction, machineID uuid.UUID, in *MachineEditInput) (string, error) {
	machines, states, transitions := s.bind(tx)
	if tx != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "machine-versions:"+machineID.String()); err != nil {
			return "", translate(err)
		}
	}
	existing, err := machines.Get(ctx, machineID)
	if err != nil {
		return "", translate(err)
	}
	vers, err := machines.ListVersions(ctx, machineID)
	if err != nil {
		return "", translate(err)
	}
	next := existing.Version
	for _, v := range vers {
		if v.Version > next {
			next = v.Version
		}
	}
	keys := map[string]bool{}
	ids := map[string]uuid.UUID{}
	for _, st := range in.States {
		keys[st.Key] = true
		ids[st.Key] = uuid.New()
	}
	if !keys[in.InitialStateKey] {
		return "", ErrInvalidInput
	}
	now := time.Now().UTC()
	newID := uuid.New()
	mrec := &persistence.MachineRecord{
		ID: newID, ProjectID: existing.ProjectID, FamilyID: existing.FamilyID,
		Name: existing.Name, Description: existing.Description,
		Version: next + 1, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := machines.Create(ctx, mrec); err != nil {
		return "", translate(err)
	}
	for _, st := range in.States {
		srec := &persistence.WorkflowStateRecord{
			ID: ids[st.Key], MachineID: newID, Version: next + 1,
			Key: st.Key, Name: st.Key, Kind: st.Kind, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := states.Create(ctx, srec); err != nil {
			return "", translate(err)
		}
	}
	if err := machines.SetInitialState(ctx, newID, ids[in.InitialStateKey]); err != nil {
		return "", translate(err)
	}
	for _, tr := range in.Transitions {
		trec := &persistence.StateTransitionRecord{
			ID: uuid.New(), MachineID: newID, Version: next + 1,
			FromStateID: ids[tr.From], ToStateID: ids[tr.To],
			Outcome: tr.Outcome, Enabled: true, CreatedAt: now, UpdatedAt: now,
		}
		if err := transitions.Create(ctx, trec); err != nil {
			return "", translate(err)
		}
	}
	return newID.String(), nil
}

// validateEditInput rejects an edit before any write (service-level input
// validation, WBS 6.20): non-empty state set, unique keys, a known initial
// key, and transitions whose endpoints are known keys.
func validateEditInput(in *MachineEditInput) error {
	if in == nil || len(in.States) == 0 {
		return ErrInvalidInput
	}
	seen := map[string]bool{}
	for _, st := range in.States {
		if st.Key == "" || seen[st.Key] {
			return ErrInvalidInput
		}
		seen[st.Key] = true
	}
	if !seen[in.InitialStateKey] {
		return ErrInvalidInput
	}
	for _, tr := range in.Transitions {
		if !seen[tr.From] || !seen[tr.To] {
			return ErrInvalidInput
		}
	}
	return nil
}
