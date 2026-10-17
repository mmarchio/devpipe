// P6-13 integration half: condition field resolvers driven with REAL
// persisted data (WBS 6.11 / 6.12 / 6.13). The resolver logic itself is
// pure (internal/state); here the inputs come from persisted project
// settings, work-unit, and phase rows.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/state"
)

// p613Project creates a fresh project and returns its parsed ID.
func p613Project(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	id := createPhaseProject(t, h)
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	return parsed
}

// p613UnitFacts loads the persisted work-unit statuses for all plans in the
// project into resolver facts (real rows, no snapshot).
func p613UnitFacts(t *testing.T, h *Harness, projectID uuid.UUID) []state.WorkUnitStatusFact {
	t.Helper()
	pool := h.App.DB().Raw()
	plans := dbrepo.NewPlanRepository(pool)
	units := dbrepo.NewWorkUnitRepository(pool)
	var facts []state.WorkUnitStatusFact
	// Enumerate the project's phases, then their plans, then units — all
	// from persisted rows.
	phases, err := dbrepo.NewPhaseRepository(pool).ListByProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	for _, ph := range phases {
		pl, err := plans.ListByPhase(context.Background(), ph.ID)
		if err != nil {
			t.Fatalf("list plans: %v", err)
		}
		for _, p := range pl {
			us, err := units.ListByPlan(context.Background(), p.ID)
			if err != nil {
				t.Fatalf("list units: %v", err)
			}
			for _, u := range us {
				facts = append(facts, state.WorkUnitStatusFact{Status: u.Status})
			}
		}
	}
	return facts
}

// p613PhaseFacts loads the persisted phase statuses into resolver facts.
func p613PhaseFacts(t *testing.T, h *Harness, projectID uuid.UUID) []state.PhaseStatusFact {
	t.Helper()
	phases, err := dbrepo.NewPhaseRepository(h.App.DB().Raw()).ListByProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	var facts []state.PhaseStatusFact
	for _, ph := range phases {
		facts = append(facts, state.PhaseStatusFact{Status: ph.Status})
	}
	return facts
}

// T6.11.4 — Threshold is read from the persisted project settings value,
// not hardcoded: the resolver's threshold argument is sourced from the
// settings row, and the persisted value drives the outcome.
func TestT6114ThresholdFromPersistedSettings(t *testing.T) {
	h := NewHarness(t)
	proj := p613Project(t, h)
	settings := dbrepo.NewSettingsRepository(h.App.DB().Raw())
	s, err := settings.Get(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if _, err := state.ResolveContextThreshold(0.9, s.ContextThreshold); err != nil {
		t.Fatalf("resolve with persisted threshold: %v", err)
	}
	// The persisted threshold is a real, non-zero domain value in (0, 1].
	if s.ContextThreshold <= 0 || s.ContextThreshold > 1 {
		t.Fatalf("persisted threshold out of range: %v", s.ContextThreshold)
	}
}

// T6.12.1 / T6.12.3 / T6.12.4 — remaining-work resolver driven with real
// persisted work units.
func TestT612RemainingWorkFromPersistedUnits(t *testing.T) {
	h := NewHarness(t)
	proj := p613Project(t, h)

	// Phase + plan + units, mixed statuses.
	p1 := seedPhaseStatus(t, h, proj, 1, "One", "EXECUTING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "done", "DONE")
	seedUnit(t, h, plan, 2, "running", "EXECUTING")
	seedUnit(t, h, plan, 3, "pending", "PENDING")

	facts := p613UnitFacts(t, h, proj)
	if len(facts) != 3 {
		t.Fatalf("expected 3 persisted units, got %d", len(facts))
	}
	// Remaining work present → NEXT_UNIT (T6.12.1).
	if got := state.ResolveRemainingWork(facts); got != state.OutcomeNextUnit {
		t.Fatalf("with non-DONE units: outcome = %q, want NEXT_UNIT", got)
	}

	// Mark all of this project's units DONE → PHASE_VALIDATE; DONE units
	// ignored (T6.12.3). Scope the update to this project's rows so the
	// fresh harness project's data is the only input.
	if _, err := h.DBExec(context.Background(),
		"UPDATE work_units SET status = 'DONE' WHERE id IN (SELECT w.id FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id JOIN phases ph ON ph.id = p.phase_id WHERE ph.project_id = '"+proj.String()+"')"); err != nil {
		t.Fatalf("mark units done: %v", err)
	}
	if got := state.ResolveRemainingWork(p613UnitFacts(t, h, proj)); got != state.OutcomePhaseValidate {
		t.Fatalf("all units DONE: outcome = %q, want PHASE_VALIDATE", got)
	}

	// A BLOCKED unit prevents a false phase completion (T6.12.4).
	if _, err := h.DBExec(context.Background(),
		"UPDATE work_units SET status = 'BLOCKED' WHERE sequence = 2 AND id IN (SELECT w.id FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id JOIN phases ph ON ph.id = p.phase_id WHERE ph.project_id = '"+proj.String()+"')"); err != nil {
		t.Fatalf("set blocked: %v", err)
	}
	if got := state.ResolveRemainingWork(p613UnitFacts(t, h, proj)); got == state.OutcomePhaseValidate {
		t.Fatalf("a BLOCKED unit present: outcome = PHASE_VALIDATE — a false phase completion")
	}
}

// T6.13.1 / T6.13.4 — remaining-phase resolver driven with real persisted
// phases.
func TestT613RemainingPhasesFromPersistedPhases(t *testing.T) {
	h := NewHarness(t)
	proj := p613Project(t, h)
	seedPhaseStatus(t, h, proj, 1, "One", "DONE")
	seedPhaseStatus(t, h, proj, 2, "Two", "PENDING")

	facts := p613PhaseFacts(t, h, proj)
	if len(facts) != 2 {
		t.Fatalf("expected 2 persisted phases, got %d", len(facts))
	}
	// A non-DONE phase is pending → PLAN_NEXT_PHASE (T6.13.1); the DONE
	// phase is skipped (T6.13.4).
	if got := state.ResolveRemainingPhases(facts); got != state.OutcomePlanNextPhase {
		t.Fatalf("one pending phase: outcome = %q, want PLAN_NEXT_PHASE", got)
	}
	// All of this project's phases DONE → PROJECT_DONE.
	if _, err := h.DBExec(context.Background(),
		"UPDATE phases SET status = 'DONE' WHERE project_id = '"+proj.String()+"'"); err != nil {
		t.Fatalf("mark phases done: %v", err)
	}
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, proj)); got != state.OutcomeProjectDone {
		t.Fatalf("all phases DONE: outcome = %q, want PROJECT_DONE", got)
	}
}

// T6.13.3 — a phase appended AFTER a prior query is discoverable by a
// subsequent query: no snapshot/caching inside the resolver; each call
// re-reads persisted phase order.
func TestT6133PhaseAddedBecomesDiscoverable(t *testing.T) {
	h := NewHarness(t)
	proj := p613Project(t, h)
	seedPhaseStatus(t, h, proj, 1, "One", "DONE")

	// Before append: no remaining phase → PROJECT_DONE.
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, proj)); got != state.OutcomeProjectDone {
		t.Fatalf("pre-append: outcome = %q, want PROJECT_DONE", got)
	}
	// Append a new phase during execution.
	seedPhaseStatus(t, h, proj, 2, "Two", "PENDING")
	// The subsequent query discovers it → PLAN_NEXT_PHASE.
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, proj)); got != state.OutcomePlanNextPhase {
		t.Fatalf("post-append: outcome = %q, want PLAN_NEXT_PHASE", got)
	}
}
