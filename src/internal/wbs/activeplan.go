package wbs

import (
	"github.com/google/uuid"

	"overseer/internal/phase"
)

// CurrentPhase returns the project's current phase: the lowest-order phase
// that is not DONE (WBS 2.19). It returns nil when the project has no phases
// or every phase is complete (T2.19.1, T2.19.5).
func CurrentPhase(phases []*phase.Phase) *phase.Phase {
	var cur *phase.Phase
	for _, p := range phases {
		if p == nil || p.Status == phase.StatusDONE {
			continue
		}
		if cur == nil || p.Order < cur.Order {
			cur = p
		}
	}
	return cur
}

// ActivePlan returns a phase's active execution plan: the highest-version
// plan that is not SUPERSEDED (WBS 2.19). It returns nil when the phase has
// no such plan (T2.19.2). Plans of other phases are ignored.
func ActivePlan(phaseID uuid.UUID, plans []*ExecutionPlan) *ExecutionPlan {
	var active *ExecutionPlan
	for _, p := range plans {
		if p == nil || p.PhaseID != phaseID || p.Status == PlanStatusSUPERSEDED {
			continue
		}
		if active == nil || p.Version > active.Version {
			active = p
		}
	}
	return active
}
