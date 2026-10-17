// P6-32 (WBS 6.42): context-reset workflow test. The INCOMPLETE outcome is
// routed by the P6-13 context-threshold resolver (ResolveContextThreshold)
// into one of two outcome tokens; the P6-31 preview evaluates the matching
// transition (no apply) and the P6-21 op applies it. This pins the two
// selection routes and the association retention for the final overseer:
//
//	Scenario A: IMPLEMENT, INCOMPLETE, context 50% / threshold 80%
//	    -> CONTINUE_CURRENT_SESSION
//	Scenario B: IMPLEMENT, INCOMPLETE, context 85% / threshold 80%
//	    -> RESET_SESSION
//
// while the project, phase, work unit, and captured status association are
// preserved. No Hermes command is sent (no execution layer is invoked) —
// only the selection routes and retention are exercised. The actual status
// payload lands in a later execution phase.
package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
	"overseer/internal/state"
)

// p632Fixture builds the WBS 6.42 context-reset fixture: a project, a
// machine whose IMPLEMENT state carries the two context-threshold fork
// transitions (IMPLEMENT -> CONTINUE_CURRENT_SESSION and
// IMPLEMENT -> RESET_SESSION, one per distinct outcome token so the
// (machine, from, outcome) unique index is not violated), a phase + work
// unit, a work-unit status set to EXECUTING (the "captured status"), and a
// runtime initialized at IMPLEMENT with the phase + work unit bound. It
// returns every id the scenarios need.
func p632Fixture(t *testing.T, h *Harness) (project, machine, implement, contID, rstID, phase, unit string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "p632ctx",
		[]service.MachineStateDef{
			{Key: "IMPLEMENT", Kind: "ACTION"},
			{Key: "CONTINUE_CURRENT_SESSION", Kind: "CONTROL"},
			{Key: "RESET_SESSION", Kind: "CONTROL"},
		},
		[]service.MachineTransitionDef{
			{From: "IMPLEMENT", Outcome: state.OutcomeContinueCurrentSession, To: "CONTINUE_CURRENT_SESSION"},
			{From: "IMPLEMENT", Outcome: state.OutcomeResetSession, To: "RESET_SESSION"},
		},
		"IMPLEMENT",
	)
	implement = p620StateIDs(t, h, machine, "IMPLEMENT")
	contID = p620StateIDs(t, h, machine, "CONTINUE_CURRENT_SESSION")
	rstID = p620StateIDs(t, h, machine, "RESET_SESSION")

	// Phase + plan + work unit (the WBS 6.20 fixture shape). The work unit
	// is seeded at status EXECUTING — the "captured status" the WBS 6.42
	// preservation rule must leave untouched.
	if _, err := h.DBExec(ctx, "INSERT INTO phases (id, project_id, name, description, phase_order, status, created_at, updated_at) VALUES (gen_random_uuid(), '"+project+"', 'P', '', 1, 'PENDING', now(), now())"); err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM phases WHERE project_id = '"+project+"'", &phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES (gen_random_uuid(), '"+phase+"', 1, 1, 'READY')"); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	var planID string
	if err := h.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phase+"'", &planID); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO work_units (id, execution_plan_id, sequence, name, kind, status, objective, created_at) VALUES (gen_random_uuid(), '"+planID+"', 1, 'W', 'ATOMIC', 'EXECUTING', 'obj', now())"); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM work_units WHERE execution_plan_id = '"+planID+"'", &unit); err != nil {
		t.Fatalf("read work unit: %v", err)
	}

	// Initialize the runtime at the machine's initial state (IMPLEMENT) at
	// version 1, then bind the phase + work unit associations (the captured
	// status association) by direct SQL so the runtime row stays at v1.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phase+"', work_unit_id = '"+unit+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p632Preserved is the WBS 6.42 preserved set, read by DIRECT SQL (ground
// truth, not the service readback): the project's status + version (the
// apply op must not touch the project row), the phase status, the work unit
// status (the captured status), and the runtime's phase/work-unit
// associations.
type p632Preserved struct {
	projectStatus  string
	projectVersion int
	phaseStatus    string
	unitStatus     string
	runtimePhase   string
	runtimeUnit    string
}

func p632Snapshot(t *testing.T, h *Harness, project, phase, unit string) p632Preserved {
	t.Helper()
	ctx := context.Background()
	var p p632Preserved
	if err := h.DBScan(ctx, "SELECT status, version FROM projects WHERE id = '"+project+"'", &p.projectStatus, &p.projectVersion); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &p.phaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unit+"'", &p.unitStatus); err != nil {
		t.Fatalf("read work unit: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') FROM runtime_state WHERE project_id = '"+project+"'", &p.runtimePhase, &p.runtimeUnit); err != nil {
		t.Fatalf("read runtime associations: %v", err)
	}
	return p
}

// p632Runtime reads the persisted runtime current state + version.
func p632Runtime(t *testing.T, h *Harness, project string) (stateID string, version int) {
	t.Helper()
	if err := h.DBScan(context.Background(),
		"SELECT current_state_id::text, version FROM runtime_state WHERE project_id = '"+project+"'", &stateID, &version); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	return
}

// p632Preview builds the transition preview service over the harness pool
// (the P6-31 selection route).
func p632Preview(h *Harness) *service.TransitionPreviewService {
	pool := h.App.DB().Raw()
	return service.NewTransitionPreviewService(
		dbrepo.NewRuntimeStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
	).WithTxPool(pool)
}

// p632HistoryCount reads the transition-history row count for the project.
func p632HistoryCount(t *testing.T, h *Harness, project string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM state_transition_history WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	return n
}

// Scenario A (WBS 6.42): context 50% / threshold 80% deterministically
// selects CONTINUE_CURRENT_SESSION, and applying it preserves the project,
// phase, work unit, and captured status association.
func TestP632ScenarioABelowThresholdSelectsContinue(t *testing.T) {
	h := NewHarness(t)
	project, machine, implement, contID, _, phase, unit := p632Fixture(t, h)
	ctx := context.Background()

	// The P6-13 resolver maps (usage, threshold) -> outcome token.
	token, err := state.ResolveContextThreshold(50, 80)
	if err != nil {
		t.Fatalf("resolve threshold: %v", err)
	}
	if token != state.OutcomeContinueCurrentSession {
		t.Fatalf("resolver(50,80) = %q, want %q", token, state.OutcomeContinueCurrentSession)
	}

	// Preview (the P6-31 no-apply selection route) selects the
	// CONTINUE_CURRENT_SESSION destination.
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{
		Outcome: token, ContextUsage: 50, ContextThreshold: 80,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != contID {
		t.Fatalf("preview selection = %+v, want the CONTINUE_CURRENT_SESSION destination %s", prev.Selected, contID)
	}

	before := p632Snapshot(t, h, project, phase, unit)
	histBefore := p632HistoryCount(t, h, project)

	// Apply the selected transition (phase/unit omitted -> retained).
	out, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: implement, ToStateID: contID, Outcome: token, ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.State.CurrentStateID != contID {
		t.Fatalf("applied current state = %s, want %s", out.State.CurrentStateID, contID)
	}
	// Ground truth: the runtime moved to CONTINUE and the version bumped.
	stID, ver := p632Runtime(t, h, project)
	if stID != contID || ver != 2 {
		t.Fatalf("runtime after apply = (%s, %d), want (%s, 2)", stID, ver, contID)
	}
	if p632HistoryCount(t, h, project) != histBefore+1 {
		t.Fatalf("history row count did not advance by exactly one (no Hermes command / no extra write)")
	}

	// The preserved set is untouched.
	after := p632Snapshot(t, h, project, phase, unit)
	if after != before {
		t.Fatalf("preserved set changed: before=%+v after=%+v", before, after)
	}
	_ = machine
}

// Scenario B (WBS 6.42): context 85% / threshold 80% deterministically
// selects RESET_SESSION, and applying it preserves the same set.
func TestP632ScenarioBAtAboveThresholdSelectsReset(t *testing.T) {
	h := NewHarness(t)
	project, machine, implement, _, rstID, phase, unit := p632Fixture(t, h)
	ctx := context.Background()

	token, err := state.ResolveContextThreshold(85, 80)
	if err != nil {
		t.Fatalf("resolve threshold: %v", err)
	}
	if token != state.OutcomeResetSession {
		t.Fatalf("resolver(85,80) = %q, want %q", token, state.OutcomeResetSession)
	}

	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{
		Outcome: token, ContextUsage: 85, ContextThreshold: 80,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != rstID {
		t.Fatalf("preview selection = %+v, want the RESET_SESSION destination %s", prev.Selected, rstID)
	}

	before := p632Snapshot(t, h, project, phase, unit)
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: implement, ToStateID: rstID, Outcome: token, ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != rstID || ver != 2 {
		t.Fatalf("runtime after apply = (%s, %d), want (%s, 2)", stID, ver, rstID)
	}
	after := p632Snapshot(t, h, project, phase, unit)
	if after != before {
		t.Fatalf("preserved set changed: before=%+v after=%+v", before, after)
	}
	_ = machine
}

// The at-threshold boundary (80% / 80%): usage == threshold is "at or
// above", so the resolver selects RESET_SESSION (pins the boundary the WBS
// calls out).
func TestP632AtThresholdBoundarySelectsReset(t *testing.T) {
	token, err := state.ResolveContextThreshold(80, 80)
	if err != nil {
		t.Fatalf("resolve threshold: %v", err)
	}
	if token != state.OutcomeResetSession {
		t.Fatalf("resolver(80,80) = %q, want %q (at-threshold is a reset)", token, state.OutcomeResetSession)
	}
}

// Determinism: repeated evaluations of each route (the pure resolver and the
// persisted-row selection) select the same destination every time — no
// nondeterminism in the selection.
func TestP632SelectionDeterministic(t *testing.T) {
	for i := 0; i < 10; i++ {
		if tk, e := state.ResolveContextThreshold(50, 80); e != nil || tk != state.OutcomeContinueCurrentSession {
			t.Fatalf("iter %d: resolver(50,80) not deterministic", i)
		}
		if tk, e := state.ResolveContextThreshold(85, 80); e != nil || tk != state.OutcomeResetSession {
			t.Fatalf("iter %d: resolver(85,80) not deterministic", i)
		}
	}

	h := NewHarness(t)
	project, _, _, contID, rstID, _, _ := p632Fixture(t, h)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		pa, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{
			Outcome: state.OutcomeContinueCurrentSession, ContextUsage: 50, ContextThreshold: 80,
		})
		if err != nil || pa.Selected == nil || pa.Selected.ToStateID != contID {
			t.Fatalf("iter %d: CONTINUE preview diverged: %+v err=%v", i, pa.Selected, err)
		}
		// The preview is no-apply; the runtime is still at IMPLEMENT, so the
		// RESET route can be evaluated on the same position.
		pb, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{
			Outcome: state.OutcomeResetSession, ContextUsage: 85, ContextThreshold: 80,
		})
		if err != nil || pb.Selected == nil || pb.Selected.ToStateID != rstID {
			t.Fatalf("iter %d: RESET preview diverged: %+v err=%v", i, pb.Selected, err)
		}
	}
}

// No side effects beyond the applied transition: after each route, the
// project, phase, and work-unit rows are untouched (the captured status in
// particular) and only the runtime row + one history row change — no Hermes
// command is dispatched.
func TestP632NoSideEffectsBeyondTransition(t *testing.T) {
	for _, sc := range []struct {
		name      string
		usage     float64
		threshold float64
	}{{name: "A", usage: 50, threshold: 80}, {name: "B", usage: 85, threshold: 80}} {
		t.Run(sc.name, func(t *testing.T) {
			h := NewHarness(t)
			project, _, implement, contID, rstID, phase, unit := p632Fixture(t, h)
			ctx := context.Background()
			token, _ := state.ResolveContextThreshold(sc.usage, sc.threshold)
			want := contID
			if token == state.OutcomeResetSession {
				want = rstID
			}
			before := p632Snapshot(t, h, project, phase, unit)
			if _, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
				FromStateID: implement, ToStateID: want, Outcome: token, ExpectedVersion: 1,
			}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			after := p632Snapshot(t, h, project, phase, unit)
			if after != before {
				t.Fatalf("side effect: preserved set changed before=%+v after=%+v", before, after)
			}
			// The work-unit status (the captured status) is explicitly untouched.
			if after.unitStatus != "EXECUTING" {
				t.Fatalf("captured status = %q, want EXECUTING (no command dispatched)", after.unitStatus)
			}
		})
	}
}
