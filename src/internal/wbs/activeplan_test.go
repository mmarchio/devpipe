package wbs

import (
	"testing"

	"github.com/google/uuid"

	"overseer/internal/phase"
)

// T2.19.2 (unit level) — Active plan identified: the highest-version plan of
// the phase that is not SUPERSEDED is the active plan.
func TestT2192ActivePlanHighestVersion(t *testing.T) {
	phID := uuid.New()
	p1, _ := NewExecutionPlan(phID, 1)
	p2, _ := NewExecutionPlan(phID, 2)
	p2.Version = 2 // NewExecutionPlan starts every plan at version 1
	got := ActivePlan(phID, []*ExecutionPlan{p1, p2})
	if got == nil || got.ID != p2.ID {
		t.Fatalf("active plan = %+v, want plan v2", got)
	}
}

// A SUPERSEDED higher version is skipped; the latest non-SUPERSEDED wins.
func TestT2192ActivePlanSkipsSuperseded(t *testing.T) {
	phID := uuid.New()
	p1, _ := NewExecutionPlan(phID, 1)
	p2, _ := NewExecutionPlan(phID, 2)
	p2.Status = PlanStatusSUPERSEDED
	got := ActivePlan(phID, []*ExecutionPlan{p1, p2})
	if got == nil || got.ID != p1.ID {
		t.Fatalf("active plan = %+v, want plan v1 (v2 superseded)", got)
	}
}

// Plans of other phases are ignored.
func TestT2192ActivePlanIgnoresOtherPhases(t *testing.T) {
	phID := uuid.New()
	other := uuid.New()
	pOther, _ := NewExecutionPlan(other, 9)
	pMine, _ := NewExecutionPlan(phID, 1)
	got := ActivePlan(phID, []*ExecutionPlan{pOther, pMine})
	if got == nil || got.ID != pMine.ID {
		t.Fatalf("active plan = %+v, want the plan of the phase", got)
	}
}

// No plans (or all superseded) means no active plan.
func TestT2192ActivePlanNone(t *testing.T) {
	phID := uuid.New()
	if got := ActivePlan(phID, nil); got != nil {
		t.Fatalf("active plan = %+v, want nil", got)
	}
	p, _ := NewExecutionPlan(phID, 1)
	p.Status = PlanStatusSUPERSEDED
	if got := ActivePlan(phID, []*ExecutionPlan{p}); got != nil {
		t.Fatalf("active plan = %+v, want nil when all superseded", got)
	}
}

// T2.19.1 (unit level) — the current phase is the lowest non-DONE order.
func TestT2191CurrentPhaseLowestNonDone(t *testing.T) {
	phases := []*phase.Phase{
		mkPhase(2, "B", phase.StatusDONE),
		mkPhase(3, "C", phase.StatusPENDING),
		mkPhase(1, "A", phase.StatusDONE),
	}
	got := CurrentPhase(phases)
	if got == nil || got.Order != 3 || got.ID != phases[1].ID {
		t.Fatalf("current phase = %+v, want order 3", got)
	}
}

func TestT2191CurrentPhaseAllDone(t *testing.T) {
	got := CurrentPhase([]*phase.Phase{mkPhase(1, "A", phase.StatusDONE)})
	if got != nil {
		t.Fatalf("current phase = %+v, want nil for all-DONE", got)
	}
}

func TestT2191CurrentPhaseEmpty(t *testing.T) {
	if got := CurrentPhase(nil); got != nil {
		t.Fatalf("current phase = %+v, want nil for no phases", got)
	}
}

func mkPhase(order int, name string, status phase.PhaseStatus) *phase.Phase {
	p, _ := phase.New(uuid.New(), order, name, "")
	p.Status = status
	return p
}
