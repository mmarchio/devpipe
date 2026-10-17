// Condition field resolvers against persisted data (WBS 6.11 / 6.12 / 6.13).
// Each resolver is a PURE function over declared inputs — no DB access in
// the resolver logic so the architecture guard stays green; integration
// tests drive them with real persisted rows.
package state

import (
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// Outcome tokens the resolvers select (the desired next-state outcome the
// state machine evaluates).
const (
	// WBS 6.11 outcomes.
	OutcomeContinueCurrentSession = "CONTINUE_CURRENT_SESSION"
	OutcomeResetSession           = "RESET_SESSION"

	// WBS 6.12 outcomes.
	OutcomeNextUnit      = "NEXT_UNIT"
	OutcomePhaseValidate = "PHASE_VALIDATE"

	// WBS 6.13 outcomes.
	OutcomePlanNextPhase = "PLAN_NEXT_PHASE"
	OutcomeProjectDone   = "PROJECT_DONE"
)

// WorkUnitStatusFact is the declared, domain-typed input describing one
// persisted work unit for the remaining-work resolver.
type WorkUnitStatusFact struct {
	Status wbs.WorkUnitStatus
}

// PhaseStatusFact is the declared, domain-typed input describing one
// persisted phase for the remaining-phase resolver.
type PhaseStatusFact struct {
	Status phase.PhaseStatus
}

// ResolveContextThreshold selects the context-threshold outcome (WBS 6.11):
// usage below the threshold continues the current session; usage at or
// above it resets the session. The threshold is a parameter — the caller
// supplies it from persisted project configuration (T6.11.4). Defined
// fallback (T6.11.5): a missing (zero) usage value is treated as
// below-threshold and selects CONTINUE_CURRENT_SESSION.
func ResolveContextThreshold(usage, threshold float64) (string, error) {
	if usage >= threshold {
		return OutcomeResetSession, nil
	}
	return OutcomeContinueCurrentSession, nil
}

// ResolveRemainingWork selects the work-remaining outcome (WBS 6.12):
// non-DONE units count as remaining work — any DONE unit is ignored
// (T6.12.3), any BLOCKED or FAILED unit likewise keeps work remaining
// (T6.12.4) so phase completion is never falsely reported. Remaining > 0
// selects NEXT_UNIT; remaining == 0 selects PHASE_VALIDATE.
func ResolveRemainingWork(units []WorkUnitStatusFact) string {
	for _, u := range units {
		if u.Status != wbs.WorkUnitStatusDONE {
			return OutcomeNextUnit
		}
	}
	return OutcomePhaseValidate
}

// ResolveRemainingPhases selects the remaining-phase outcome (WBS 6.13):
// phases are counted dynamically from the caller's list — a phase appended
// after a prior query is discoverable by the next call (T6.13.3, no
// snapshot/caching) — and DONE phases are skipped (T6.13.4). Remaining > 0
// selects PLAN_NEXT_PHASE; remaining == 0 selects PROJECT_DONE.
func ResolveRemainingPhases(phases []PhaseStatusFact) string {
	for _, p := range phases {
		if p.Status != phase.StatusDONE {
			return OutcomePlanNextPhase
		}
	}
	return OutcomeProjectDone
}
