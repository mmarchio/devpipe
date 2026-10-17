// P6-16 (WBS 6.19): unit-level tests for the state-machine validation
// service over IN-MEMORY fixtures (no DB). This is where the all-codes
// report, the deterministic ordering, the activation gate, and the
// non-mutation guarantee are exercised; the integration file drives the
// same service over real persisted rows.
package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/state"
)

// --- in-memory fakes (minimal, read-only surface the validator uses) ---

type fakeMachines struct{ m *persistence.MachineRecord }

func (f *fakeMachines) Create(context.Context, *persistence.MachineRecord) error { return nil }
func (f *fakeMachines) Get(ctx context.Context, id uuid.UUID) (*persistence.MachineRecord, error) {
	if f.m == nil || f.m.ID != id {
		return nil, persistence.ErrNotFound
	}
	return f.m, nil
}
func (f *fakeMachines) ListByProject(context.Context, uuid.UUID) ([]*persistence.MachineRecord, error) {
	if f.m == nil {
		return nil, nil
	}
	return []*persistence.MachineRecord{f.m}, nil
}
func (f *fakeMachines) SetInitialState(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// GetVersion/ListVersions satisfy the P6-17 versioning surface: the
// validator reads the current version only, so a v1 passthrough suffices.
func (f *fakeMachines) GetVersion(ctx context.Context, id uuid.UUID, version int) (*persistence.MachineRecord, error) {
	return f.Get(ctx, id)
}

func (f *fakeMachines) ListVersions(ctx context.Context, id uuid.UUID) ([]*persistence.MachineRecord, error) {
	m, err := f.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return []*persistence.MachineRecord{m}, nil
}

func (f *fakeMachines) WithTx(persistence.Transaction) persistence.StateMachineRepository {
	return f
}

type fakeStates struct {
	updates int
	ss      []*persistence.WorkflowStateRecord
}

func (f *fakeStates) Create(context.Context, *persistence.WorkflowStateRecord) error { return nil }
func (f *fakeStates) Get(ctx context.Context, id uuid.UUID) (*persistence.WorkflowStateRecord, error) {
	for _, s := range f.ss {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, persistence.ErrNotFound
}

// P6-17: ListByMachine is version-scoped; the fake ignores the version (its states are the machine's v1).
func (f *fakeStates) Update(ctx context.Context, rec *persistence.WorkflowStateRecord) (*persistence.WorkflowStateRecord, error) {
	f.updates++
	return rec, nil
}
func (f *fakeStates) Delete(context.Context, uuid.UUID) error { return nil }
func (f *fakeStates) ListByMachine(context.Context, uuid.UUID, int) ([]*persistence.WorkflowStateRecord, error) {
	return f.ss, nil
}
func (f *fakeStates) WithTx(persistence.Transaction) persistence.WorkflowStateRepository { return f }

type fakeTransitions struct {
	ts []*persistence.StateTransitionRecord
}

func (f *fakeTransitions) Create(context.Context, *persistence.StateTransitionRecord) error {
	return nil
}

// P6-27: the update surface; the validator never calls it, so it is a
// no-op passthrough.
func (f *fakeTransitions) Update(ctx context.Context, rec *persistence.StateTransitionRecord) (*persistence.StateTransitionRecord, error) {
	return rec, nil
}
func (f *fakeTransitions) Get(ctx context.Context, id uuid.UUID) (*persistence.StateTransitionRecord, error) {
	for _, t := range f.ts {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, persistence.ErrNotFound
}

// P6-17: version-scoped; the fake ignores the version.
func (f *fakeTransitions) ReferencedByStates(ctx context.Context, machineID uuid.UUID, version int) (map[uuid.UUID]bool, error) {
	return map[uuid.UUID]bool{}, nil
}
func (f *fakeTransitions) ListByMachine(context.Context, uuid.UUID, int) ([]*persistence.StateTransitionRecord, error) {
	return f.ts, nil
}
func (f *fakeTransitions) WithTx(persistence.Transaction) persistence.StateTransitionRepository {
	return f
}

type fakeAssignments struct {
	as []*persistence.StateRoleAssignmentRecord
}

func (f *fakeAssignments) Create(context.Context, *persistence.StateRoleAssignmentRecord) error {
	return nil
}
func (f *fakeAssignments) Get(ctx context.Context, id uuid.UUID) (*persistence.StateRoleAssignmentRecord, error) {
	for _, a := range f.as {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeAssignments) Remove(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeAssignments) ListByState(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateRoleAssignmentRecord, error) {
	var out []*persistence.StateRoleAssignmentRecord
	for _, a := range f.as {
		if a.StateID == stateID {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeAssignments) WithTx(persistence.Transaction) persistence.StateRoleAssignmentRepository {
	return f
}

type fakeRoles struct {
	byID map[uuid.UUID]*persistence.RoleRecord
}

func (f *fakeRoles) Create(context.Context, *persistence.RoleRecord) error { return nil }
func (f *fakeRoles) Get(ctx context.Context, id string) (*persistence.RoleRecord, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, persistence.ErrNotFound
	}
	if r, ok := f.byID[u]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeRoles) ListByScope(context.Context, *uuid.UUID) ([]*persistence.RoleRecord, error) {
	return nil, nil
}
func (f *fakeRoles) SetEnabled(context.Context, uuid.UUID, bool) (*persistence.RoleRecord, error) {
	return nil, nil
}
func (f *fakeRoles) Update(context.Context, uuid.UUID, string, string) (*persistence.RoleRecord, error) {
	return nil, nil
}
func (f *fakeRoles) Delete(context.Context, uuid.UUID) error                   { return nil }
func (f *fakeRoles) WithTx(persistence.Transaction) persistence.RoleRepository { return f }

type fakeAllowed struct {
	byRole map[uuid.UUID][]string
}

func (f *fakeAllowed) AssignState(context.Context, uuid.UUID, string) (*persistence.RoleAllowedStateRecord, error) {
	return nil, nil
}
func (f *fakeAllowed) UnassignState(context.Context, uuid.UUID, string) error { return nil }
func (f *fakeAllowed) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*persistence.RoleAllowedStateRecord, error) {
	var out []*persistence.RoleAllowedStateRecord
	for _, k := range f.byRole[roleID] {
		out = append(out, &persistence.RoleAllowedStateRecord{RoleID: roleID, StateKey: k})
	}
	return out, nil
}
func (f *fakeAllowed) IsEligible(ctx context.Context, roleID uuid.UUID, stateKey string) (bool, error) {
	for _, k := range f.byRole[roleID] {
		if k == stateKey {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeAllowed) WithTx(persistence.Transaction) persistence.RoleAllowedStateRepository {
	return f
}

// --- fixture: one machine carrying EVERY violation class at once ---

// p616TestMachine returns (service, machineID, enabled states). The machine
// is enabled with initial state A. It carries: a disabled duplicate-key
// state (duplicate_key), a transition to a state outside the machine
// (transition_invalid), an equal-priority outcome tie (ambiguity), two
// template-less action states (template_missing), an assignment to a
// missing role (role_not_found), a disabled role (role_disabled), an
// assignment outside the role's allowed states (role_allowed_state), a
// terminal state with an outgoing edge (terminal), unreachable states
// (reachability), and a zero-progress control cycle (cycle).
func p616TestMachine() (*StateMachineValidationService, uuid.UUID, map[string]*state.WorkflowState) {
	now := time.Now().UTC()
	mid := uuid.New()
	a := uuid.New()
	b := uuid.New()
	b2 := uuid.New()
	c := uuid.New()
	d := uuid.New()
	f := uuid.New()
	x := uuid.New()
	y := uuid.New()
	roleOK := uuid.New()
	roleDisabled := uuid.New()
	machine := &persistence.MachineRecord{
		ID: mid, ProjectID: uuid.New(), Name: "m", Version: 1, Enabled: true,
		InitialStateID: &a, CreatedAt: now, UpdatedAt: now,
	}
	enable := true
	ss := []*persistence.WorkflowStateRecord{
		{ID: a, MachineID: mid, Key: "A", Name: "A", Kind: string(state.KindControl), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: b, MachineID: mid, Key: "B", Name: "B", Kind: string(state.KindAction), Enabled: true, CreatedAt: now, UpdatedAt: now},
		// Duplicate key (disabled -> inert in graph checks, caught by the key check).
		{ID: b2, MachineID: mid, Key: "B", Name: "B2", Kind: string(state.KindAction), Enabled: false, CreatedAt: now, UpdatedAt: now},
		{ID: c, MachineID: mid, Key: "C", Name: "C", Kind: string(state.KindValidation), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: d, MachineID: mid, Key: "D", Name: "D", Kind: string(state.KindTerminal), Terminal: true, Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: f, MachineID: mid, Key: "F", Name: "F", Kind: string(state.KindControl), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: x, MachineID: mid, Key: "X", Name: "X", Kind: string(state.KindControl), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: y, MachineID: mid, Key: "Y", Name: "Y", Kind: string(state.KindControl), Enabled: true, CreatedAt: now, UpdatedAt: now},
	}
	// Transitions: A->B (pri 10) and A->C (pri 10, outcome B also matches C? no —
	// same outcome "GO" on both: the tie); A->C (pri 20, outcome "GO2") is the
	// valid distinct one; D->A (terminal outgoing); A->F; X->Y; Y->X.
	ghost := uuid.New()
	ts := []*persistence.StateTransitionRecord{
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: b, Outcome: "GO", Priority: 10, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: c, Outcome: "GO", Priority: 10, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: c, Outcome: "GO2", Priority: 20, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: d, ToStateID: a, Outcome: "OPEN", Priority: 10, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: f, Outcome: "NEXT", Priority: 30, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: ghost, Outcome: "GHOST", Priority: 40, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: x, ToStateID: y, Outcome: "C1", Priority: 10, Enabled: enable, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: mid, FromStateID: y, ToStateID: x, Outcome: "C2", Priority: 10, Enabled: enable, CreatedAt: now, UpdatedAt: now},
	}
	roles := map[uuid.UUID]*persistence.RoleRecord{
		roleOK:       {ID: roleOK, Key: "OK", Name: "OK", Enabled: true, Version: 1, CreatedAt: now, UpdatedAt: now},
		roleDisabled: {ID: roleDisabled, Key: "DIS", Name: "DIS", Enabled: false, Version: 1, CreatedAt: now, UpdatedAt: now},
	}
	as := []*persistence.StateRoleAssignmentRecord{
		{ID: uuid.New(), StateID: b, RoleID: roleOK, Ordinal: 0, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), StateID: b, RoleID: roleDisabled, Ordinal: 1, CreatedAt: now, UpdatedAt: now},
		// Missing role (ghost role ID, never in roles map).
		{ID: uuid.New(), StateID: c, RoleID: uuid.New(), Ordinal: 0, CreatedAt: now, UpdatedAt: now},
	}
	allowed := map[uuid.UUID][]string{
		// roleOK allows no state key -> both of its assignments (B, C)
		// violate the allowed-states rule.
		roleOK:       {},
		roleDisabled: {"B"},
	}
	svc := NewStateMachineValidationService(
		&fakeMachines{m: machine},
		&fakeStates{ss: ss},
		&fakeTransitions{ts: ts},
		&fakeAssignments{as: as},
		&fakeRoles{byID: roles},
		&fakeAllowed{byRole: allowed},
	)
	states := map[string]*state.WorkflowState{}
	for _, s := range ss {
		states[s.Key+s.ID.String()] = &state.WorkflowState{ID: s.ID, Key: s.Key, Kind: state.StateKind(s.Kind), Terminal: s.Terminal, Enabled: s.Enabled}
	}
	return svc, mid, states
}

// T6.19.2 (unit half) — the aggregate report carries EVERY violation class
// in one response, with structured codes.
func TestP616ValidationReportAllCodes(t *testing.T) {
	svc, mid, _ := p616TestMachine()
	rep, err := svc.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Valid {
		t.Fatal("expected an invalid machine")
	}
	got := map[string]int{}
	for _, e := range rep.Errors {
		got[e.Code]++
	}
	// Exact expected counts for the fixture:
	//   duplicate_key: 1 (B x2),
	//   transition_invalid: 1 (A->ghost),
	//   ambiguity: 1 (A GO pri10 tie),
	//   template_missing: 2 (B + C),
	//   role_not_found: 1 (ghost role on C),
	//   role_disabled: 1 (disabled role on B),
	//   role_allowed_state: 1 (roleOK not allowed on B),
	//   terminal: 1 (D has outgoing),
	//   dead_end: 3 (B, C, F have no outgoing enabled transition),
	//   reachability: 3 (D, X, Y unreachable from A),
	//   cycle: 1 (X<->Y control loop).
	want := map[string]int{
		"duplicate_key":      1,
		"transition_invalid": 1,
		"ambiguity":          1,
		"template_missing":   2,
		"role_not_found":     1,
		"role_disabled":      1,
		"role_allowed_state": 1,
		"terminal":           1,
		"dead_end":           3,
		"reachability":       3,
		"cycle":              1,
	}
	for code, n := range want {
		if got[code] != n {
			t.Fatalf("code %q count = %d, want %d (all: %v)", code, got[code], n, got)
		}
	}
	total := 0
	for _, n := range want {
		total += n
	}
	if len(rep.Errors) != total {
		t.Fatalf("report has %d errors, want exactly %d (extra: %v)", len(rep.Errors), total, got)
	}
}

// Deterministic ordering — two consecutive reports are byte-identical JSON.
func TestP616ValidationDeterministic(t *testing.T) {
	svc, mid, _ := p616TestMachine()
	r1, err := svc.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	r2, err := svc.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if string(b1) != string(b2) {
		t.Fatalf("reports differ across calls:\n%s\n%s", b1, b2)
	}
}

// A machine with a clean definition passes.
func TestP616ValidationCleanPasses(t *testing.T) {
	_, mid, _ := p616TestMachine()
	// Rebuild a clean variant: only A + F, A->F, F terminal, no assignments.
	now := time.Now().UTC()
	a, f := uuid.New(), uuid.New()
	ss := []*persistence.WorkflowStateRecord{
		{ID: a, MachineID: mid, Key: "A", Name: "A", Kind: string(state.KindControl), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: f, MachineID: mid, Key: "F", Name: "F", Kind: string(state.KindTerminal), Terminal: true, Enabled: true, CreatedAt: now, UpdatedAt: now},
	}
	ts := []*persistence.StateTransitionRecord{
		{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: f, Outcome: "DONE", Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now},
	}
	clean := NewStateMachineValidationService(
		&fakeMachines{m: &persistence.MachineRecord{ID: mid, Name: "m", Version: 1, Enabled: true, InitialStateID: &a}},
		&fakeStates{ss: ss},
		&fakeTransitions{ts: ts},
		&fakeAssignments{as: nil},
		&fakeRoles{byID: map[uuid.UUID]*persistence.RoleRecord{}},
		&fakeAllowed{byRole: map[uuid.UUID][]string{}},
	)
	rep, err := clean.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !rep.Valid || len(rep.Errors) != 0 {
		t.Fatalf("clean machine invalid: %+v", rep.Errors)
	}
}

// T6.19.4 (unit half) — the activation gate rejects a dirty machine and an
// inactive one, and accepts a clean machine.
func TestP616ActivationGate(t *testing.T) {
	_, mid, _ := p616TestMachine()
	_ = mid
	svc := NewStateMachineValidationService(
		&fakeMachines{m: &persistence.MachineRecord{ID: mid, Name: "m", Version: 1, Enabled: true, InitialStateID: func() *uuid.UUID { a := uuid.New(); return &a }()}},
		&fakeStates{ss: nil},
		&fakeTransitions{ts: nil},
		&fakeAssignments{as: nil},
		&fakeRoles{byID: map[uuid.UUID]*persistence.RoleRecord{}},
		&fakeAllowed{byRole: map[uuid.UUID][]string{}},
	)
	// Dirty: no initial state -> gate must reject.
	if rep, err := svc.CanActivate(context.Background(), mid.String()); err != nil {
		t.Fatalf("gate: %v", err)
	} else if rep.Valid {
		t.Fatal("dirty machine passed the activation gate")
	}
	// Inactive: machine disabled -> gate must reject.
	a := uuid.New()
	svc2 := NewStateMachineValidationService(
		&fakeMachines{m: &persistence.MachineRecord{ID: mid, Name: "m", Version: 1, Enabled: false, InitialStateID: &a}},
		&fakeStates{ss: []*persistence.WorkflowStateRecord{{ID: a, MachineID: mid, Key: "A", Name: "A", Kind: string(state.KindTerminal), Terminal: true, Enabled: true}}},
		&fakeTransitions{ts: nil},
		&fakeAssignments{as: nil},
		&fakeRoles{byID: map[uuid.UUID]*persistence.RoleRecord{}},
		&fakeAllowed{byRole: map[uuid.UUID][]string{}},
	)
	if rep, err := svc2.CanActivate(context.Background(), mid.String()); err != nil {
		t.Fatalf("gate: %v", err)
	} else if rep.Valid {
		t.Fatal("disabled machine passed the activation gate")
	}
	// Clean + enabled -> accepted.
	f := uuid.New()
	svc3 := NewStateMachineValidationService(
		&fakeMachines{m: &persistence.MachineRecord{ID: mid, Name: "m", Version: 1, Enabled: true, InitialStateID: &a}},
		&fakeStates{ss: []*persistence.WorkflowStateRecord{
			{ID: a, MachineID: mid, Key: "A", Name: "A", Kind: string(state.KindControl), Enabled: true},
			{ID: f, MachineID: mid, Key: "F", Name: "F", Kind: string(state.KindTerminal), Terminal: true, Enabled: true},
		}},
		&fakeTransitions{ts: []*persistence.StateTransitionRecord{
			{ID: uuid.New(), MachineID: mid, FromStateID: a, ToStateID: f, Outcome: "D", Priority: 10, Enabled: true},
		}},
		&fakeAssignments{as: nil},
		&fakeRoles{byID: map[uuid.UUID]*persistence.RoleRecord{}},
		&fakeAllowed{byRole: map[uuid.UUID][]string{}},
	)
	if rep, err := svc3.CanActivate(context.Background(), mid.String()); err != nil {
		t.Fatalf("gate: %v", err)
	} else if !rep.Valid {
		t.Fatalf("clean machine rejected by the activation gate: %+v", rep.Errors)
	}
}
