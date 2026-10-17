package integration

import (
	"context"
	"testing"

	"overseer/internal/service"
)

// P6-23 (WBS 6.31/6.32): work-unit + phase context binding retention on the
// transition-application surface. Chosen retention semantics (stated here
// and in ACTIVE_TASK.md): an OMITTED (nil) association in the apply input
// RETAINS the stored runtime-state association; an explicitly provided value
// RE-BINDS it. The WBS requires no clear path, so nil never clears — the
// stored phase/unit can only move via an explicit rebind (unit replacement
// after UNIT_DONE; phase advancement).

// p623Apply drives the P6-21 application service over the harness pool and
// returns the applied result.
func p623Apply(t *testing.T, h *Harness, project, from, to, outcome string, expectedVersion int, phase, unit *string) *service.AppliedTransition {
	t.Helper()
	out, err := p621Service(h, true).ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: from, ToStateID: to, Outcome: outcome,
		PhaseID: phase, WorkUnitID: unit, ExpectedVersion: expectedVersion,
	})
	if err != nil {
		t.Fatalf("apply %s->%s (%s): %v", from, to, outcome, err)
	}
	return out
}

// p623Stored reads the persisted runtime_state ground truth (direct SQL,
// not the service readback) so retention is asserted against storage.
func p623Stored(t *testing.T, h *Harness, project string) (stateID, phaseID, unitID string, version int) {
	t.Helper()
	if err := h.DBScan(context.Background(),
		"SELECT current_state_id::text, coalesce(phase_id::text,''), coalesce(work_unit_id::text,''), version "+
			"FROM runtime_state WHERE project_id = '"+project+"'",
		&stateID, &phaseID, &unitID, &version); err != nil {
		t.Fatalf("read runtime_state: %v", err)
	}
	return
}

// p623SecondUnit seeds a second work unit in the fixture phase's plan — the
// "next unit" a rebind must be allowed to select (non-vacuity: a test that
// only asserted "not nil" would pass for the wrong reason).
func p623SecondUnit(t *testing.T, h *Harness, project, phase string) string {
	t.Helper()
	ctx := context.Background()
	var planID string
	if err := h.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phase+"'", &planID); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO work_units (id, execution_plan_id, sequence, name, kind, status, objective, created_at) VALUES (gen_random_uuid(), '"+planID+"', 2, 'W2', 'ATOMIC', 'PENDING', 'obj2', now())"); err != nil {
		t.Fatalf("seed second unit: %v", err)
	}
	var unit2 string
	if err := h.DBScan(ctx, "SELECT id::text FROM work_units WHERE execution_plan_id = '"+planID+"' AND name = 'W2'", &unit2); err != nil {
		t.Fatalf("read second unit: %v", err)
	}
	return unit2
}

// p623SecondPhase seeds a second phase for the project — the advancement
// target for the phase-rebind tests.
func p623SecondPhase(t *testing.T, h *Harness, project string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "INSERT INTO phases (id, project_id, name, description, phase_order, status, created_at, updated_at) VALUES (gen_random_uuid(), '"+project+"', 'P2', '', 2, 'PENDING', now(), now())"); err != nil {
		t.Fatalf("seed second phase: %v", err)
	}
	var phase2 string
	if err := h.DBScan(ctx, "SELECT id::text FROM phases WHERE project_id = '"+project+"' AND name = 'P2'", &phase2); err != nil {
		t.Fatalf("read second phase: %v", err)
	}
	return phase2
}

// T6.31.1 — Work-unit ID retained: a transition that carries the work-unit
// binding persists it and it is read back unchanged.
func TestP623WorkUnitIDRetained(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, phase, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph, un := phase, unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, &un)
	_, _, storedUnit, _ := p623Stored(t, h, project)
	if storedUnit != unit {
		t.Fatalf("stored work_unit_id = %q, want %q (the carried binding must persist unchanged)", storedUnit, unit)
	}
}

// T6.31.2 — Continuation doesn't select next unit: a continuation (same
// unit) transition that omits the work-unit binding does NOT advance to the
// next unit — the stored work_unit_id stays the same (neither nil nor the
// seeded second unit).
func TestP623ContinuationDoesNotSelectNextUnit(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, phase, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph, un := phase, unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, &un)
	unit2 := p623SecondUnit(t, h, project, phase)
	// The continuation transition omits the unit (nil = retain).
	p623Apply(t, h, project, stateB, stateC, "CONTINUE", 2, nil, nil)
	_, _, storedUnit, _ := p623Stored(t, h, project)
	if storedUnit == "" {
		t.Fatal("stored work_unit_id cleared on continuation — retention violated")
	}
	if storedUnit == unit2 {
		t.Fatalf("stored work_unit_id = %q, the continuation advanced to the NEXT unit %q", storedUnit, unit2)
	}
	if storedUnit != unit {
		t.Fatalf("stored work_unit_id = %q, want %q (unchanged)", storedUnit, unit)
	}
}

// T6.31.3 — Reset path retains same unit: a reset transition (the unit's
// session restarts) keeps the same work-unit association.
func TestP623ResetPathRetainsSameUnit(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	un := unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, nil, &un)
	// Reset back to the implementing state of the same unit — no unit given.
	p623Apply(t, h, project, stateB, stateA, "RESET", 2, nil, nil)
	_, _, storedUnit, _ := p623Stored(t, h, project)
	if storedUnit != unit {
		t.Fatalf("stored work_unit_id after reset = %q, want %q (reset must retain the same unit)", storedUnit, unit)
	}
}

// T6.31.4 — UNIT_DONE allows future selection of next unit: after UNIT_DONE
// the association is replaceable — a LATER transition may explicitly bind
// the next unit and it must be stored.
func TestP623UnitDoneAllowsFutureSelection(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, phase, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph, un := phase, unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, &un)
	p623Apply(t, h, project, stateB, stateC, "UNIT_DONE", 2, nil, nil)
	unit2 := p623SecondUnit(t, h, project, phase)
	// The unit is replaceable: an explicit rebind to the next unit succeeds.
	un2 := unit2
	p623Apply(t, h, project, stateC, stateA, "NEXT_UNIT", 3, nil, &un2)
	_, _, storedUnit, _ := p623Stored(t, h, project)
	if storedUnit != unit2 {
		t.Fatalf("stored work_unit_id after explicit rebind = %q, want %q (the association must be replaceable after UNIT_DONE)", storedUnit, unit2)
	}
}

// T6.32.1 — Phase retained across states: the phase association persists
// across successive transitions that do not rebind it.
func TestP623PhaseRetainedAcrossStates(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, phase, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph := phase
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, nil)
	// Successive phase-execution states, all omitting the phase.
	p623Apply(t, h, project, stateB, stateC, "DONE", 2, nil, nil)
	p623Apply(t, h, project, stateC, stateA, "PLAN", 3, nil, nil)
	_, storedPhase, _, _ := p623Stored(t, h, project)
	if storedPhase != phase {
		t.Fatalf("stored phase_id after successive transitions = %q, want %q (phase must be retained until advancement)", storedPhase, phase)
	}
}

// T6.22.2/T6.32.2 — Work-unit changes don't alter phase: re-binding the work
// unit (explicit unit, no phase) leaves the stored phase association
// untouched.
func TestP623WorkUnitChangeDoesNotAlterPhase(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, phase, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph, un := phase, unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, &un)
	unit2 := p623SecondUnit(t, h, project, phase)
	un2 := unit2
	p623Apply(t, h, project, stateB, stateC, "NEXT_UNIT", 2, nil, &un2)
	storedState, storedPhase, storedUnit, _ := p623Stored(t, h, project)
	_ = storedState
	if storedUnit != unit2 {
		t.Fatalf("stored work_unit_id = %q, want %q (the rebind must land)", storedUnit, unit2)
	}
	if storedPhase != phase {
		t.Fatalf("stored phase_id = %q after a work-unit rebind, want %q (unit changes must not touch phase)", storedPhase, phase)
	}
}

// T6.32.3 — Phase changes only through allowed advancement: the phase
// association moves ONLY when a transition explicitly rebinds it; omitted
// transitions never move it, and an explicit rebind (advancement) does.
func TestP623PhaseChangesOnlyThroughAdvancement(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, phase, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	ph := phase
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, nil)
	p623Apply(t, h, project, stateB, stateC, "DONE", 2, nil, nil)
	if _, sp, _, _ := p623Stored(t, h, project); sp != phase {
		t.Fatalf("stored phase before advancement = %q, want %q (no implicit change allowed)", sp, phase)
	}
	phase2 := p623SecondPhase(t, h, project)
	ph2 := phase2
	p623Apply(t, h, project, stateC, stateA, "PHASE_DONE", 3, &ph2, nil)
	if _, sp, _, _ := p623Stored(t, h, project); sp != phase2 {
		t.Fatalf("stored phase after explicit advancement = %q, want %q", sp, phase2)
	}
	// And the new phase is itself retained by subsequent omitted transitions.
	p623Apply(t, h, project, stateA, stateB, "GO", 4, nil, nil)
	if _, sp, _, _ := p623Stored(t, h, project); sp != phase2 {
		t.Fatalf("stored phase after advancement+continuation = %q, want %q (the rebinding must stick)", sp, phase2)
	}
}
