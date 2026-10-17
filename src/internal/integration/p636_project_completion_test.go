// P6-36 (WBS 6.46): project completion workflow test. The completion
// path is simulated WITHOUT an LLM through the P6-21
// transition-application op, driving each step's destination as the next
// state in the WBS 6.46 sequence:
//
//	(last work unit DONE)
//	PHASE_VALIDATE  ->  PASS  ->  PHASE_DONE
//	PHASE_DONE      ->  EVALUATE  ->  MORE_PHASES
//
// MORE_PHASES?  -- the P6-13 remaining-phase resolver (ResolveRemainingPhases)
// over the persisted phases picks the fork. With NO remaining phases (all
// DONE) it selects the NO outcome token (PROJECT_DONE), and the selection
// route (P6-31 preview) routes to the PROJECT_DONE state.
//
// PROJECT_DONE is TERMINAL: the machine defines no outgoing transitions
// from it (terminal flag set, zero outgoing rows) — a further normal
// transition from the position finds nothing to select (rejected at the
// selection route). The project's data + history are fully retained: the
// only writes are the runtime row and the transition-history rows the
// apply op records (the phase row's DONE status is test bookkeeping for
// the no-phases-remain precondition, not the machine). No Hermes command
// is sent.
package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/service"
	"overseer/internal/state"
)

// p636Fixture builds the WBS 6.46 project-completion fixture: a project,
// a machine whose states/transitions encode the completion sequence (the
// machine's INITIAL state is PHASE_VALIDATE — the "prepared position" the
// path starts from; the machine's TERMINAL PROJECT_DONE state has the
// terminal flag set and NO outgoing transitions), a single phase with a
// plan + one DONE work unit (the "last work unit DONE" precondition), and
// the runtime initialized and bound to the phase + unit.
func p636Fixture(t *testing.T, h *Harness) (project, machine, phaseValidate, phaseDone, morePhases, phasePlan, projectDone, phaseID, unitID string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "p636done",
		[]service.MachineStateDef{
			{Key: "PHASE_VALIDATE", Kind: "CONTROL"},
			{Key: "PHASE_DONE", Kind: "CONTROL"},
			{Key: "MORE_PHASES", Kind: "DECISION"},
			{Key: "PHASE_PLAN", Kind: "ACTION"},
			{Key: "PROJECT_DONE", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{
			// The completion sequence (WBS 6.46), driven
			// state-by-state with the WBS outcome tokens.
			{From: "PHASE_VALIDATE", Outcome: "PASS", To: "PHASE_DONE"},
			{From: "PHASE_DONE", Outcome: "EVALUATE", To: "MORE_PHASES"},
			// The MORE_PHASES? fork: one row per distinct outcome
			// token. PROJECT_DONE defines NO outgoing transitions —
			// it is terminal.
			{From: "MORE_PHASES", Outcome: state.OutcomePlanNextPhase, To: "PHASE_PLAN"},
			{From: "MORE_PHASES", Outcome: state.OutcomeProjectDone, To: "PROJECT_DONE"},
		},
		"PHASE_VALIDATE",
	)
	// The terminal BOOLEAN flag marks PROJECT_DONE (the seed helper
	// leaves the flag false for all states — set it explicitly).
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET terminal = true WHERE machine_id = '"+machine+"' AND key = 'PROJECT_DONE'"); err != nil {
		t.Fatalf("flag terminal: %v", err)
	}
	phaseValidate = p620StateIDs(t, h, machine, "PHASE_VALIDATE")
	phaseDone = p620StateIDs(t, h, machine, "PHASE_DONE")
	morePhases = p620StateIDs(t, h, machine, "MORE_PHASES")
	phasePlan = p620StateIDs(t, h, machine, "PHASE_PLAN")
	projectDone = p620StateIDs(t, h, machine, "PROJECT_DONE")

	// The single phase: a plan + one DONE work unit (the last work unit
	// of the completed phase).
	pu := seedPhaseStatus(t, h, uuid.MustParse(project), 1, "Only", "PENDING")
	phaseID = pu.String()
	plan := seedPlan(t, h, pu, 1, 1, "READY")
	seedUnit(t, h, plan, 1, "last", "DONE")
	unitID = p633UnitBySeq(t, h, plan, 1)

	// Initialize the runtime at the machine's initial state
	// (PHASE_VALIDATE — the prepared position) and bind the phase + its
	// (now DONE) work unit so the path starts at the phase boundary.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID+"', work_unit_id = '"+unitID+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p636MarkPhaseDone marks the single phase DONE — test bookkeeping for
// the "no phases remain" precondition, not a machine write.
func p636MarkPhaseDone(t *testing.T, h *Harness, phaseID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
}

// p636Walk drives ONE completion-path step through the P6-31 preview
// (the selection route) and the P6-21 apply op, asserting the preview
// selects the expected destination BEFORE applying. expectedVersion is
// the caller's optimistic-concurrency guard (it advances with every step).
func p636Walk(t *testing.T, h *Harness, project, fromID, toID, outcome string, expectedVersion int) {
	t.Helper()
	ctx := context.Background()
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
	if err != nil {
		t.Fatalf("preview(%s): %v", outcome, err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != toID {
		t.Fatalf("preview selected %+v, want destination %s (outcome %s)", prev.Selected, toID, outcome)
	}
	out, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: fromID, ToStateID: toID, Outcome: outcome, ExpectedVersion: expectedVersion,
	})
	if err != nil {
		t.Fatalf("apply(%s): %v", outcome, err)
	}
	if out.State.CurrentStateID != toID {
		t.Fatalf("applied current state = %s, want %s", out.State.CurrentStateID, toID)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != toID || ver != expectedVersion+1 {
		t.Fatalf("runtime after apply = (%s, %d), want (%s, %d)", stID, ver, toID, expectedVersion+1)
	}
}

// p636ToProjectDone drives the full completion path to PROJECT_DONE with
// no phases remaining (the single phase is DONE):
// PHASE_VALIDATE --PASS--> PHASE_DONE --EVALUATE--> MORE_PHASES
// --PROJECT_DONE (the resolver's NO outcome)--> PROJECT_DONE (v4).
func p636ToProjectDone(t *testing.T, h *Harness, project, phaseValidate, phaseDone, morePhases, projectDone string) {
	t.Helper()
	p636Walk(t, h, project, phaseValidate, phaseDone, "PASS", 1)
	p636Walk(t, h, project, phaseDone, morePhases, "EVALUATE", 2)
	p636Walk(t, h, project, morePhases, projectDone, state.OutcomeProjectDone, 3)
}

// T6.46.1 — with no remaining phases (all DONE), the MORE_PHASES? resolver
// selects the NO outcome and the selection route routes to PROJECT_DONE.
func TestP636ProjectDoneSelected(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, _, projectDone, phaseID, _ := p636Fixture(t, h)
	ctx := context.Background()
	p636MarkPhaseDone(t, h, phaseID)

	p636Walk(t, h, project, phaseValidate, phaseDone, "PASS", 1)
	p636Walk(t, h, project, phaseDone, morePhases, "EVALUATE", 2)

	// No remaining phase -> the resolver selects the NO outcome token.
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, uuid.MustParse(project))); got != state.OutcomeProjectDone {
		t.Fatalf("resolver = %q, want the NO outcome %q", got, state.OutcomeProjectDone)
	}
	// The selection route routes the NO token to PROJECT_DONE.
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomeProjectDone})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != projectDone {
		t.Fatalf("preview selected %+v, want the PROJECT_DONE destination %s", prev.Selected, projectDone)
	}
	// Apply the final step.
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: projectDone, Outcome: state.OutcomeProjectDone, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply completion: %v", err)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != projectDone || ver != 4 {
		t.Fatalf("runtime after completion = (%s, %d), want (%s, 4)", stID, ver, projectDone)
	}
}

// T6.46.2 — PROJECT_DONE is terminal: the machine defines no outgoing
// transitions from it (terminal flag set, state kind TERMINAL, zero
// outgoing rows — direct SQL).
func TestP636ProjectDoneTerminal(t *testing.T) {
	h := NewHarness(t)
	_, _, _, _, _, _, projectDone, _, _ := p636Fixture(t, h)
	ctx := context.Background()

	var terminal bool
	if err := h.DBScan(ctx, "SELECT terminal FROM workflow_states WHERE key = 'PROJECT_DONE' AND id = ('"+projectDone+"')::uuid", &terminal); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !terminal {
		t.Fatalf("PROJECT_DONE terminal flag = false, want true")
	}
	var kind string
	if err := h.DBScan(ctx, "SELECT kind FROM workflow_states WHERE id = ('"+projectDone+"')::uuid", &kind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kind != "TERMINAL" {
		t.Fatalf("PROJECT_DONE kind = %q, want TERMINAL", kind)
	}
	var outCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transitions WHERE from_state_id = ('"+projectDone+"')::uuid AND enabled", &outCount); err != nil {
		t.Fatalf("count outgoing: %v", err)
	}
	if outCount != 0 {
		t.Fatalf("PROJECT_DONE has %d outgoing enabled transitions, want 0 (terminal)", outCount)
	}
}

// T6.46.3 — a further normal transition from PROJECT_DONE is rejected:
// at the terminal position the selection route finds NO transition for
// any normal outcome token (nil selection, no error) — and nothing is
// written (runtime and history untouched).
func TestP636FurtherTransitionRejected(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, _, projectDone, phaseID, _ := p636Fixture(t, h)
	ctx := context.Background()
	p636MarkPhaseDone(t, h, phaseID)

	p636ToProjectDone(t, h, project, phaseValidate, phaseDone, morePhases, projectDone)
	if p632HistoryCount(t, h, project) != 3 {
		t.Fatalf("precondition: history = %d rows, want 3 (the applied steps)", p632HistoryCount(t, h, project))
	}

	// Every normal outcome token at PROJECT_DONE selects nothing — no
	// transition exists for the position.
	for _, outcome := range []string{"PASS", "EVALUATE", "CONTINUE", state.OutcomePlanNextPhase, state.OutcomeProjectDone} {
		prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
		if err != nil {
			t.Fatalf("preview(%s): %v", outcome, err)
		}
		if prev.Selected != nil {
			t.Fatalf("outcome %q selected a transition from the terminal position: %+v (want none)", outcome, prev.Selected)
		}
	}
	// Nothing was written: runtime still at PROJECT_DONE v4, history
	// unchanged.
	stID, ver := p632Runtime(t, h, project)
	if stID != projectDone || ver != 4 {
		t.Fatalf("runtime after rejected attempts = (%s, %d), want unchanged (%s, 4)", stID, ver, projectDone)
	}
	if p632HistoryCount(t, h, project) != 3 {
		t.Fatalf("history advanced after rejected attempts")
	}
}

// T6.46.4 — project data + history retained: after the full completion
// path the project row, the phase row, and the work-unit row are
// untouched (direct SQL), and the transition history is complete — one
// row per applied step, in seq order, with the retained phase + unit
// associations. No side effects beyond the applied transitions; no
// Hermes command is sent.
func TestP636DataHistoryRetained(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, _, projectDone, phaseID, unitID := p636Fixture(t, h)
	ctx := context.Background()

	var beforeProjectStatus, beforePhaseStatus, beforeUnitStatus string
	var beforeProjectVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &beforeProjectStatus, &beforeProjectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phaseID+"'", &beforePhaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = ('"+unitID+"')::uuid", &beforeUnitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	histBefore := p632HistoryCount(t, h, project)

	p636MarkPhaseDone(t, h, phaseID)
	p636ToProjectDone(t, h, project, phaseValidate, phaseDone, morePhases, projectDone)

	// Exactly one history row per applied step — three steps, three rows.
	if p632HistoryCount(t, h, project) != histBefore+3 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	// The history is complete: one seq-ordered row per applied step, with
	// the retained phase + unit on every row.
	want := []string{"PASS", "EVALUATE", state.OutcomeProjectDone}
	for i, o := range want {
		var outcome, hPhase, hUnit string
		if err := h.DBScan(ctx, "SELECT outcome, coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM state_transition_history WHERE project_id = '"+project+"' ORDER BY seq LIMIT 1 OFFSET "+itoaP636(i), &outcome, &hPhase, &hUnit); err != nil {
			t.Fatalf("read history row %d: %v", i+1, err)
		}
		if outcome != o {
			t.Fatalf("history row %d outcome = %q, want %q", i+1, outcome, o)
		}
		if hPhase != phaseID || hUnit != unitID {
			t.Fatalf("history row %d associations = (%s, %s), want retained (%s, %s)", i+1, hPhase, hUnit, phaseID, unitID)
		}
	}
	// The project row is untouched (the machine writes no project row).
	var afterProjectStatus, afterPhaseStatus, afterUnitStatus string
	var afterProjectVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &afterProjectStatus, &afterProjectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phaseID+"'", &afterPhaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = ('"+unitID+"')::uuid", &afterUnitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if afterProjectStatus != beforeProjectStatus || afterProjectVersion != beforeProjectVersion {
		t.Fatalf("project row changed: before=(%s,%d) after=(%s,%d)", beforeProjectStatus, beforeProjectVersion, afterProjectStatus, afterProjectVersion)
	}
	// The phase row keeps the test-set DONE (bookkeeping), the unit row
	// keeps its DONE status — the machine wrote no phase/unit row.
	if afterPhaseStatus != "DONE" {
		t.Fatalf("phase status = %q, want the test-set DONE (untouched by the machine)", afterPhaseStatus)
	}
	if afterUnitStatus != "DONE" {
		t.Fatalf("work unit status changed: %q -> %q (no side effect expected)", beforeUnitStatus, afterUnitStatus)
	}
}

// Determinism: repeated evaluations of the selection routes at the
// MORE_PHASES position — the pure remaining-phase resolver over the
// persisted phases AND the persisted-row transition preview (both fork
// routes) — select the expected states every time. The preview is
// no-apply.
func TestP636SelectionDeterministic(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, projectDone, phaseID, _ := p636Fixture(t, h)
	ctx := context.Background()

	// Pure resolver determinism: all-DONE facts -> the NO outcome.
	for i := 0; i < 10; i++ {
		if got := state.ResolveRemainingPhases([]state.PhaseStatusFact{{Status: phase.StatusDONE}}); got != state.OutcomeProjectDone {
			t.Fatalf("iter %d: resolver with no remaining phase not deterministic (%q)", i, got)
		}
	}

	p636MarkPhaseDone(t, h, phaseID)
	p636Walk(t, h, project, phaseValidate, phaseDone, "PASS", 1)
	p636Walk(t, h, project, phaseDone, morePhases, "EVALUATE", 2)

	for i := 0; i < 10; i++ {
		// Both fork routes select their expected destination every
		// evaluation at the same position.
		pa, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomePlanNextPhase})
		if err != nil || pa.Selected == nil || pa.Selected.ToStateID != phasePlan {
			t.Fatalf("iter %d: PLAN_NEXT_PHASE preview diverged: %+v err=%v", i, pa.Selected, err)
		}
		pb, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomeProjectDone})
		if err != nil || pb.Selected == nil || pb.Selected.ToStateID != projectDone {
			t.Fatalf("iter %d: PROJECT_DONE preview diverged: %+v err=%v", i, pb.Selected, err)
		}
		// The resolver over the REAL persisted phases is stable too:
		// the single phase is DONE, so every evaluation selects the NO
		// outcome.
		if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, uuid.MustParse(project))); got != state.OutcomeProjectDone {
			t.Fatalf("iter %d: persisted-phase resolver diverged (%q)", i, got)
		}
	}
}

func itoaP636(i int) string { return fmt.Sprint(i) }
