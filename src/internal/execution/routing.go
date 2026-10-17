package execution

import (
	"sort"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// PlanState classifies the current phase's execution plan for bootstrap
// routing (WBS 15.26/15.27, PLAN Decision 4).
type PlanState string

// Plan states.
const (
	PlanMissing PlanState = "MISSING"
	PlanStale   PlanState = "STALE"
	PlanInvalid PlanState = "INVALID"
	PlanCurrent PlanState = "CURRENT"
)

// RequiresPlanning reports a state that routes toward planning.
func (p PlanState) RequiresPlanning() bool { return p != PlanCurrent }

// RoutingInput is the persisted data routing is derived from.
type RoutingInput struct {
	// Phases are the project's phases in any order (current persisted
	// ordering is authoritative, WBS 15.68).
	Phases []*phase.Phase
	// CurrentWBSVersion is the current phase's current WBS version (0 =
	// none). Only meaningful for the current phase.
	CurrentWBSVersion int
	// Plans are the current phase's plans (others are ignored).
	Plans []*wbs.ExecutionPlan
	// Units are the active plan's work units.
	Units []*wbs.WorkUnit
	// EligibleUnits are the active plan's dependency-ready PENDING/READY
	// units (WorkUnitRepository.ListEligible), ascending sequence.
	EligibleUnits []*wbs.WorkUnit
}

// RoutingFacts are the deterministic bootstrap routing facts.
type RoutingFacts struct {
	Phase              *phase.Phase
	CompletionEligible bool
	PlanState          PlanState
	Plan               *wbs.ExecutionPlan
	NextUnit           *wbs.WorkUnit
	AllUnitsDone       bool
	// NoExecutableUnit: non-DONE units remain but none is executable
	// (in progress or dependency-ready) — WBS_INVALID.
	NoExecutableUnit bool
	DoneUnits        int
	TotalUnits       int
}

// CurrentPhaseOf returns the lowest-order non-DONE phase (WBS 15.22).
func CurrentPhaseOf(phases []*phase.Phase) *phase.Phase { return wbs.CurrentPhase(phases) }

// ResolveRouting derives the routing facts (WBS 15.22–15.27, 15.64–15.68).
// The caller supplies Plans/Units/EligibleUnits of the phase CurrentPhaseOf
// selects.
func ResolveRouting(in RoutingInput) RoutingFacts {
	f := RoutingFacts{}
	f.Phase = wbs.CurrentPhase(in.Phases)
	if f.Phase == nil {
		f.CompletionEligible = true
		return f
	}
	plan := wbs.ActivePlan(f.Phase.ID, in.Plans)
	f.Plan = plan
	switch {
	case plan == nil:
		f.PlanState = PlanMissing
	case in.CurrentWBSVersion <= 0 || plan.WBSVersion != in.CurrentWBSVersion:
		f.PlanState = PlanStale
	case plan.Status == wbs.PlanStatusGENERATING || plan.Status == wbs.PlanStatusFAILED:
		f.PlanState = PlanInvalid
	default:
		f.PlanState = PlanCurrent
	}
	if f.PlanState != PlanCurrent {
		return f
	}
	units := make([]*wbs.WorkUnit, 0, len(in.Units))
	for _, u := range in.Units {
		if u != nil && u.ExecutionPlanID == plan.ID {
			units = append(units, u)
		}
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].Sequence < units[j].Sequence })
	f.TotalUnits = len(units)
	for _, u := range units {
		if u.Status == wbs.WorkUnitStatusDONE {
			f.DoneUnits++
		}
	}
	if f.TotalUnits > 0 && f.DoneUnits == f.TotalUnits {
		f.AllUnitsDone = true
		return f
	}
	if f.TotalUnits == 0 {
		// A current plan without units: nothing to implement, nothing
		// DONE — treated as no executable unit (WBS_INVALID).
		f.NoExecutableUnit = true
		return f
	}
	// In-progress unit takes precedence (resume), else the first
	// dependency-ready PENDING/READY unit (WBS 15.64/15.65).
	for _, u := range units {
		switch u.Status {
		case wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE, wbs.WorkUnitStatusVALIDATING:
			f.NextUnit = u
			return f
		}
	}
	eligible := map[uuid.UUID]bool{}
	for _, u := range in.EligibleUnits {
		if u != nil {
			eligible[u.ID] = true
		}
	}
	for _, u := range units {
		if eligible[u.ID] && (u.Status == wbs.WorkUnitStatusPENDING || u.Status == wbs.WorkUnitStatusREADY) {
			f.NextUnit = u
			return f
		}
	}
	f.NoExecutableUnit = true
	return f
}

// Routing outcome tokens (PLAN Decision 4).
const (
	OutcomePlanRequired = "PLAN_REQUIRED"
	OutcomePlanReady    = "PLAN_READY"
	OutcomeWorkComplete = "WORK_COMPLETE"
	OutcomeReady        = "READY"
	OutcomeProceed      = "PROCEED"
	OutcomeBootstrap    = "BOOTSTRAP"
)

// OutcomePreference returns the ordered outcome tokens a control state
// tries during bootstrap (PLAN Decision 4).
func (f RoutingFacts) OutcomePreference() []string {
	var out []string
	switch {
	case f.PlanState.RequiresPlanning():
		out = append(out, OutcomePlanRequired)
	case f.AllUnitsDone:
		out = append(out, OutcomeWorkComplete)
	case f.NextUnit != nil:
		out = append(out, OutcomePlanReady)
	}
	if f.PlanState == PlanCurrent && f.NextUnit != nil {
		out = append(out, OutcomeReady)
	}
	return append(out, OutcomeProceed)
}
