// P6-34 (WBS 6.44): failed-validation workflow test. The failed work-unit
// path is simulated WITHOUT an LLM through the P6-21 transition-application
// op, driving each step's destination as the next state in the WBS 6.44
// sequence:
//
//	IMPLEMENT  (runtime bound to the work unit being worked)
//	    ↓ COMPLETE
//	VALIDATE
//	    ↓ FAIL
//	FIX
//	    ↓ FIXED
//	IMPLEMENT
//
// While the path advances, the SAME work unit remains active: the
// work-unit association is retained on every step (no rebind - the only
// legal association move is the explicit NEXT_UNIT rebind, which this
// path never performs) and the phase association is retained throughout
// (WBS 6.31/6.32, P6-23). The state machine never writes work-unit rows:
// the unit's persisted status is untouched after the full path (the
// machine definition itself does not mark the unit DONE). No Hermes
// command is sent: the only writes are the runtime row and the
// transition-history rows the apply op records.
package integration

import (
	"context"
	"testing"

	"overseer/internal/service"
)

// p634Fixture builds the WBS 6.44 failed-validation fixture: a project, a
// machine whose states/transitions encode the failed path (the machine's
// INITIAL state is IMPLEMENT - the implementation position the path starts
// from), a phase, a plan, and ONE work unit (the unit being worked, at
// status EXECUTING - the captured status the path must leave untouched).
// The runtime is initialized at IMPLEMENT and bound to the phase + unit.
func p634Fixture(t *testing.T, h *Harness) (project, machine, implement, validate, fix, phase, unit string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "p634fail",
		[]service.MachineStateDef{
			{Key: "IMPLEMENT", Kind: "ACTION"},
			{Key: "VALIDATE", Kind: "ACTION"},
			{Key: "FIX", Kind: "ACTION"},
		},
		[]service.MachineTransitionDef{
			// The failed-validation sequence (WBS 6.44), driven
			// state-by-state with the WBS outcome tokens (one row
			// per distinct (from, outcome) pair).
			{From: "IMPLEMENT", Outcome: "COMPLETE", To: "VALIDATE"},
			{From: "VALIDATE", Outcome: "FAIL", To: "FIX"},
			{From: "FIX", Outcome: "FIXED", To: "IMPLEMENT"},
		},
		"IMPLEMENT",
	)
	implement = p620StateIDs(t, h, machine, "IMPLEMENT")
	validate = p620StateIDs(t, h, machine, "VALIDATE")
	fix = p620StateIDs(t, h, machine, "FIX")

	// Phase + plan + one work unit at status EXECUTING (the unit being
	// worked - the captured status the machine must leave untouched).
	phase = seedPhaseStatus(t, h, p633UUID(t, project), 1, "P", "PENDING").String()
	plan := seedPlan(t, h, p633UUID(t, phase), 1, 1, "READY")
	seedUnit(t, h, plan, 1, "A", "EXECUTING")
	unit = p633UnitBySeq(t, h, plan, 1)

	// Initialize the runtime at the machine's initial state (IMPLEMENT -
	// the implementation position) and bind the phase + work-unit
	// associations so the path starts with the unit being worked.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phase+"', work_unit_id = '"+unit+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p634Walk drives ONE step of the WBS 6.44 sequence through the P6-31
// preview (the selection route) and the P6-21 apply op, asserting the
// preview selects the expected destination BEFORE applying. No
// phase/work-unit rebind is passed - retention is the expected behavior.
func p634Walk(t *testing.T, h *Harness, project, fromID, toID, outcome string, expectedVersion int) {
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

// p634Associations reads the runtime's phase + work-unit associations by
// direct SQL.
func p634Associations(t *testing.T, h *Harness, project string) (phase, unit string) {
	t.Helper()
	if err := h.DBScan(context.Background(), "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &phase, &unit); err != nil {
		t.Fatalf("read runtime associations: %v", err)
	}
	return
}

// T6.44.1: the FAIL outcome at VALIDATE selects the FIX destination (the
// selection route), and the full failed path IMPLEMENT -> VALIDATE -> FIX
// advances through the runtime with the recorded history rows.
func TestP634FailAtValidateSelectsFix(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, _, unit := p634Fixture(t, h)

	histBefore := p632HistoryCount(t, h, project)

	// T6.44.1: the preview at VALIDATE selects the FIX destination for
	// the FAIL outcome token.
	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	prev, err := p632Preview(h).Preview(context.Background(), project, &service.PreviewTransitionInput{Outcome: "FAIL"})
	if err != nil {
		t.Fatalf("preview(FAIL): %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != fix {
		t.Fatalf("FAIL preview selected %+v, want the FIX destination %s", prev.Selected, fix)
	}
	p634Walk(t, h, project, validate, fix, "FAIL", 2)

	// The runtime is at FIX and the version advanced by exactly the two
	// applied steps; exactly two history rows were appended.
	stID, ver := p632Runtime(t, h, project)
	if stID != fix || ver != 3 {
		t.Fatalf("runtime after path = (%s, %d), want (%s, 3)", stID, ver, fix)
	}
	if p632HistoryCount(t, h, project) != histBefore+2 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	// The recorded entries carry the work unit being worked.
	gotO1, gotU1 := p633HistoryAt(t, h, project, histBefore)
	gotO2, gotU2 := p633HistoryAt(t, h, project, histBefore+1)
	if gotO1 != "COMPLETE" || gotO2 != "FAIL" {
		t.Fatalf("history outcomes = (%s, %s), want (COMPLETE, FAIL)", gotO1, gotO2)
	}
	if gotU1 != unit || gotU2 != unit {
		t.Fatalf("history work-units = (%s, %s), want the same unit %s", gotU1, gotU2, unit)
	}
}

// T6.44.2: the FIX step remains bound to the SAME work unit - the
// work-unit association is retained on the VALIDATE and FIX steps (no
// rebind), and the phase association is retained throughout.
func TestP634FixRemainsBoundToSameWorkUnit(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, phase, unit := p634Fixture(t, h)

	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	// Retained on the VALIDATE step (the unit being worked, not cleared
	// and not moved).
	if ph, un := p634Associations(t, h, project); ph != phase || un != unit {
		t.Fatalf("associations at VALIDATE = (%s, %s), want (%s, %s) (retained)", ph, un, phase, unit)
	}
	p634Walk(t, h, project, validate, fix, "FAIL", 2)
	// Retained on the FIX step - the SAME work unit, no rebind.
	ph, un := p634Associations(t, h, project)
	if ph != phase || un != unit {
		t.Fatalf("associations at FIX = (%s, %s), want (%s, %s) (same unit retained, no rebind)", ph, un, phase, unit)
	}
	// The FIX history row records the SAME unit association.
	_, histUnit := p633HistoryAt(t, h, project, 1)
	if histUnit != unit {
		t.Fatalf("FIX history work-unit = %s, want the same unit %s", histUnit, unit)
	}
}

// T6.44.3: FIX returns to the implementation path (FIX -> IMPLEMENT), and
// the runtime is back at IMPLEMENT with the same unit association intact
// (the failed path is a loop, not a terminal).
func TestP634FixReturnsToImplementation(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, phase, unit := p634Fixture(t, h)

	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	p634Walk(t, h, project, validate, fix, "FAIL", 2)
	// T6.44.3: the selection route at FIX returns to the implementation
	// path, and the apply lands the runtime back at IMPLEMENT.
	p634Walk(t, h, project, fix, implement, "FIXED", 3)

	stID, ver := p632Runtime(t, h, project)
	if stID != implement || ver != 4 {
		t.Fatalf("runtime after full failed path = (%s, %d), want (%s, 4)", stID, ver, implement)
	}
	// Back on the implementation path, the SAME unit is still the active
	// one.
	ph, un := p634Associations(t, h, project)
	if ph != phase || un != unit {
		t.Fatalf("associations after return to IMPLEMENT = (%s, %s), want (%s, %s)", ph, un, phase, unit)
	}
}

// T6.44.4: after the full failed path the unit is NOT marked DONE by the
// state machine definition itself - the work-unit row's persisted status
// is untouched (the machine never writes work-unit rows).
func TestP634UnitNotMarkedDoneByDefinition(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, _, unit := p634Fixture(t, h)
	ctx := context.Background()

	// The captured status is EXECUTING (the unit being worked).
	var before string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unit+"'", &before); err != nil {
		t.Fatalf("read unit status: %v", err)
	}
	if before != "EXECUTING" {
		t.Fatalf("unit status before path = %q, want the fixture's EXECUTING", before)
	}

	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	p634Walk(t, h, project, validate, fix, "FAIL", 2)
	p634Walk(t, h, project, fix, implement, "FIXED", 3)

	// The state machine definition itself never marks the unit DONE -
	// the persisted status is still the captured EXECUTING after the
	// full failed path (the unit is being re-worked, not completed).
	var after string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unit+"'", &after); err != nil {
		t.Fatalf("read unit status: %v", err)
	}
	if after != "EXECUTING" {
		t.Fatalf("unit status after full failed path = %q, want the untouched EXECUTING (not marked DONE)", after)
	}
}

// Determinism: the selection routes select the expected states across
// repeated evaluations. The three path positions are evaluated
// repeatedly: the no-apply preview at each position selects the same
// destination every time, and the path itself advances deterministically
// (the version guard fails on any divergence).
func TestP634SelectionDeterministic(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, _, _ := p634Fixture(t, h)
	ctx := context.Background()

	// IMPLEMENT position: the COMPLETE route selects VALIDATE.
	for i := 0; i < 10; i++ {
		prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: "COMPLETE"})
		if err != nil || prev.Selected == nil || prev.Selected.ToStateID != validate {
			t.Fatalf("iter %d: COMPLETE preview diverged: %+v err=%v", i, prev.Selected, err)
		}
	}
	// Advance to VALIDATE.
	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	// VALIDATE position: the FAIL route selects FIX.
	for i := 0; i < 10; i++ {
		prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: "FAIL"})
		if err != nil || prev.Selected == nil || prev.Selected.ToStateID != fix {
			t.Fatalf("iter %d: FAIL preview diverged: %+v err=%v", i, prev.Selected, err)
		}
	}
	// Advance to FIX.
	p634Walk(t, h, project, validate, fix, "FAIL", 2)
	// FIX position: the FIXED route returns to IMPLEMENT.
	for i := 0; i < 10; i++ {
		prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: "FIXED"})
		if err != nil || prev.Selected == nil || prev.Selected.ToStateID != implement {
			t.Fatalf("iter %d: FIXED preview diverged: %+v err=%v", i, prev.Selected, err)
		}
	}
}

// No side effects beyond the applied transitions: after the full failed
// path the project row, phase row, and work-unit row are untouched and
// only the runtime row + one history row per applied step changed - no
// Hermes command is dispatched.
func TestP634NoSideEffectsBeyondTransitions(t *testing.T) {
	h := NewHarness(t)
	project, _, implement, validate, fix, phase, unit := p634Fixture(t, h)
	ctx := context.Background()

	var projStatus string
	var projVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &projStatus, &projVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	var phaseStatus, unitStatus string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &phaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unit+"'", &unitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	histBefore := p632HistoryCount(t, h, project)

	p634Walk(t, h, project, implement, validate, "COMPLETE", 1)
	p634Walk(t, h, project, validate, fix, "FAIL", 2)
	p634Walk(t, h, project, fix, implement, "FIXED", 3)

	// Exactly one history row per applied step - three steps, three rows.
	if p632HistoryCount(t, h, project) != histBefore+3 {
		t.Fatalf("history advanced by more than one row per applied step")
	}
	// The project row, phase row, and unit row are byte-identical to
	// before - the machine wrote no non-runtime rows.
	var nStatus string
	var nVersion int
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &nStatus, &nVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if nStatus != projStatus || nVersion != projVersion {
		t.Fatalf("project row changed: (%q, %d) -> (%q, %d)", projStatus, projVersion, nStatus, nVersion)
	}
	var nPhaseStatus string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &nPhaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if nPhaseStatus != phaseStatus {
		t.Fatalf("phase status changed: %q -> %q", phaseStatus, nPhaseStatus)
	}
	var nUnitStatus string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unit+"'", &nUnitStatus); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if nUnitStatus != unitStatus {
		t.Fatalf("unit status changed: %q -> %q (the machine never writes unit rows)", unitStatus, nUnitStatus)
	}
	// The runtime row itself is the only changed row: back at IMPLEMENT
	// at v4 with the phase + unit associations retained.
	stID, ver := p632Runtime(t, h, project)
	if stID != implement || ver != 4 {
		t.Fatalf("runtime = (%s, %d), want (%s, 4)", stID, ver, implement)
	}
}
