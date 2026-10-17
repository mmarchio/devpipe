// P6-15 (WBS 6.16 / 6.17 / 6.18): graph checks over a machine's persisted
// states + transitions — reachability, terminal-state validation, and
// zero-progress cycle detection. Pure functions over declared inputs — no
// DB in the check logic so the architecture guard stays green. Integration
// tests in internal/integration drive them with real persisted rows.
package state

import (
	"testing"

	"github.com/google/uuid"
)

// p615State builds a WorkflowState for test fixtures.
func p615State(machineID uuid.UUID, key string, kind StateKind, terminal, enabled bool) *WorkflowState {
	s, err := NewState(StateInput{
		StateMachineID: machineID,
		Key:            key,
		Name:           key,
		Kind:           kind,
		Terminal:       terminal,
	})
	if err != nil {
		panic(err)
	}
	s.Enabled = enabled
	return s
}

// p615Transition builds a StateTransition for test fixtures.
func p615Transition(machineID, from, to uuid.UUID, outcome string, priority int, enabled bool) *StateTransition {
	tr, err := NewTransition(TransitionInput{
		StateMachineID: machineID,
		FromStateID:    from,
		ToStateID:      to,
		Outcome:        outcome,
		Priority:       priority,
	})
	if err != nil {
		panic(err)
	}
	tr.Enabled = enabled
	return tr
}

// p615DevMachine returns a fully reachable machine:
//
//	START (control) -> IMPLEMENT (action) -> VALIDATE (validation) -> DONE (terminal)
//
// plus the given extra states/transitions.
func p615DevMachine(machineID uuid.UUID) (initID uuid.UUID, states []*WorkflowState, transitions []*StateTransition) {
	st := func(key string, kind StateKind, terminal bool) *WorkflowState {
		return p615State(machineID, key, kind, terminal, true)
	}
	start := st("START", KindControl, false)
	impl := st("IMPLEMENT", KindAction, false)
	validate := st("VALIDATE", KindValidation, false)
	done := st("DONE", KindTerminal, true)
	tra := p615Transition(machineID, start.ID, impl.ID, "BEGIN", 10, true)
	trb := p615Transition(machineID, impl.ID, validate.ID, "COMPLETE", 10, true)
	trc := p615Transition(machineID, validate.ID, done.ID, "PASS", 10, true)
	return start.ID,
		[]*WorkflowState{start, impl, validate, done},
		[]*StateTransition{tra, trb, trc}
}

func p615Machine(initID uuid.UUID) *Machine {
	m, err := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p615"})
	if err != nil {
		panic(err)
	}
	m.InitialStateID = initID
	return m
}

// p615Keys returns the sorted key list of a state slice.
func p615Keys(states []*WorkflowState) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, s.Key)
	}
	return out
}

// T6.16.1 — Fully reachable graph passes.
func TestT6161FullyReachablePasses(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	rep := CheckReachability(p615Machine(init), states, transitions)
	if !rep.Valid {
		t.Fatalf("fully reachable machine rejected: %+v", rep.OrphanStates)
	}
}

// T6.16.2 — Orphan state detected: an enabled state with no incoming path
// from the initial state is reported.
func TestT6162OrphanDetected(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	orphan := p615State(mid, "ORPHAN_STATE", KindAction, false, true)
	states = append(states, orphan)
	rep := CheckReachability(p615Machine(init), states, transitions)
	if rep.Valid {
		t.Fatal("orphan state not detected")
	}
	if len(rep.OrphanStates) != 1 || rep.OrphanStates[0].ID != orphan.ID {
		t.Fatalf("orphan report = %+v, want exactly ORPHAN_STATE", rep.OrphanStates)
	}
}

// T6.16.3 — Disabled-edge handling: disabled transitions are excluded from
// the reachability graph, so a state reachable ONLY through a disabled edge
// is flagged (the chosen policy, recorded per the task).
func TestT6163DisabledEdgeOrphanFlagged(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	hidden := p615State(mid, "HIDDEN", KindAction, false, true)
	states = append(states, hidden)
	// Only edge into HIDDEN is DISABLED.
	transitions = append(transitions, p615Transition(mid, init, hidden.ID, "GOTO_HIDDEN", 10, false))
	rep := CheckReachability(p615Machine(init), states, transitions)
	if rep.Valid {
		t.Fatal("state reachable only via disabled edge not flagged")
	}
	if len(rep.OrphanStates) != 1 || rep.OrphanStates[0].ID != hidden.ID {
		t.Fatalf("orphan report = %+v, want exactly HIDDEN", rep.OrphanStates)
	}
}

// T6.16.3 (policy consistency) — a DISABLED state is excluded from the
// reachability graph entirely and never reported as an orphan.
func TestT6163DisabledStateIgnored(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	disabled := p615State(mid, "DISABLED_OFF", KindAction, false, true)
	disabled.Enabled = false
	states = append(states, disabled)
	rep := CheckReachability(p615Machine(init), states, transitions)
	if !rep.Valid {
		t.Fatalf("disabled state reported as orphan: %+v", rep.OrphanStates)
	}
}

// T6.16.4 — Initial state considered reachable by definition (a machine
// whose ONLY enabled edge leaves the initial state still passes when every
// state is reachable; the initial state never appears in the orphan report).
func TestT6164InitialStateReachable(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	rep := CheckReachability(p615Machine(init), states, transitions)
	for _, o := range rep.OrphanStates {
		if o.ID == init {
			t.Fatal("initial state reported as orphan")
		}
	}
	if !rep.Valid {
		t.Fatalf("machine with reachable initial state rejected: %+v", rep.OrphanStates)
	}
}

// T6.17.1 — Terminal state without outgoing transition valid.
func TestT6171TerminalNoOutgoingValid(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	rep := CheckTerminalStates(p615Machine(init), states, transitions)
	if !rep.Valid {
		t.Fatalf("valid terminal topology rejected: %+v / %+v", rep.TerminalWithOutgoing, rep.DeadEndStates)
	}
}

// T6.17.2 — Terminal state WITH an outgoing enabled transition rejected.
func TestT6172TerminalWithOutgoingRejected(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	done := states[3] // DONE, terminal
	// The terminal state transitions elsewhere.
	transitions = append(transitions, p615Transition(mid, done.ID, states[1].ID, "REOPEN", 10, true))
	rep := CheckTerminalStates(p615Machine(init), states, transitions)
	if rep.Valid {
		t.Fatal("terminal state with outgoing transition not rejected")
	}
	if len(rep.TerminalWithOutgoing) != 1 || rep.TerminalWithOutgoing[0].ID != done.ID {
		t.Fatalf("report = %+v, want exactly DONE", rep.TerminalWithOutgoing)
	}
}

// T6.17.2 (consistency) — a terminal state with only a DISABLED outgoing
// transition remains valid (disabled edges excluded from the graph).
func TestT6172DisabledOutgoingAllowed(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	done := states[3]
	transitions = append(transitions, p615Transition(mid, done.ID, states[1].ID, "REOPEN", 10, false))
	rep := CheckTerminalStates(p615Machine(init), states, transitions)
	if !rep.Valid {
		t.Fatalf("disabled outgoing edge from terminal rejected: %+v", rep.TerminalWithOutgoing)
	}
}

// T6.17.3 — Nonterminal dead-end detected: a nonterminal enabled state with
// no outgoing enabled transition is reported.
func TestT6173NonterminalDeadEndDetected(t *testing.T) {
	mid := uuid.New()
	init, states, transitions := p615DevMachine(mid)
	stuck := p615State(mid, "STUCK", KindAction, false, true)
	states = append(states, stuck)
	// STUCK is reachable (edge from START) but has no outgoing edge.
	transitions = append(transitions, p615Transition(mid, init, stuck.ID, "SIDE", 10, true))
	rep := CheckTerminalStates(p615Machine(init), states, transitions)
	if rep.Valid {
		t.Fatal("nonterminal dead-end not detected")
	}
	if len(rep.DeadEndStates) != 1 || rep.DeadEndStates[0].ID != stuck.ID {
		t.Fatalf("dead-end report = %+v, want exactly STUCK", rep.DeadEndStates)
	}
}

// p615ControlCycle builds a pure control cycle A -> B -> C -> A.
func p615ControlCycle(mid uuid.UUID) (*Machine, []*WorkflowState, []*StateTransition) {
	a := p615State(mid, "CA", KindControl, false, true)
	b := p615State(mid, "CB", KindControl, false, true)
	c := p615State(mid, "CC", KindControl, false, true)
	trs := []*StateTransition{
		p615Transition(mid, a.ID, b.ID, "X1", 10, true),
		p615Transition(mid, b.ID, c.ID, "X2", 10, true),
		p615Transition(mid, c.ID, a.ID, "X3", 10, true),
	}
	m, _ := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p615-cycle"})
	m.InitialStateID = a.ID
	return m, []*WorkflowState{a, b, c}, trs
}

// T6.18.1 — Pure control cycle A->B->C->A rejected as a zero-progress cycle.
func TestT6181PureControlCycleRejected(t *testing.T) {
	mid := uuid.New()
	m, states, trs := p615ControlCycle(mid)
	rep := DetectZeroProgressCycles(m, states, trs)
	if rep.Valid {
		t.Fatal("pure control cycle not rejected")
	}
	if len(rep.Cycles) != 1 {
		t.Fatalf("cycle report = %+v, want exactly 1 cycle", rep.Cycles)
	}
	keys := p615Keys(rep.Cycles[0].States)
	if len(keys) != 3 || keys[0] != "CA" || keys[1] != "CB" || keys[2] != "CC" {
		t.Fatalf("cycle states = %v, want [CA CB CC] sorted by key", keys)
	}
}

// T6.18.2 — Worker-execution loop IMPLEMENT -> VALIDATE -> FIX -> IMPLEMENT
// allowed: a cycle involving a non-CONTROL kind is an intentional execution
// loop, not a zero-progress control loop.
func TestT6182ExecutionLoopAllowed(t *testing.T) {
	mid := uuid.New()
	impl := p615State(mid, "IMPLEMENT", KindAction, false, true)
	validate := p615State(mid, "VALIDATE", KindValidation, false, true)
	fix := p615State(mid, "FIX", KindAction, false, true)
	m, _ := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p615-exec"})
	m.InitialStateID = impl.ID
	trs := []*StateTransition{
		p615Transition(mid, impl.ID, validate.ID, "COMPLETE", 10, true),
		p615Transition(mid, validate.ID, fix.ID, "FAIL", 10, true),
		p615Transition(mid, fix.ID, impl.ID, "FIXED", 10, true),
	}
	rep := DetectZeroProgressCycles(m, []*WorkflowState{impl, validate, fix}, trs)
	if !rep.Valid {
		t.Fatalf("execution loop rejected: %+v", rep.Cycles)
	}
}

// T6.18.3 — Continuation loop guarded by a context/progress condition
// allowed: a condition on a transition referencing a progress-bearing
// declared field breaks the zero-progress classification.
func TestT6183GuardedContinuationLoopAllowed(t *testing.T) {
	mid := uuid.New()
	a := p615State(mid, "LOOP_A", KindControl, false, true)
	b := p615State(mid, "LOOP_B", KindControl, false, true)
	m, _ := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p615-guarded"})
	m.InitialStateID = a.ID
	trs := []*StateTransition{
		p615Transition(mid, a.ID, b.ID, "CONTINUE", 10, true),
	}
	back := p615Transition(mid, b.ID, a.ID, "RECHECK", 10, true)
	back.HasCondition = true
	rep := DetectZeroProgressCycles(m, []*WorkflowState{a, b}, trs)
	if !rep.Valid {
		t.Fatalf("guarded continuation loop rejected: %+v", rep.Cycles)
	}
}

// T6.18.4 — The cycle report identifies the involved states: in a machine
// containing BOTH a control cycle and an execution loop, only the control
// cycle is reported and its state set is exact (the execution loop's states
// must not leak into the report).
func TestT6184ReportIdentifiesInvolvedStates(t *testing.T) {
	mid := uuid.New()
	ca, cb, cc := p615State(mid, "ZCA", KindControl, false, true),
		p615State(mid, "ZCB", KindControl, false, true),
		p615State(mid, "ZCC", KindControl, false, true)
	impl := p615State(mid, "ZIMPL", KindAction, false, true)
	fix := p615State(mid, "ZFIX", KindAction, false, true)
	m, _ := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p618-report"})
	m.InitialStateID = impl.ID
	trs := []*StateTransition{
		p615Transition(mid, impl.ID, ca.ID, "BEGIN", 10, true),
		p615Transition(mid, ca.ID, cb.ID, "C1", 10, true),
		p615Transition(mid, cb.ID, cc.ID, "C2", 10, true),
		p615Transition(mid, cc.ID, ca.ID, "C3", 10, true),
		p615Transition(mid, impl.ID, fix.ID, "FAIL", 10, true),
		p615Transition(mid, fix.ID, impl.ID, "FIXED", 10, true),
	}
	states := []*WorkflowState{impl, fix, ca, cb, cc}
	rep := DetectZeroProgressCycles(m, states, trs)
	if rep.Valid || len(rep.Cycles) != 1 {
		t.Fatalf("expected exactly the control cycle, got %+v", rep.Cycles)
	}
	keys := p615Keys(rep.Cycles[0].States)
	if len(keys) != 3 || keys[0] != "ZCA" || keys[1] != "ZCB" || keys[2] != "ZCC" {
		t.Fatalf("reported states = %v, want exactly [ZCA ZCB ZCC]", keys)
	}
}

// T6.18.1 (consistency) — a self-loop on a CONTROL state is a zero-progress
// cycle and is rejected.
func TestT6181SelfLoopControlRejected(t *testing.T) {
	mid := uuid.New()
	a := p615State(mid, "SOLO", KindControl, false, true)
	m, _ := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "p615-solo"})
	m.InitialStateID = a.ID
	trs := []*StateTransition{p615Transition(mid, a.ID, a.ID, "STAY", 10, true)}
	rep := DetectZeroProgressCycles(m, []*WorkflowState{a}, trs)
	if rep.Valid {
		t.Fatal("control self-loop not rejected")
	}
	if len(rep.Cycles) != 1 || len(rep.Cycles[0].States) != 1 || rep.Cycles[0].States[0].ID != a.ID {
		t.Fatalf("report = %+v, want one cycle containing SOLO", rep.Cycles)
	}
}
