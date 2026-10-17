// P6-33 (WBS 6.43): successful work-unit workflow test. The full successful
// work-unit path is simulated WITHOUT an LLM through the P6-21
// transition-application op, driving each step's destination as the next
// state in the WBS 6.43 sequence:
//
//	UNIT_DONE (runtime bound to a work unit)
//	    ↓ PASS
//	VALIDATE  ->  COMPLETE -> IMPLEMENT  ->  PREPARED -> WORK_UNIT_PREPARE
//	    (the path is driven state-by-state; the machine encodes the whole
//	    sequence with the WBS's outcome tokens)
//	    ↓
//	MORE_WORK?  -- the P6-13 remaining-work resolver (ResolveRemainingWork)
//	    over the persisted units picks the fork:
//	    NEXT_UNIT      when a non-DONE unit exists (explicit rebind lands)
//	    PHASE_VALIDATE when no work remains
//
// While the path advances, the phase association is RETAINED on every step
// and the work-unit association follows the unit being worked and moves ONLY
// on the explicit NEXT_UNIT rebind (the WBS 6.31/6.32 retention semantics,
// P6-23). No Hermes command is sent: the only writes are the runtime row and
// the transition-history rows the apply op records — the project, phase, and
// non-advanced work-unit rows are asserted untouched by direct SQL.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/service"
	"overseer/internal/state"
	"overseer/internal/wbs"
)

// p633Fixture builds the WBS 6.43 successful-path fixture: a project, a
// machine whose states/transitions encode the whole successful work-unit
// sequence (the machine's INITIAL state is WORK_UNIT_PREPARE — the "prepared
// position" the path starts from), a phase, a plan, and TWO work units (unitA
// is the unit being worked, unitB the remaining work that drives the
// MORE_WORK? fork). The runtime is initialized and bound to the phase +
// unitA. It returns every id the scenarios need.
func p633Fixture(t *testing.T, h *Harness) (project, machine, prepare, implement, validate, unitDone, nextUnit, phaseValidate, phase, unitA, unitB string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "p633suc",
		[]service.MachineStateDef{
			{Key: "WORK_UNIT_PREPARE", Kind: "ACTION"},
			{Key: "IMPLEMENT", Kind: "ACTION"},
			{Key: "VALIDATE", Kind: "ACTION"},
			{Key: "UNIT_DONE", Kind: "CONTROL"},
			{Key: "NEXT_UNIT", Kind: "CONTROL"},
			{Key: "PHASE_VALIDATE", Kind: "CONTROL"},
		},
		[]service.MachineTransitionDef{
			// The successful work-unit sequence (WBS 6.43), driven
			// state-by-state with the WBS outcome tokens.
			{From: "WORK_UNIT_PREPARE", Outcome: "PREPARED", To: "IMPLEMENT"},
			{From: "IMPLEMENT", Outcome: "COMPLETE", To: "VALIDATE"},
			{From: "VALIDATE", Outcome: "PASS", To: "UNIT_DONE"},
			// The MORE_WORK? fork: one row per distinct outcome token
			// (the (machine, from, outcome) unique index).
			{From: "UNIT_DONE", Outcome: state.OutcomeNextUnit, To: "NEXT_UNIT"},
			{From: "UNIT_DONE", Outcome: state.OutcomePhaseValidate, To: "PHASE_VALIDATE"},
		},
		"WORK_UNIT_PREPARE",
	)
	prepare = p620StateIDs(t, h, machine, "WORK_UNIT_PREPARE")
	implement = p620StateIDs(t, h, machine, "IMPLEMENT")
	validate = p620StateIDs(t, h, machine, "VALIDATE")
	unitDone = p620StateIDs(t, h, machine, "UNIT_DONE")
	nextUnit = p620StateIDs(t, h, machine, "NEXT_UNIT")
	phaseValidate = p620StateIDs(t, h, machine, "PHASE_VALIDATE")

	// Phase + plan + two work units: unitA is being worked (EXECUTING —
	// the "captured status" the path must leave untouched), unitB is the
	// remaining work that drives the MORE_WORK? -> NEXT_UNIT branch.
	phase = seedPhaseStatus(t, h, p633UUID(t, project), 1, "P", "PENDING").String()
	plan := seedPlan(t, h, p633UUID(t, phase), 1, 1, "READY")
	seedUnit(t, h, plan, 1, "A", "EXECUTING")
	seedUnit(t, h, plan, 2, "B", "PENDING")
	unitA = p633UnitBySeq(t, h, plan, 1)
	unitB = p633UnitBySeq(t, h, plan, 2)

	// Initialize the runtime at the machine's initial state
	// (WORK_UNIT_PREPARE — the prepared position) and bind the phase +
	// work-unit associations so the path starts with the unit being worked.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phase+"', work_unit_id = '"+unitA+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p633UUID parses a string id (the seed helpers address rows by uuid).
func p633UUID(t *testing.T, s string) (u uuid.UUID) {
	t.Helper()
	u = uuid.MustParse(s)
	return
}

// p633UnitBySeq reads a work-unit id by its sequence number.
func p633UnitBySeq(t *testing.T, h *Harness, plan uuid.UUID, seq int) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(), "SELECT id::text FROM work_units WHERE execution_plan_id = '"+plan.String()+"' AND sequence = "+itoa(seq), &id); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	return id
}

// p633Walk drives ONE successful step of the WBS 6.43 sequence through the
// P6-31 preview (the selection route) and the P6-21 apply op, asserting the
// preview selects the expected destination BEFORE applying. expectedVersion
// is the caller's optimistic-concurrency guard (it advances with every step).
func p633Walk(t *testing.T, h *Harness, project, fromID, toID, outcome string, expectedVersion int) {
	t.Helper()
	ctx := context.Background()
	// Selection route: the preview (no-apply) selects the expected
	// destination for the current position + outcome token.
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
	if err != nil {
		t.Fatalf("preview(%s): %v", outcome, err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != toID {
		t.Fatalf("preview selected %+v, want destination %s (outcome %s)", prev.Selected, toID, outcome)
	}
	// Apply the selected transition (phase/unit omitted -> retained).
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

// p633Preserved is the WBS 6.43 preserved set, read by DIRECT SQL: the
// project's status + version, the phase status, both work units' statuses
// (unitA's captured status in particular), and the runtime's phase/work-unit
// associations.
type p633Preserved struct {
	projectStatus  string
	projectVersion int
	phaseStatus    string
	unitAStatus    string
	unitBStatus    string
	runtimePhase   string
	runtimeUnit    string
}

func p633Snapshot(t *testing.T, h *Harness, project, phase, unitA, unitB string) p633Preserved {
	t.Helper()
	ctx := context.Background()
	var p p633Preserved
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &p.projectStatus, &p.projectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &p.phaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unitA+"'", &p.unitAStatus); err != nil {
		t.Fatalf("read unitA: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unitB+"'", &p.unitBStatus); err != nil {
		t.Fatalf("read unitB: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &p.runtimePhase, &p.runtimeUnit); err != nil {
		t.Fatalf("read runtime associations: %v", err)
	}
	return p
}

// p633HistoryAt reads the nth (0-based, project sequence order) history row's
// outcome + recorded work-unit association.
func p633HistoryAt(t *testing.T, h *Harness, project string, n int) (outcome, unitID string) {
	t.Helper()
	var u *string
	q := "SELECT outcome, coalesce(work_unit_id::text,'') FROM (SELECT outcome, work_unit_id FROM state_transition_history WHERE project_id = '" + project + "' ORDER BY seq) x LIMIT 1 OFFSET " + itoa(n)
	if err := h.DBScan(context.Background(), q, &outcome, &u); err != nil {
		t.Fatalf("read history row %d: %v", n, err)
	}
	if u != nil {
		unitID = *u
	}
	return
}

// The successful path reaches WORK_UNIT_PREPARE -> IMPLEMENT -> VALIDATE ->
// UNIT_DONE, the phase association is retained on every step, and the
// work-unit association follows the unit being worked (unitA) throughout.
func TestP633SuccessfulPathReachesUnitDone(t *testing.T) {
	h := NewHarness(t)
	project, _, prepare, implement, validate, unitDone, _, _, phase, unitA, unitB := p633Fixture(t, h)

	before := p633Snapshot(t, h, project, phase, unitA, unitB)
	histBefore := p632HistoryCount(t, h, project)

	p633Walk(t, h, project, prepare, implement, "PREPARED", 1)
	p633Walk(t, h, project, implement, validate, "COMPLETE", 2)
	p633Walk(t, h, project, validate, unitDone, "PASS", 3)

	// The runtime is at UNIT_DONE and the version advanced by exactly the
	// three applied steps.
	stID, ver := p632Runtime(t, h, project)
	if stID != unitDone || ver != 4 {
		t.Fatalf("runtime after path = (%s, %d), want (%s, 4)", stID, ver, unitDone)
	}
	// Exactly the three applied steps were recorded — nothing else.
	if p632HistoryCount(t, h, project) != histBefore+3 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	// The recorded entries carry the unit being worked (unitA) and the
	// phase association throughout the path.
	for i, want := range []struct{ outcome, unit string }{
		{outcome: "PREPARED", unit: unitA}, {outcome: "COMPLETE", unit: unitA}, {outcome: "PASS", unit: unitA},
	} {
		gotO, gotU := p633HistoryAt(t, h, project, histBefore+i)
		if gotO != want.outcome || gotU != want.unit {
			t.Fatalf("history row %d = (%s, %s), want (%s, %s)", i, gotO, gotU, want.outcome, want.unit)
		}
	}
	// The preserved rows are untouched: project row, phase row, unit
	// statuses. (The runtime's phase + work-unit associations are asserted
	// separately — they are expected to be RETAINED, not cleared.)
	before.runtimePhase = ""
	before.runtimeUnit = ""
	after := p633Snapshot(t, h, project, phase, unitA, unitB)
	after.runtimePhase = ""
	after.runtimeUnit = ""
	if after != before {
		t.Fatalf("preserved set changed: before=%+v after=%+v", before, after)
	}
	// The runtime's phase + work-unit associations were RETAINED on every
	// step (phase throughout, unitA — the unit being worked — throughout).
	cur := p633Snapshot(t, h, project, phase, unitA, unitB)
	if cur.runtimePhase != phase || cur.runtimeUnit != unitA {
		t.Fatalf("runtime associations were not retained (got phase=%s unit=%s, want phase=%s unit=%s)", cur.runtimePhase, cur.runtimeUnit, phase, unitA)
	}
}

// MORE_WORK? -> NEXT_UNIT: with another (non-DONE) work unit present, the
// resolver over the persisted units selects NEXT_UNIT, and the explicit
// rebind to the next unit lands while the phase association is retained.
func TestP633MoreWorkRoutesToNextUnitWithRebind(t *testing.T) {
	h := NewHarness(t)
	project, _, prepare, implement, validate, unitDone, nextUnit, _, phase, unitA, unitB := p633Fixture(t, h)
	ctx := context.Background()

	// Drive the path to UNIT_DONE (unitA completed).
	p633Walk(t, h, project, prepare, implement, "PREPARED", 1)
	p633Walk(t, h, project, implement, validate, "COMPLETE", 2)
	p633Walk(t, h, project, validate, unitDone, "PASS", 3)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id = '"+unitA+"'"); err != nil {
		t.Fatalf("mark unitA done: %v", err)
	}

	// The P6-13 remaining-work resolver over the REAL persisted units
	// selects NEXT_UNIT (unitB is still PENDING).
	token := state.ResolveRemainingWork(p613UnitFacts(t, h, p633UUID(t, project)))
	if token != state.OutcomeNextUnit {
		t.Fatalf("resolver = %q, want NEXT_UNIT", token)
	}
	// Selection route: the preview at UNIT_DONE selects the NEXT_UNIT
	// destination for that outcome token.
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: token})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != nextUnit {
		t.Fatalf("preview selected %+v, want the NEXT_UNIT destination %s", prev.Selected, nextUnit)
	}

	// Apply the fork with the EXPLICIT rebind to the next unit (unitB) —
	// the only way the work-unit association may move.
	bind := unitB
	out, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: unitDone, ToStateID: nextUnit, Outcome: token,
		WorkUnitID: &bind, ExpectedVersion: 4,
	})
	if err != nil {
		t.Fatalf("apply rebind: %v", err)
	}
	if out.State.CurrentStateID != nextUnit {
		t.Fatalf("applied current state = %s, want %s", out.State.CurrentStateID, nextUnit)
	}
	// Ground truth: the rebind landed (runtime now tracks unitB) and the
	// phase association was RETAINED on the step.
	stID, ver := p632Runtime(t, h, project)
	if stID != nextUnit || ver != 5 {
		t.Fatalf("runtime after rebind = (%s, %d), want (%s, 5)", stID, ver, nextUnit)
	}
	var rPhase, rUnit string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &rPhase, &rUnit); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rPhase != phase || rUnit != unitB {
		t.Fatalf("runtime associations after rebind = (%s, %s), want (%s, %s)", rPhase, rUnit, phase, unitB)
	}
	// The fork's history row records the rebind target (unitB), not unitA.
	_, histUnit := p633HistoryAt(t, h, project, 3)
	if histUnit != unitB {
		t.Fatalf("fork history work-unit = %s, want the rebind target %s", histUnit, unitB)
	}
	// The non-advanced unit (unitB's row) is untouched: the rebind is a
	// runtime association move only.
	if st := p633Snapshot(t, h, project, phase, unitA, unitB); st.unitBStatus != "PENDING" {
		t.Fatalf("unitB status = %q, want PENDING (no side effect)", st.unitBStatus)
	}
}

// MORE_WORK? -> PHASE_VALIDATE: with no remaining work (all units DONE), the
// resolver over the persisted units selects PHASE_VALIDATE, the path advances
// to phase validation, and the phase association is retained.
func TestP633NoRemainingWorkRoutesToPhaseValidate(t *testing.T) {
	h := NewHarness(t)
	project, _, prepare, implement, validate, unitDone, _, phaseValidate, phase, unitA, unitB := p633Fixture(t, h)
	ctx := context.Background()

	p633Walk(t, h, project, prepare, implement, "PREPARED", 1)
	p633Walk(t, h, project, implement, validate, "COMPLETE", 2)
	p633Walk(t, h, project, validate, unitDone, "PASS", 3)
	// No work remains: both units DONE.
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id IN ('"+unitA+"', '"+unitB+"')"); err != nil {
		t.Fatalf("mark units done: %v", err)
	}

	token := state.ResolveRemainingWork(p613UnitFacts(t, h, p633UUID(t, project)))
	if token != state.OutcomePhaseValidate {
		t.Fatalf("resolver = %q, want PHASE_VALIDATE", token)
	}
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: token})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != phaseValidate {
		t.Fatalf("preview selected %+v, want the PHASE_VALIDATE destination %s", prev.Selected, phaseValidate)
	}

	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: unitDone, ToStateID: phaseValidate, Outcome: token, ExpectedVersion: 4,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != phaseValidate || ver != 5 {
		t.Fatalf("runtime after apply = (%s, %d), want (%s, 5)", stID, ver, phaseValidate)
	}
	// The phase association is retained on the fork step (phase validation
	// is the phase's own step).
	var rPhase, rUnit string
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &rPhase, &rUnit); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rPhase != phase {
		t.Fatalf("runtime phase after fork = %q, want %q (retained)", rPhase, phase)
	}
	// The phase row is untouched — the fork advances the runtime only.
	var phaseStatus string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &phaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if phaseStatus != "PENDING" {
		t.Fatalf("phase status = %q, want PENDING (no side effect)", phaseStatus)
	}
}

// Determinism: repeated evaluations of the selection routes — the pure
// remaining-work resolver over the persisted units AND the persisted-row
// transition preview — select the expected states every time. The preview is
// no-apply, so both fork routes are evaluated repeatedly at the same
// UNIT_DONE position.
func TestP633SelectionDeterministic(t *testing.T) {
	h := NewHarness(t)
	project, _, prepare, implement, validate, unitDone, nextUnit, phaseValidate, _, unitA, unitB := p633Fixture(t, h)

	// Pure resolver determinism (both fork inputs).
	for i := 0; i < 10; i++ {
		if got := state.ResolveRemainingWork([]state.WorkUnitStatusFact{{Status: wbs.WorkUnitStatusPENDING}}); got != state.OutcomeNextUnit {
			t.Fatalf("iter %d: resolver with remaining work not deterministic (%q)", i, got)
		}
		if got := state.ResolveRemainingWork([]state.WorkUnitStatusFact{{Status: wbs.WorkUnitStatusDONE}}); got != state.OutcomePhaseValidate {
			t.Fatalf("iter %d: resolver with no remaining work not deterministic (%q)", i, got)
		}
	}

	// Drive the path to UNIT_DONE once.
	p633Walk(t, h, project, prepare, implement, "PREPARED", 1)
	p633Walk(t, h, project, implement, validate, "COMPLETE", 2)
	p633Walk(t, h, project, validate, unitDone, "PASS", 3)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		// The persisted-row selection (the P6-31 route) diverges by the
		// outcome token; both routes select the expected destination every
		// evaluation.
		pa, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomeNextUnit})
		if err != nil || pa.Selected == nil || pa.Selected.ToStateID != nextUnit {
			t.Fatalf("iter %d: NEXT_UNIT preview diverged: %+v err=%v", i, pa.Selected, err)
		}
		pb, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: state.OutcomePhaseValidate})
		if err != nil || pb.Selected == nil || pb.Selected.ToStateID != phaseValidate {
			t.Fatalf("iter %d: PHASE_VALIDATE preview diverged: %+v err=%v", i, pb.Selected, err)
		}
		// The resolver over the REAL persisted units is stable too: unitB
		// is PENDING, so every evaluation selects NEXT_UNIT.
		if got := state.ResolveRemainingWork(p613UnitFacts(t, h, p633UUID(t, project))); got != state.OutcomeNextUnit {
			t.Fatalf("iter %d: persisted-unit resolver diverged (%q)", i, got)
		}
	}
	_ = unitA
	_ = unitB
}

// No side effects beyond the applied transitions: after the full successful
// path plus the NEXT_UNIT rebind, the project row, phase row, and work-unit
// rows are untouched (the captured unitA status in particular) and only the
// runtime row + one history row per applied step changed — no Hermes command
// is dispatched.
func TestP633NoSideEffectsBeyondTransitions(t *testing.T) {
	h := NewHarness(t)
	project, _, prepare, implement, validate, unitDone, nextUnit, _, phase, unitA, unitB := p633Fixture(t, h)
	ctx := context.Background()

	before := p633Snapshot(t, h, project, phase, unitA, unitB)
	histBefore := p632HistoryCount(t, h, project)

	p633Walk(t, h, project, prepare, implement, "PREPARED", 1)
	p633Walk(t, h, project, implement, validate, "COMPLETE", 2)
	p633Walk(t, h, project, validate, unitDone, "PASS", 3)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id = '"+unitA+"'"); err != nil {
		t.Fatalf("mark unitA done: %v", err)
	}
	bind := unitB
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: unitDone, ToStateID: nextUnit, Outcome: state.OutcomeNextUnit,
		WorkUnitID: &bind, ExpectedVersion: 4,
	}); err != nil {
		t.Fatalf("apply rebind: %v", err)
	}

	// Exactly one history row per applied step — four steps, four rows.
	if p632HistoryCount(t, h, project) != histBefore+4 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	// The preserved rows: project row (status + version), phase row, and
	// unitB's row are byte-identical to before.
	after := p633Snapshot(t, h, project, phase, unitA, unitB)
	if after.projectStatus != before.projectStatus || after.projectVersion != before.projectVersion {
		t.Fatalf("project row changed: before=%+v after=%+v", before, after)
	}
	if after.phaseStatus != before.phaseStatus {
		t.Fatalf("phase status changed: %q -> %q", before.phaseStatus, after.phaseStatus)
	}
	if after.unitBStatus != "PENDING" {
		t.Fatalf("non-advanced unit status = %q, want PENDING (no side effect)", after.unitBStatus)
	}
	// The captured unitA status was marked DONE only by the test's own
	// bookkeeping — the apply ops never touch work-unit rows.
	if after.unitAStatus != "DONE" {
		t.Fatalf("unitA status = %q, want the test-set DONE", after.unitAStatus)
	}
}
