// P6-22 (WBS 6.30): initial runtime state. The integration half drives
// the Initialize op over real persisted rows (machine v1, runtime_state
// 00030): the machine's configured initial state is stored, the machine
// version binding is captured, a duplicate init is an idempotent no-op,
// and invalid machines are rejected before any row is written.
package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// p622Service builds the runtime-state service with the machine surface
// wired for Initialize (WBS 6.30).
func p622Service(h *Harness, usePool bool) *service.RuntimeStateService {
	pool := h.App.DB().Raw()
	svc := service.NewRuntimeStateService(dbrepo.NewRuntimeStateRepository(pool)).
		WithMachines(dbrepo.NewStateMachineRepository(pool))
	if usePool {
		svc = svc.WithTxPool(pool)
	}
	return svc
}

// p622Count returns the persisted runtime_state row count for the project.
func p622Count(t *testing.T, h *Harness, project string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM runtime_state WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("count runtime_state: %v", err)
	}
	return n
}

// T6.30.1 — Runtime initialized to configured initial state: the stored
// row's current state is the machine's configured initial state (A), with
// the machine binding stored.
func TestP622InitializeToConfiguredInitialState(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-init",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	wantState := p620StateIDs(t, h, mid, "A")
	svc := p622Service(h, true)
	st, err := svc.Initialize(context.Background(), proj, mid)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if st.CurrentStateID != wantState {
		t.Fatalf("current state = %s, want the machine's initial state %s", st.CurrentStateID, wantState)
	}
	if st.StateMachineID != mid {
		t.Fatalf("machine id = %s, want %s", st.StateMachineID, mid)
	}
	got, err := svc.Get(context.Background(), proj)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentStateID != wantState || got.Version != 1 {
		t.Fatalf("stored = (%s, %d), want (%s, 1)", got.CurrentStateID, got.Version, wantState)
	}
}

// T6.30.2 — Machine version captured: initializing from a v2 machine
// (created via the P6-17 edit) stores the binding with the machine's
// actual version, not a hardcoded 1.
func TestP622InitializeCapturesMachineVersion(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-v2init",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	// Create a v2 of the machine (the edit-creates-a-version rule, P6-17).
	v2id, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States:          []service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		Transitions:     []service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
	})
	if err != nil {
		t.Fatalf("edit to v2: %v", err)
	}
	wantState := p620StateIDs(t, h, v2id, "B") // v2's configured initial state is B
	svc := p622Service(h, true)
	st, err := svc.Initialize(context.Background(), proj, v2id)
	if err != nil {
		t.Fatalf("initialize v2: %v", err)
	}
	if st.StateMachineVersion != 2 {
		t.Fatalf("stored machine version = %d, want 2", st.StateMachineVersion)
	}
	if st.CurrentStateID != wantState {
		t.Fatalf("current state = %s, want the v2 machine's initial state %s", st.CurrentStateID, wantState)
	}
}

// T6.30.3 — Duplicate initialization is idempotent: a second init on an
// initialized project does not fail, does not bump the version, and does
// not touch the stored row (chosen semantics: no-op when already
// initialized).
func TestP622InitializeDuplicateIsIdempotent(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-dup",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	svc := p622Service(h, true)
	if _, err := svc.Initialize(context.Background(), proj, mid); err != nil {
		t.Fatalf("first initialize: %v", err)
	}
	before, err := svc.Get(context.Background(), proj)
	if err != nil {
		t.Fatalf("get before duplicate: %v", err)
	}
	after, err := svc.Initialize(context.Background(), proj, mid)
	if err != nil {
		t.Fatalf("duplicate initialize must be a no-op, got %v", err)
	}
	if p622Count(t, h, proj) != 1 {
		t.Fatalf("runtime_state rows = %d, want 1 (duplicate init must not write)", p622Count(t, h, proj))
	}
	got, err := svc.Get(context.Background(), proj)
	if err != nil {
		t.Fatalf("get after duplicate: %v", err)
	}
	if got.Version != before.Version || got.CurrentStateID != before.CurrentStateID {
		t.Fatalf("row changed after duplicate init: (%d, %s) -> (%d, %s)",
			before.Version, before.CurrentStateID, got.Version, got.CurrentStateID)
	}
	if after.Version != before.Version {
		t.Fatalf("duplicate init returned version %d, want the stored %d", after.Version, before.Version)
	}
}

// T6.30.4 — Invalid machine cannot initialize runtime: an unknown machine
// id is rejected with ErrNotFound and a machine with no configured
// initial state is rejected with ErrInvalidInput; in both cases no
// runtime_state row is written.
func TestP622InitializeRejectsInvalidMachine(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-inval",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}},
		nil, "A")
	// A machine row with no initial state (the seed helper always sets one;
	// clear it via direct SQL to reach the no-initial path).
	if _, err := h.DBExec(context.Background(),
		"UPDATE state_machines SET initial_state_id = NULL WHERE id = '"+mid+"'"); err != nil {
		t.Fatalf("clear initial state: %v", err)
	}
	svc := p622Service(h, true)
	// Unknown machine id.
	if _, err := svc.Initialize(context.Background(), proj, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown machine err = %v, want service.ErrNotFound", err)
	}
	if p622Count(t, h, proj) != 0 {
		t.Fatalf("unknown machine wrote a row, want none")
	}
	// Machine with no configured initial state.
	if _, err := svc.Initialize(context.Background(), proj, mid); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("no-initial-state err = %v, want service.ErrInvalidInput", err)
	}
	if p622Count(t, h, proj) != 0 {
		t.Fatalf("no-initial-state machine wrote a row, want none")
	}
	// After the rejections, a valid machine inits cleanly (the project was
	// never half-initialized).
	p622RestoreInitial(t, h, mid)
	if _, err := svc.Initialize(context.Background(), proj, mid); err != nil {
		t.Fatalf("valid initialize after rejections: %v", err)
	}
}

// p622RestoreInitial restores the machine's initial state (cleared by the
// T6.30.4 fixture) so the final valid init has a configured initial
// state.
func p622RestoreInitial(t *testing.T, h *Harness, mid string) {
	t.Helper()
	var aid string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'A'", &aid); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	if _, err := h.DBExec(context.Background(),
		"UPDATE state_machines SET initial_state_id = '"+aid+"' WHERE id = '"+mid+"'"); err != nil {
		t.Fatalf("restore initial state: %v", err)
	}
}
