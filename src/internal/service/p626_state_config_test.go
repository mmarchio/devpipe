// P6-26 (WBS 6.36) unit half: the state-configuration service over
// in-memory fakes of the persistence interfaces. DB-level and REST-level
// proof is the integration half (p626 integration tests over the harness).
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// --- fakes (local to this test file; p626-prefixed per flat-package rule) ---

type p626Machines struct {
	rec *persistence.MachineRecord
}

func (f *p626Machines) Create(context.Context, *persistence.MachineRecord) error { return nil }
func (f *p626Machines) Get(ctx context.Context, id uuid.UUID) (*persistence.MachineRecord, error) {
	if f.rec != nil && f.rec.ID == id {
		return f.rec, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *p626Machines) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.MachineRecord, error) {
	if f.rec == nil || f.rec.ProjectID != projectID {
		return nil, nil
	}
	return []*persistence.MachineRecord{f.rec}, nil
}
func (f *p626Machines) SetInitialState(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *p626Machines) GetVersion(ctx context.Context, id uuid.UUID, version int) (*persistence.MachineRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p626Machines) ListVersions(ctx context.Context, id uuid.UUID) ([]*persistence.MachineRecord, error) {
	return nil, nil
}
func (f *p626Machines) WithTx(persistence.Transaction) persistence.StateMachineRepository {
	return f
}

type p626States struct {
	states  []*persistence.WorkflowStateRecord
	created int
	updated int
	deleted int
	delErr  error
	updErr  error
}

func (f *p626States) Create(ctx context.Context, rec *persistence.WorkflowStateRecord) error {
	f.created++
	f.states = append(f.states, rec)
	return nil
}
func (f *p626States) Get(ctx context.Context, id uuid.UUID) (*persistence.WorkflowStateRecord, error) {
	for _, st := range f.states {
		if st.ID == id {
			return st, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *p626States) ListByMachine(ctx context.Context, machineID uuid.UUID, version int) ([]*persistence.WorkflowStateRecord, error) {
	out := []*persistence.WorkflowStateRecord{}
	for _, st := range f.states {
		if st.MachineID == machineID && st.Version == version {
			out = append(out, st)
		}
	}
	return out, nil
}
func (f *p626States) Update(ctx context.Context, rec *persistence.WorkflowStateRecord) (*persistence.WorkflowStateRecord, error) {
	if f.updErr != nil {
		return nil, f.updErr
	}
	f.updated++
	for i, st := range f.states {
		if st.ID == rec.ID {
			rec.ID = st.ID
			f.states[i] = rec
			return rec, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *p626States) Delete(ctx context.Context, id uuid.UUID) error {
	f.deleted++
	if f.delErr != nil {
		return f.delErr
	}
	for i, st := range f.states {
		if st.ID == id {
			f.states = append(f.states[:i], f.states[i+1:]...)
			return nil
		}
	}
	return persistence.ErrNotFound
}
func (f *p626States) WithTx(persistence.Transaction) persistence.WorkflowStateRepository {
	return f
}

type p626Trans struct {
	refs map[uuid.UUID]bool
}

func (f *p626Trans) Create(context.Context, *persistence.StateTransitionRecord) error {
	return nil
}

// P6-27: the update surface; the state-config service never calls it.
func (f *p626Trans) Update(ctx context.Context, rec *persistence.StateTransitionRecord) (*persistence.StateTransitionRecord, error) {
	return rec, nil
}
func (f *p626Trans) Get(context.Context, uuid.UUID) (*persistence.StateTransitionRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p626Trans) ListByMachine(context.Context, uuid.UUID, int) ([]*persistence.StateTransitionRecord, error) {
	return nil, nil
}
func (f *p626Trans) ReferencedByStates(ctx context.Context, machineID uuid.UUID, version int) (map[uuid.UUID]bool, error) {
	return f.refs, nil
}
func (f *p626Trans) WithTx(persistence.Transaction) persistence.StateTransitionRepository {
	return f
}

type p626Instr struct {
	appendErr error
	appended  []string
	versions  int
}

func (f *p626Instr) AppendVersion(ctx context.Context, stateID uuid.UUID, content string) (*persistence.StateInstructionRecord, error) {
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.appended = append(f.appended, content)
	f.versions++
	return &persistence.StateInstructionRecord{
		StateID: stateID, Version: f.versions, Content: content,
	}, nil
}
func (f *p626Instr) GetVersion(ctx context.Context, stateID uuid.UUID, version int) (*persistence.StateInstructionRecord, error) {
	if version < 1 || version > f.versions {
		return nil, persistence.ErrNotFound
	}
	return &persistence.StateInstructionRecord{
		StateID: stateID, Version: version,
		Content: f.appended[version-1], ContentHash: "h" + itoaSvc(version),
	}, nil
}
func (f *p626Instr) ListVersions(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateInstructionRecord, error) {
	return nil, nil
}
func (f *p626Instr) GetCurrentVersion(ctx context.Context, stateID uuid.UUID) (int, error) {
	return f.versions, nil
}
func (f *p626Instr) WithTx(persistence.Transaction) persistence.StateInstructionRepository {
	return f
}

type p626Assigns struct {
	createErr error
	created   []*persistence.StateRoleAssignmentRecord
	listed    []*persistence.StateRoleAssignmentRecord
}

func (f *p626Assigns) Create(ctx context.Context, rec *persistence.StateRoleAssignmentRecord) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, rec)
	return nil
}
func (f *p626Assigns) Get(context.Context, uuid.UUID) (*persistence.StateRoleAssignmentRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p626Assigns) ListByState(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateRoleAssignmentRecord, error) {
	out := []*persistence.StateRoleAssignmentRecord{}
	for _, a := range f.created {
		if a.StateID == stateID {
			out = append(out, a)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return f.listed, nil
}
func (f *p626Assigns) Remove(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *p626Assigns) WithTx(persistence.Transaction) persistence.StateRoleAssignmentRepository {
	return f
}

type p626Files struct {
	required  []string
	removed   []string
	requireOK bool
}

func (f *p626Files) Require(ctx context.Context, stateID uuid.UUID, category string) (*persistence.StateFileRequirementRecord, error) {
	if !f.requireOK {
		return nil, persistence.ErrConflict
	}
	f.required = append(f.required, category)
	return &persistence.StateFileRequirementRecord{StateID: stateID, Category: category}, nil
}
func (f *p626Files) ListByState(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateFileRequirementRecord, error) {
	out := []*persistence.StateFileRequirementRecord{}
	for _, c := range f.required {
		out = append(out, &persistence.StateFileRequirementRecord{Category: c})
	}
	return out, nil
}
func (f *p626Files) Remove(ctx context.Context, stateID uuid.UUID, category string) error {
	f.removed = append(f.removed, category)
	return nil
}
func (f *p626Files) WithTx(persistence.Transaction) persistence.StateFileRequirementRepository {
	return f
}

type p626Roles struct {
	byID map[uuid.UUID]*persistence.RoleRecord
}

func (f *p626Roles) Create(context.Context, *persistence.RoleRecord) error { return nil }
func (f *p626Roles) Get(ctx context.Context, id string) (*persistence.RoleRecord, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, persistence.ErrNotFound
	}
	if r, ok := f.byID[uid]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *p626Roles) ListByScope(context.Context, *uuid.UUID) ([]*persistence.RoleRecord, error) {
	return nil, nil
}
func (f *p626Roles) SetEnabled(context.Context, uuid.UUID, bool) (*persistence.RoleRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p626Roles) Update(context.Context, uuid.UUID, string, string) (*persistence.RoleRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p626Roles) Delete(context.Context, uuid.UUID) error { return nil }
func (f *p626Roles) WithTx(persistence.Transaction) persistence.RoleRepository {
	return f
}

type p626History struct {
	rows []*persistence.TransitionHistoryRecord
}

func (f *p626History) Append(context.Context, *persistence.TransitionHistoryRecord, bool) error {
	return nil
}
func (f *p626History) ListByProject(context.Context, uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	return nil, nil
}
func (f *p626History) ListByStateID(ctx context.Context, stateID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	out := []*persistence.TransitionHistoryRecord{}
	for _, h := range f.rows {
		if h.FromStateID == stateID || h.ToStateID == stateID {
			out = append(out, h)
		}
	}
	return out, nil
}
func (f *p626History) WithTx(persistence.Transaction) persistence.TransitionHistoryRepository {
	return f
}

type p626Runtime struct {
	cur *persistence.RuntimeStateRecord
}

func (f *p626Runtime) Create(context.Context, *persistence.RuntimeStateRecord) error {
	return nil
}
func (f *p626Runtime) Get(ctx context.Context, projectID uuid.UUID) (*persistence.RuntimeStateRecord, error) {
	if f.cur != nil && f.cur.ProjectID == projectID {
		return f.cur, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *p626Runtime) UpdateWithVersion(context.Context, *persistence.RuntimeStateRecord, int) error {
	return nil
}
func (f *p626Runtime) WithTx(persistence.Transaction) persistence.RuntimeStateRepository {
	return f
}

// p626Fixture wires a service over fakes with a live machine (v1, initial
// state A) and one state A.
func p626Fixture(t *testing.T) (*StateConfigService, *p626States, *p626Roles, *p626Trans, *p626History, *p626Runtime, *p626Instr, *p626Files, *p626Assigns, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	mid := uuid.New()
	aid := uuid.New()
	projID := uuid.New()
	mach := &persistence.MachineRecord{ID: mid, ProjectID: projID, FamilyID: mid, Name: "m", Version: 1, Enabled: true}
	states := &p626States{states: []*persistence.WorkflowStateRecord{{
		ID: aid, MachineID: mid, Version: 1, Key: "A", Name: "A", Kind: "CONTROL", Enabled: true,
	}}}
	roleID := uuid.New()
	roles := &p626Roles{byID: map[uuid.UUID]*persistence.RoleRecord{
		roleID: {ID: roleID, Key: "ENGINEER", Name: "ENGINEER"},
	}}
	trans := &p626Trans{refs: map[uuid.UUID]bool{}}
	instr := &p626Instr{}
	assigns := &p626Assigns{}
	files := &p626Files{requireOK: true}
	hist := &p626History{}
	rt := &p626Runtime{}
	svc := NewStateConfigService(
		&p626Machines{rec: mach}, states, trans,
		instr, assigns, files, roles, hist, rt,
	)
	return svc, states, roles, trans, hist, rt, instr, files, assigns, mid, aid, projID
}

// T6.36.1 (unit half) — State created: an invalid project id is rejected
// before any write; a valid create persists the row.
func TestP626CreateState(t *testing.T) {
	svc, _, _, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	if _, err := svc.CreateState(ctx, "not-a-uuid", &CreateStateInput{Name: "x", Kind: "ACTION"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid project id: err = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.CreateState(ctx, projID.String(), &CreateStateInput{Name: "x", Kind: "BOGUS"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid kind: err = %v, want ErrInvalidInput", err)
	}
}

// T6.36.1 (unit half) — a valid create persists the row and returns the
// view with the normalized key (lowercase input → uppercase stored).
func TestP626CreateStatePersists(t *testing.T) {
	svc, states, _, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	view, err := svc.CreateState(ctx, projID.String(), &CreateStateInput{
		Key: " new-action ", Name: "New Action", Kind: "ACTION",
	})
	if err != nil {
		t.Fatalf("CreateState = %v", err)
	}
	if view.Key != "NEW-ACTION" {
		t.Fatalf("view key = %q, want NEW-ACTION (normalized)", view.Key)
	}
	if states.created != 1 || len(states.states) != 2 {
		t.Fatalf("created = %d, want 1 new row", states.created)
	}
	if !states.states[1].Enabled {
		t.Fatal("new state enabled = false, want the default true")
	}
}

// T6.36.2 (unit half) — State edited: the update mutates the stored row
// and the view reflects it; a disable is an update with Enabled=false.
func TestP626UpdateState(t *testing.T) {
	svc, states, _, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	disabled := false
	name := "Renamed"
	view, err := svc.UpdateState(ctx, projID.String(), "A", &UpdateStateInput{
		Name: &name, Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateState = %v", err)
	}
	if view.Name != "Renamed" || view.Enabled {
		t.Fatalf("view = %+v, want name Renamed + disabled", view)
	}
	if states.updated != 1 {
		t.Fatalf("updated = %d, want 1", states.updated)
	}
	var aid uuid.UUID
	for _, st := range states.states {
		if st.Key == "A" {
			aid = st.ID
		}
	}
	got, _ := states.Get(ctx, aid)
	if got.Name != "Renamed" || got.Enabled {
		t.Fatalf("stored row = %+v, want the mutation persisted", got)
	}
}

// T6.36.3 (unit half) — Role assigned: the op persists the assignment and
// the view lists the role key.
func TestP626AssignRole(t *testing.T) {
	svc, _, roles, _, _, _, _, _, assigns, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	view, err := svc.AssignRole(ctx, projID.String(), "A", roleIDIn(t, roles))
	if err != nil {
		t.Fatalf("AssignRole = %v", err)
	}
	if len(assigns.created) != 1 || len(view.Roles) != 1 || view.Roles[0] != "ENGINEER" {
		t.Fatalf("assigns = %v, view.Roles = %v, want the ENGINEER assignment", assigns.created, view.Roles)
	}
}

// T6.36.3 (unit half) — a duplicate assignment surfaces the repository's
// conflict (the DB unique rule; the service translates it to 409).
func TestP626AssignRoleDuplicate(t *testing.T) {
	svc, _, roles, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	svc.assigns = &p626Assigns{createErr: persistence.ErrConflict}
	_, err := svc.AssignRole(ctx, projID.String(), "A", roleIDIn(t, roles))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate assign: err = %v, want ErrConflict", err)
	}
}

// T6.36.4 (unit half) — State requirements configured: instruction append
// (one new version), state-file replacement (remove prior + require in
// order), and the prompt-template reference update.
func TestP626ConfigureRequirements(t *testing.T) {
	svc, _, _, _, _, _, instr, files, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	if _, err := svc.SetStateInstructions(ctx, projID.String(), "A", "do the work"); err != nil {
		t.Fatalf("SetStateInstructions = %v", err)
	}
	if len(instr.appended) != 1 || instr.versions != 1 {
		t.Fatalf("instruction versions = %d, want 1 append", instr.versions)
	}
	if _, err := svc.SetStateFiles(ctx, projID.String(), "A", []string{"PLAN", "REPORT"}); err != nil {
		t.Fatalf("SetStateFiles = %v", err)
	}
	if files.required == nil {
		t.Fatal("no file requirements recorded")
	}
	tpl := "IMPL_TPL"
	if _, err := svc.SetPromptTemplate(ctx, projID.String(), "A", tpl); err != nil {
		t.Fatalf("SetPromptTemplate = %v", err)
	}
}

// T6.36.4 (unit half) — the set-replacement guards: an empty list and a
// duplicate category are rejected before any write.
func TestP626SetStateFilesGuards(t *testing.T) {
	svc, _, _, _, _, _, _, files, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	if _, err := svc.SetStateFiles(ctx, projID.String(), "A", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty list: err = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.SetStateFiles(ctx, projID.String(), "A", []string{"A", "A"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate: err = %v, want ErrInvalidInput", err)
	}
	if len(files.required) != 0 {
		t.Fatal("rejected input must not write")
	}
}

// T6.36.5 (unit half) — Historically referenced state protected: the
// delete rejects with the conflict BEFORE any write (the state row is
// untouched), for each protecting reference class.
func TestP626DeleteReferencedProtected(t *testing.T) {
	ctx := context.Background()

	// transition-reference
	t.Run("transition", func(t *testing.T) {
		svc, states, _, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
		var aid uuid.UUID
		for _, st := range states.states {
			aid = st.ID
		}
		svc.trans = &p626Trans{refs: map[uuid.UUID]bool{aid: true}}
		err := svc.DeleteState(ctx, projID.String(), "A")
		var dErr *DeleteStateErr
		if !errors.As(err, &dErr) || dErr.Reason != "transition" {
			t.Fatalf("err = %v, want *DeleteStateErr transition", err)
		}
		if states.deleted != 0 {
			t.Fatal("protected delete must not remove the row")
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatal("protection must chain to ErrConflict (409)")
		}
	})
	// initial-state reference
	t.Run("initial-state", func(t *testing.T) {
		svc, _, _, _, _, _, _, _, _, mid, aid, projID := p626Fixture(t)
		svc.machines = &p626Machines{rec: &persistence.MachineRecord{
			ID: mid, ProjectID: projID, FamilyID: mid, Name: "m", Version: 1,
			InitialStateID: &aid,
		}}
		var dErr *DeleteStateErr
		if err := svc.DeleteState(ctx, projID.String(), "A"); !errors.As(err, &dErr) || dErr.Reason != "initial-state" {
			t.Fatalf("err = %v, want initial-state protection", err)
		}
	})
	// transition-history reference
	t.Run("history", func(t *testing.T) {
		svc, _, _, _, _, _, _, _, _, _, aid, projID := p626Fixture(t)
		svc.history = &p626History{rows: []*persistence.TransitionHistoryRecord{{FromStateID: aid}}}
		var dErr *DeleteStateErr
		if err := svc.DeleteState(ctx, projID.String(), "A"); !errors.As(err, &dErr) || dErr.Reason != "transition-history" {
			t.Fatalf("err = %v, want transition-history protection", err)
		}
	})
	// runtime-state reference
	t.Run("runtime-state", func(t *testing.T) {
		svc, _, _, _, _, _, _, _, _, _, aid, projID := p626Fixture(t)
		svc.runtime = &p626Runtime{cur: &persistence.RuntimeStateRecord{ProjectID: projID, CurrentStateID: aid}}
		var dErr *DeleteStateErr
		if err := svc.DeleteState(ctx, projID.String(), "A"); !errors.As(err, &dErr) || dErr.Reason != "runtime-state" {
			t.Fatalf("err = %v, want runtime-state protection", err)
		}
	})
}

// T6.36.5 (unit half) — an unreferenced state deletes cleanly (the DB
// cascade takes the child configuration).
func TestP626DeleteUnreferenced(t *testing.T) {
	svc, states, _, _, _, _, _, _, _, _, _, projID := p626Fixture(t)
	ctx := context.Background()
	if err := svc.DeleteState(ctx, projID.String(), "A"); err != nil {
		t.Fatalf("DeleteState = %v, want nil", err)
	}
	if states.deleted != 1 || len(states.states) != 0 {
		t.Fatal("unreferenced state must be removed")
	}
}

func roleIDIn(t *testing.T, roles *p626Roles) string {
	t.Helper()
	for id := range roles.byID {
		return id.String()
	}
	t.Fatal("no role in fixture")
	return ""
}

func itoaSvc(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
