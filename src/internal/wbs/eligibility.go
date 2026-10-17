package wbs

// PhaseCompletionEligible reports whether a phase may be marked DONE (WBS 4.21):
// the phase has an active execution plan AND every one of that plan's work
// units is DONE. It is a pure, derived query over the supplied plan and units —
// no status mutation. Any PENDING/EXECUTING/FAILED/BLOCKED unit (or no plan,
// or a plan with zero units) makes the phase ineligible. The transition
// itself is a later service operation, not this query.
func PhaseCompletionEligible(plan *ExecutionPlan, units []*WorkUnit) bool {
	if plan == nil {
		return false
	}
	if len(units) == 0 {
		return false
	}
	for _, u := range units {
		if u == nil || u.Status != WorkUnitStatusDONE {
			return false
		}
	}
	return true
}
