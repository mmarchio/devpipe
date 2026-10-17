// P6-15 integration half: graph checks (reachability / terminal / zero-
// progress cycles) driven with REAL persisted machine, state, and
// transition rows (WBS 6.16 / 6.17 / 6.18). The check logic is pure
// (internal/state); here the inputs come from the persisted tables via the
// repositories.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/state"
)

// p615StateFixture seeds a persisted workflow-state row and returns the
// domain value.
func p615StateFixture(t *testing.T, h *Harness, machineID uuid.UUID, key string, kind state.StateKind, terminal, enabled bool) *state.WorkflowState {
	t.Helper()
	ctx := context.Background()
	enabledSQL := "true"
	if !enabled {
		enabledSQL = "false"
	}
	terminalSQL := "false"
	if terminal {
		terminalSQL = "true"
	}
	q := "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '" + machineID.String() + "', '" + key + "', '" + key + "', '', '" + string(kind) + "', " + terminalSQL + ", " + enabledSQL + ", now(), now())"
	if _, err := h.DBExec(ctx, q); err != nil {
		t.Fatalf("seed state %s: %v", key, err)
	}
	var sid string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID.String()+"' AND key = '"+key+"'", &sid); err != nil {
		t.Fatalf("read state id: %v", err)
	}
	id, _ := uuid.Parse(sid)
	return &state.WorkflowState{
		ID: id, StateMachineID: machineID, Key: key, Name: key,
		Kind: kind, Terminal: terminal, Enabled: enabled,
	}
}

// p615TransitionRows loads the persisted transitions of a machine as domain
// values.
func p615TransitionRows(t *testing.T, h *Harness, machineID uuid.UUID) []*state.StateTransition {
	t.Helper()
	recs, err := dbrepo.NewStateTransitionRepository(h.App.DB().Raw()).ListByMachine(context.Background(), machineID, 1)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	var trs []*state.StateTransition
	for _, r := range recs {
		trs = append(trs, &state.StateTransition{
			ID: r.ID, StateMachineID: r.MachineID,
			FromStateID: r.FromStateID, ToStateID: r.ToStateID,
			Outcome: r.Outcome, Priority: r.Priority, Enabled: r.Enabled,
		})
	}
	return trs
}

// p615StateRows loads the persisted states of a machine as domain values.
func p615StateRows(t *testing.T, h *Harness, machineID uuid.UUID) []*state.WorkflowState {
	t.Helper()
	recs, err := dbrepo.NewWorkflowStateRepository(h.App.DB().Raw()).ListByMachine(context.Background(), machineID, 1)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	var ss []*state.WorkflowState
	for _, r := range recs {
		ss = append(ss, &state.WorkflowState{
			ID: r.ID, StateMachineID: r.MachineID, Key: r.Key, Name: r.Name,
			PromptTemplate: r.PromptTemplate, Kind: state.StateKind(r.Kind),
			Terminal: r.Terminal, Enabled: r.Enabled,
		})
	}
	return ss
}

// p615DevMachineFixture seeds a fresh machine with the base dev-workflow
// states (START -> IMPLEMENT -> VALIDATE -> DONE) and the three enabled base
// transitions, returning (machineID, machine, state map by key).
func p615DevMachineFixture(t *testing.T, h *Harness, projectID, machineName string) (uuid.UUID, *state.Machine, map[string]*state.WorkflowState) {
	t.Helper()
	machineIDStr := p605Machine(t, h, projectID, machineName)
	mid, _ := uuid.Parse(machineIDStr)
	start := p615StateFixture(t, h, mid, "START", state.KindControl, false, true)
	impl := p615StateFixture(t, h, mid, "IMPLEMENT", state.KindAction, false, true)
	validate := p615StateFixture(t, h, mid, "VALIDATE", state.KindValidation, false, true)
	done := p615StateFixture(t, h, mid, "DONE", state.KindTerminal, true, true)
	p614TransitionFixture(t, h, mid, start.ID, impl.ID, "BEGIN", 10, true)
	p614TransitionFixture(t, h, mid, impl.ID, validate.ID, "COMPLETE", 10, true)
	p614TransitionFixture(t, h, mid, validate.ID, done.ID, "PASS", 10, true)
	m := &state.Machine{ID: mid, InitialStateID: start.ID}
	return mid, m, map[string]*state.WorkflowState{"START": start, "IMPLEMENT": impl, "VALIDATE": validate, "DONE": done}
}

// T6.16.x — reachability driven with real persisted rows: an orphan and a
// state reachable only through a disabled edge are both flagged.
func TestP615IntegrationReachability(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, m, st := p615DevMachineFixture(t, h, proj, "p615-reach")
	_ = st
	p615StateFixture(t, h, mid, "ORPHAN_STATE", state.KindAction, false, true)
	hidden := p615StateFixture(t, h, mid, "HIDDEN", state.KindAction, false, true)
	p614TransitionFixture(t, h, mid, st["START"].ID, hidden.ID, "GOTO_HIDDEN", 10, false)
	rep := state.CheckReachability(m, p615StateRows(t, h, mid), p615TransitionRows(t, h, mid))
	if rep.Valid {
		t.Fatal("expected orphan + disabled-edge reports, got valid")
	}
	got := map[string]bool{}
	for _, s := range rep.OrphanStates {
		got[s.Key] = true
	}
	if !got["ORPHAN_STATE"] || !got["HIDDEN"] || len(got) != 2 {
		t.Fatalf("orphan set = %v, want exactly {ORPHAN_STATE, HIDDEN}", got)
	}
}

// T6.17.x — terminal-state validation driven with real persisted rows: a
// terminal state with an outgoing enabled transition and a nonterminal
// dead-end are both reported.
func TestP615IntegrationTerminalStates(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, m, st := p615DevMachineFixture(t, h, proj, "p615-term")
	stuck := p615StateFixture(t, h, mid, "STUCK", state.KindAction, false, true)
	p614TransitionFixture(t, h, mid, st["START"].ID, stuck.ID, "SIDE", 10, true)
	// The terminal state transitions elsewhere.
	p614TransitionFixture(t, h, mid, st["DONE"].ID, st["IMPLEMENT"].ID, "REOPEN", 10, true)
	rep := state.CheckTerminalStates(m, p615StateRows(t, h, mid), p615TransitionRows(t, h, mid))
	if rep.Valid {
		t.Fatal("expected terminal/dead-end violations, got valid")
	}
	if len(rep.TerminalWithOutgoing) != 1 || rep.TerminalWithOutgoing[0].Key != "DONE" {
		t.Fatalf("terminal violations = %+v, want exactly DONE", rep.TerminalWithOutgoing)
	}
	if len(rep.DeadEndStates) != 1 || rep.DeadEndStates[0].Key != "STUCK" {
		t.Fatalf("dead-end violations = %+v, want exactly STUCK", rep.DeadEndStates)
	}
}

// T6.18.x — zero-progress cycle detection driven with real persisted rows:
// the persisted pure-control triangle is rejected and reported with its
// exact state set; the persisted execution loop is not reported.
func TestP615IntegrationZeroProgressCycles(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, m, st := p615DevMachineFixture(t, h, proj, "p615-cycle")
	ca := p615StateFixture(t, h, mid, "ZCA", state.KindControl, false, true)
	cb := p615StateFixture(t, h, mid, "ZCB", state.KindControl, false, true)
	cc := p615StateFixture(t, h, mid, "ZCC", state.KindControl, false, true)
	fix := p615StateFixture(t, h, mid, "FIX", state.KindAction, false, true)
	p614TransitionFixture(t, h, mid, ca.ID, cb.ID, "C1", 10, true)
	p614TransitionFixture(t, h, mid, cb.ID, cc.ID, "C2", 10, true)
	p614TransitionFixture(t, h, mid, cc.ID, ca.ID, "C3", 10, true)
	p614TransitionFixture(t, h, mid, st["IMPLEMENT"].ID, fix.ID, "FAIL", 10, true)
	p614TransitionFixture(t, h, mid, fix.ID, st["IMPLEMENT"].ID, "FIXED", 10, true)
	rep := state.DetectZeroProgressCycles(m, p615StateRows(t, h, mid), p615TransitionRows(t, h, mid))
	if rep.Valid {
		t.Fatal("persisted control cycle not rejected")
	}
	if len(rep.Cycles) != 1 {
		t.Fatalf("cycle report = %+v, want exactly 1 cycle", rep.Cycles)
	}
	got := map[string]bool{}
	for _, s := range rep.Cycles[0].States {
		got[s.Key] = true
	}
	if !got["ZCA"] || !got["ZCB"] || !got["ZCC"] || len(got) != 3 {
		t.Fatalf("reported states = %v, want exactly {ZCA ZCB ZCC}", got)
	}
}
