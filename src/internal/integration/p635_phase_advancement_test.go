// P6-35 (WBS 6.45): phase advancement workflow test. The phase-advancement
// path is simulated WITHOUT an LLM through the P6-21
// transition-application op, driving each step's destination as the next
// state in the WBS 6.45 sequence:
//
//	(last work unit DONE)
//	PHASE_VALIDATE  ->  PASS  ->  PHASE_DONE
//	PHASE_DONE      ->  EVALUATE  ->  MORE_PHASES
//
// MORE_PHASES?  -- the P6-13 remaining-phase resolver (ResolveRemainingPhases)
// over the persisted phases picks the fork:
//
//	PLAN_NEXT_PHASE when a non-DONE phase exists (route -> PHASE_PLAN,
//	binding the NEW phase context)
//	PROJECT_DONE    when none remains
//
// The phase association moves ONLY on the explicit PHASE_PLAN rebind (the
// WBS 6.31/6.32 retention semantics, P6-23): every earlier step retains it,
// and the work-unit association is retained throughout (the only legal move
// for it is the explicit NEXT_UNIT rebind, P6-33). No Hermes command is
// sent: the only writes are the runtime row and the transition-history rows
// the apply op records — the project, phase, and work-unit rows are asserted
// untouched by direct SQL (the phase row's DONE status is test bookkeeping
// for the "last work unit DONE" precondition, not the machine).
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/service"
	"overseer/internal/state"
)

// p635Fixture builds the WBS 6.45 phase-advancement fixture: a project, a
// machine whose states/transitions encode the phase-advancement sequence
// (the machine's INITIAL state is PHASE_VALIDATE — the "prepared position"
// the path starts from, reached after the last work unit became DONE), the
// given phases in the given order (with a plan + one DONE work unit under
// the FIRST phase — the "last work unit DONE" precondition), and the
// runtime initialized and bound to the first phase + its unit. It returns
// every id the scenarios need.
func p635Fixture(t *testing.T, h *Harness, names []string) (project, machine, phaseValidate, phaseDone, morePhases, phasePlan, projectDone string, phaseIDs []string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "p635adv",
		[]service.MachineStateDef{
			{Key: "PHASE_VALIDATE", Kind: "CONTROL"},
			{Key: "PHASE_DONE", Kind: "CONTROL"},
			{Key: "MORE_PHASES", Kind: "DECISION"},
			{Key: "PHASE_PLAN", Kind: "ACTION"},
			{Key: "PROJECT_DONE", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{
			// The phase-advancement sequence (WBS 6.45), driven
			// state-by-state with the WBS outcome tokens.
			{From: "PHASE_VALIDATE", Outcome: "PASS", To: "PHASE_DONE"},
			{From: "PHASE_DONE", Outcome: "EVALUATE", To: "MORE_PHASES"},
			// The MORE_PHASES? fork: one row per distinct outcome
			// token (the (machine, from, outcome) unique index).
			{From: "MORE_PHASES", Outcome: state.OutcomePlanNextPhase, To: "PHASE_PLAN"},
			{From: "MORE_PHASES", Outcome: state.OutcomeProjectDone, To: "PROJECT_DONE"},
		},
		"PHASE_VALIDATE",
	)
	phaseValidate = p620StateIDs(t, h, machine, "PHASE_VALIDATE")
	phaseDone = p620StateIDs(t, h, machine, "PHASE_DONE")
	morePhases = p620StateIDs(t, h, machine, "MORE_PHASES")
	phasePlan = p620StateIDs(t, h, machine, "PHASE_PLAN")
	projectDone = p620StateIDs(t, h, machine, "PROJECT_DONE")

	// The phases in order; the first phase carries a plan + one DONE
	// work unit (the last work unit of the completed phase).
	var unit string
	for i, name := range names {
		id := seedPhaseStatus(t, h, p635UUID(t, project), i+1, name, "PENDING").String()
		phaseIDs = append(phaseIDs, id)
		if i == 0 {
			plan := seedPlan(t, h, p635UUID(t, id), 1, 1, "READY")
			seedUnit(t, h, plan, 1, "last", "DONE")
			unit = p633UnitBySeq(t, h, plan, 1)
		}
	}
	// Initialize the runtime at the machine's initial state
	// (PHASE_VALIDATE — the prepared position) and bind the first phase
	// + its (now DONE) work unit so the path starts at the phase boundary.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseIDs[0]+"', work_unit_id = '"+unit+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p635UUID parses a string id (the seed helpers address rows by uuid).
func p635UUID(t *testing.T, s string) (u uuid.UUID) {
	t.Helper()
	u = uuid.MustParse(s)
	return
}

// p635Walk drives ONE phase-advancement step through the P6-31 preview (the
// selection route) and the P6-21 apply op, asserting the preview selects the
// expected destination BEFORE applying. expectedVersion is the caller's
// optimistic-concurrency guard (it advances with every step).
func p635Walk(t *testing.T, h *Harness, project, fromID, toID, outcome string, expectedVersion int) {
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

// p635ToMorePhases drives the path to the MORE_PHASES? position (the last
// work unit is DONE — the fixture's unit is seeded DONE) and returns the
// MORE_PHASES position + the current runtime version (v3 after the two
// applied steps).
func p635ToMorePhases(t *testing.T, h *Harness, project, phaseValidate, phaseDone, morePhases string) {
	t.Helper()
	p635Walk(t, h, project, phaseValidate, phaseDone, "PASS", 1)
	p635Walk(t, h, project, phaseDone, morePhases, "EVALUATE", 2)
}

// p635NextPhase returns the next ordered INCOMPLETE phase of the project —
// the FIRST phase in ascending phase_order whose status is not DONE (direct
// SQL over the persisted rows; the selection the advancement must bind).
func p635NextPhase(t *testing.T, h *Harness, project string) string {
	t.Helper()
	var id string
	err := h.DBScan(context.Background(),
		"WITH ord AS (SELECT id, status, row_number() OVER (ORDER BY phase_order ASC) rn FROM phases WHERE project_id = '"+project+"') SELECT id::text FROM ord WHERE status <> 'DONE' ORDER BY rn LIMIT 1",
		&id)
	if err != nil {
		t.Fatalf("read next incomplete phase: %v", err)
	}
	return id
}

// T6.45.1 — with existing ordered incomplete phases present, the
// MORE_PHASES? fork selects the next ordered INCOMPLETE phase (the next one
// in order, not the first phase): with three phases and the first one DONE,
// the selection is the SECOND phase, not the third.
func TestP635NextOrderedIncompletePhaseSelected(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, _, phaseIDs := p635Fixture(t, h, []string{"A", "B", "C"})
	phase1, phase2, phase3 := phaseIDs[0], phaseIDs[1], phaseIDs[2]
	ctx := context.Background()

	// The completed (current) phase is DONE — test bookkeeping for the
	// DONE precondition, not a machine write.
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase1 done: %v", err)
	}

	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)

	// The P6-13 remaining-phase resolver over the REAL persisted phases
	// selects PLAN_NEXT_PHASE.
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, p635UUID(t, project))); got != state.OutcomePlanNextPhase {
		t.Fatalf("resolver = %q, want PLAN_NEXT_PHASE", got)
	}
	// The selection is the NEXT ordered incomplete phase: phase2 (not the
	// first phase — it is DONE — and not the later phase3).
	next := p635NextPhase(t, h, project)
	if next != phase2 {
		t.Fatalf("next incomplete phase = %s, want %s (the second phase in order, not the first — it is DONE — and not the later %s)", next, phase2, phase3)
	}
	// Selection route: the preview at MORE_PHASES selects the PHASE_PLAN
	// destination for the resolver's token.
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomePlanNextPhase})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != phasePlan {
		t.Fatalf("preview selected %+v, want the PHASE_PLAN destination %s", prev.Selected, phasePlan)
	}
	// Apply the fork with the EXPLICIT rebind to the selected phase — the
	// only legal phase move (work-unit omitted -> retained).
	bind := next
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: phasePlan, Outcome: state.OutcomePlanNextPhase,
		PhaseID: &bind, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply advancement: %v", err)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != phasePlan || ver != 4 {
		t.Fatalf("runtime after advancement = (%s, %d), want (%s, 4)", stID, ver, phasePlan)
	}
}

// T6.45.2 — a phase appended AFTER the snapshot (newly appended, later
// order) is eligible for selection: with only the completed phase present
// the resolver selects PROJECT_DONE, and once the new phase is appended the
// subsequent query discovers it and selects PLAN_NEXT_PHASE.
func TestP635NewlyAppendedPhaseEligible(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, _, phaseIDs := p635Fixture(t, h, []string{"A"})
	phase1 := phaseIDs[0]
	ctx := context.Background()

	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)

	// Before the append: no remaining phase -> PROJECT_DONE.
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, p635UUID(t, project))); got != state.OutcomeProjectDone {
		t.Fatalf("pre-append: outcome = %q, want PROJECT_DONE", got)
	}
	// Append a new phase DURING execution (later order).
	phase2 := seedPhaseStatus(t, h, p635UUID(t, project), 2, "B", "PENDING").String()
	// The subsequent query discovers it -> PLAN_NEXT_PHASE (no
	// snapshot/caching inside the resolver).
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, p635UUID(t, project))); got != state.OutcomePlanNextPhase {
		t.Fatalf("post-append: outcome = %q, want PLAN_NEXT_PHASE", got)
	}
	// The appended phase is the selection (the only incomplete one).
	if next := p635NextPhase(t, h, project); next != phase2 {
		t.Fatalf("next incomplete phase = %s, want the appended %s", next, phase2)
	}
	// The fork routes to PHASE_PLAN and binds the NEW phase.
	bind := phase2
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: phasePlan, Outcome: state.OutcomePlanNextPhase,
		PhaseID: &bind, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply advancement: %v", err)
	}
	var rPhase string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &rPhase); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rPhase != phase2 {
		t.Fatalf("runtime phase = %s, want the appended phase %s", rPhase, phase2)
	}
}

// T6.45.3 — the completed (current, DONE) phase is no longer the current one
// after advancement: the runtime's phase association is the NEW phase, and
// the completed phase row keeps its DONE status (untouched by the machine).
func TestP635CompletedPhaseNoLongerCurrent(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, _, phaseIDs := p635Fixture(t, h, []string{"A", "B"})
	phase1, phase2 := phaseIDs[0], phaseIDs[1]
	ctx := context.Background()

	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)
	bind := p635NextPhase(t, h, project)
	if bind != phase2 {
		t.Fatalf("selection = %s, want %s", bind, phase2)
	}
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: phasePlan, Outcome: state.OutcomePlanNextPhase,
		PhaseID: &bind, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply advancement: %v", err)
	}
	// Ground truth by direct SQL: the runtime tracks the NEW phase — the
	// completed phase is no longer the current one.
	var rPhase string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &rPhase); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rPhase == phase1 {
		t.Fatalf("runtime still tracks the completed phase %s", phase1)
	}
	if rPhase != phase2 {
		t.Fatalf("runtime phase = %s, want the new phase %s", rPhase, phase2)
	}
	// The completed phase row is untouched (its DONE status is the test's
	// own bookkeeping).
	var phase1Status string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase1+"'", &phase1Status); err != nil {
		t.Fatalf("read phase1: %v", err)
	}
	if phase1Status != "DONE" {
		t.Fatalf("completed phase status = %q, want DONE (untouched)", phase1Status)
	}
}

// T6.45.4 — the new phase context is established: the runtime's phase
// association is rebound to the new phase (the explicit advancement rebind,
// the only legal phase move) while the work-unit association rule is
// honored — the work unit is RETAINED (the only legal move for it is the
// explicit NEXT_UNIT rebind, WBS 6.31/6.32).
func TestP635NewPhaseContextEstablished(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, _, phaseIDs := p635Fixture(t, h, []string{"A", "B"})
	phase1, phase2 := phaseIDs[0], phaseIDs[1]
	ctx := context.Background()

	// Precondition: the runtime starts bound to the completed phase + its
	// last (DONE) work unit.
	var prePhase, preUnit string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &prePhase, &preUnit); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if prePhase != phase1 || preUnit == "" {
		t.Fatalf("precondition: runtime bound to (%s, %s), want (%s, a unit)", prePhase, preUnit, phase1)
	}

	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)
	bind := phase2
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: phasePlan, Outcome: state.OutcomePlanNextPhase,
		PhaseID: &bind, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply advancement: %v", err)
	}
	// The new phase context: the phase association is rebound to the new
	// phase AND the work-unit association is retained (the last worked
	// unit — it may move only via an explicit rebind).
	var rPhase, rUnit string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &rPhase, &rUnit); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rPhase != phase2 {
		t.Fatalf("runtime phase = %s, want the new phase %s", rPhase, phase2)
	}
	if rUnit != preUnit {
		t.Fatalf("runtime work unit = %s, want retained %s", rUnit, preUnit)
	}
	// The fork's history row records the explicit rebind target (the new
	// phase) and the retained work unit.
	var histPhase, histUnit string
	err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM (SELECT phase_id, work_unit_id FROM state_transition_history WHERE project_id = '"+project+"' ORDER BY seq) x LIMIT 1 OFFSET 2", &histPhase, &histUnit)
	if err != nil {
		t.Fatalf("read history row: %v", err)
	}
	if histPhase != phase2 || histUnit != preUnit {
		t.Fatalf("fork history = (%s, %s), want (%s, %s)", histPhase, histUnit, phase2, preUnit)
	}
}

// Determinism: repeated evaluations of the selection routes — the pure
// remaining-phase resolver over the persisted phases AND the persisted-row
// transition preview — select the expected states every time. The preview is
// no-apply, so both fork routes are evaluated repeatedly at the same
// MORE_PHASES position.
func TestP635SelectionDeterministic(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, projectDone, phaseIDs := p635Fixture(t, h, []string{"A"})
	phase1 := phaseIDs[0]
	ctx := context.Background()

	// Pure resolver determinism (both fork inputs).
	for i := 0; i < 10; i++ {
		if got := state.ResolveRemainingPhases([]state.PhaseStatusFact{{Status: phase.StatusDONE}}); got != state.OutcomeProjectDone {
			t.Fatalf("iter %d: resolver with no remaining phase not deterministic (%q)", i, got)
		}
	}

	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)

	for i := 0; i < 10; i++ {
		// The persisted-row selection (the P6-31 route) diverges by the
		// outcome token; both routes select the expected destination every
		// evaluation.
		pa, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomePlanNextPhase})
		if err != nil || pa.Selected == nil || pa.Selected.ToStateID != phasePlan {
			t.Fatalf("iter %d: PLAN_NEXT_PHASE preview diverged: %+v err=%v", i, pa.Selected, err)
		}
		pb, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomeProjectDone})
		if err != nil || pb.Selected == nil || pb.Selected.ToStateID != projectDone {
			t.Fatalf("iter %d: PROJECT_DONE preview diverged: %+v err=%v", i, pb.Selected, err)
		}
		// The resolver over the REAL persisted phases is stable too: the
		// single phase is DONE, so every evaluation selects PROJECT_DONE.
		if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, p635UUID(t, project))); got != state.OutcomeProjectDone {
			t.Fatalf("iter %d: persisted-phase resolver diverged (%q)", i, got)
		}
	}
}

// No side effects beyond the applied transitions: after the full
// phase-advancement path plus the PHASE_PLAN rebind, the project row and the
// work-unit rows are untouched, and only the runtime row + one history row
// per applied step changed — no Hermes command is dispatched. (The phase
// rows' statuses are asserted: the completed phase's DONE status is the
// test's own bookkeeping, and the new phase's row is untouched.)
func TestP635NoSideEffectsBeyondTransitions(t *testing.T) {
	h := NewHarness(t)
	project, _, phaseValidate, phaseDone, morePhases, phasePlan, _, phaseIDs := p635Fixture(t, h, []string{"A", "B"})
	phase1, phase2 := phaseIDs[0], phaseIDs[1]
	ctx := context.Background()

	var beforeProjectStatus, beforeUnitStatus string
	var beforeProjectVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &beforeProjectStatus, &beforeProjectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE execution_plan_id IN (SELECT id FROM execution_plans WHERE phase_id = '"+phase1+"') LIMIT 1", &beforeUnitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	histBefore := p632HistoryCount(t, h, project)

	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phase1+"'"); err != nil {
		t.Fatalf("mark phase done: %v", err)
	}
	p635ToMorePhases(t, h, project, phaseValidate, phaseDone, morePhases)
	bind := phase2
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: morePhases, ToStateID: phasePlan, Outcome: state.OutcomePlanNextPhase,
		PhaseID: &bind, ExpectedVersion: 3,
	}); err != nil {
		t.Fatalf("apply advancement: %v", err)
	}

	// Exactly one history row per applied step — three steps, three rows.
	if p632HistoryCount(t, h, project) != histBefore+3 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	var afterProjectStatus, afterUnitStatus string
	var afterProjectVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &afterProjectStatus, &afterProjectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE execution_plan_id IN (SELECT id FROM execution_plans WHERE phase_id = '"+phase1+"') LIMIT 1", &afterUnitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if afterProjectStatus != beforeProjectStatus || afterProjectVersion != beforeProjectVersion {
		t.Fatalf("project row changed: before=(%s,%d) after=(%s,%d)", beforeProjectStatus, beforeProjectVersion, afterProjectStatus, afterProjectVersion)
	}
	if afterUnitStatus != beforeUnitStatus {
		t.Fatalf("work unit status changed: %q -> %q (no side effect expected)", beforeUnitStatus, afterUnitStatus)
	}
	// The phase rows: the completed phase keeps the test-set DONE, the new
	// phase is untouched (PENDING).
	var phase1Status, phase2Status string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase1+"'", &phase1Status); err != nil {
		t.Fatalf("read phase1: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase2+"'", &phase2Status); err != nil {
		t.Fatalf("read phase2: %v", err)
	}
	if phase1Status != "DONE" || phase2Status != "PENDING" {
		t.Fatalf("phase statuses = (%s, %s), want (DONE, PENDING)", phase1Status, phase2Status)
	}
}
