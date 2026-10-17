package wbs

// PlanStalenessState is the derived relationship between a phase's active
// execution plan and the phase's current WBS version (WBS 4.14). It is
// always computed from the plan's stored source version and the current
// WBS version — never persisted.
type PlanStalenessState string

const (
	// PlanStaleCurrent: the active plan was generated from the current WBS
	// version.
	PlanStaleCurrent PlanStalenessState = "CURRENT"
	// PlanStaleStale: the active plan was generated from an older WBS
	// version than the current one; regeneration is necessary.
	PlanStaleStale PlanStalenessState = "STALE"
	// PlanStaleUnplanned: the phase has no execution plan (or no WBS).
	PlanStaleUnplanned PlanStalenessState = "UNPLANNED"
)

// String returns the canonical state token.
func (s PlanStalenessState) String() string { return string(s) }

// PlanStaleness derives the plan-staleness state from the phase's active
// execution plan (WBS 2.19: highest-version non-SUPERSEDED) and the phase's
// current WBS version (WBS 4.14). A nil plan — no plan, or no WBS — is
// Unplanned; a plan whose stored source version equals the current version
// is Current; a plan bound to an older version is Stale. The result is
// purely derived: nothing here mutates the plan or persists state, so a
// WBS update marks the previous plan stale on the same tick.
func PlanStaleness(plan *ExecutionPlan, currentWBSVersion int) PlanStalenessState {
	if plan == nil || currentWBSVersion <= 0 {
		return PlanStaleUnplanned
	}
	if plan.WBSVersion == currentWBSVersion {
		return PlanStaleCurrent
	}
	return PlanStaleStale
}
