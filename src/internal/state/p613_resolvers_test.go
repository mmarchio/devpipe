// P6-13 (TDD RED): condition field resolvers against persisted data (WBS
// 6.11 / 6.12 / 6.13). The resolvers are pure functions over declared
// inputs; the integration tests in internal/integration drive them with
// real persisted rows.
package state

import (
	"errors"
	"testing"

	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// T6.11.1 — Usage below threshold selects continuation.
func TestT6111BelowThresholdSelectsContinuation(t *testing.T) {
	outcome, err := ResolveContextThreshold(0.49, 0.80)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	if outcome != OutcomeContinueCurrentSession {
		t.Fatalf("usage 0.49 < 0.80: outcome = %q, want %q", outcome, OutcomeContinueCurrentSession)
	}
}

// T6.11.2 — Usage equal to threshold selects reset.
func TestT6112EqualThresholdSelectsReset(t *testing.T) {
	outcome, err := ResolveContextThreshold(0.80, 0.80)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	if outcome != OutcomeResetSession {
		t.Fatalf("usage 0.80 >= 0.80: outcome = %q, want %q", outcome, OutcomeResetSession)
	}
}

// T6.11.3 — Usage above threshold selects reset.
func TestT6113AboveThresholdSelectsReset(t *testing.T) {
	outcome, err := ResolveContextThreshold(0.81, 0.80)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	if outcome != OutcomeResetSession {
		t.Fatalf("usage 0.81 >= 0.80: outcome = %q, want %q", outcome, OutcomeResetSession)
	}
}

// T6.11.4 — Threshold is a parameter supplied from persisted project
// configuration, never a hardcoded constant: two different thresholds yield
// different outcomes for the same usage.
func TestT6114ThresholdIsParameterized(t *testing.T) {
	low, err := ResolveContextThreshold(0.70, 0.75)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	high, err := ResolveContextThreshold(0.70, 0.65)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	if low != OutcomeContinueCurrentSession {
		t.Fatalf("usage 0.70 vs threshold 0.75: outcome = %q, want continuation", low)
	}
	if high != OutcomeResetSession {
		t.Fatalf("usage 0.70 vs threshold 0.65: outcome = %q, want reset", high)
	}
}

// T6.11.5 — Missing usage handled by the defined fallback: a missing
// (zero) usage value is treated as below-threshold and selects
// CONTINUE_CURRENT_SESSION (recorded in ACTIVE_TASK.md Notes).
func TestT6115MissingUsageFallsBackToContinuation(t *testing.T) {
	outcome, err := ResolveContextThreshold(0, 0.80)
	if err != nil {
		t.Fatalf("ResolveContextThreshold with missing usage: %v", err)
	}
	if outcome != OutcomeContinueCurrentSession {
		t.Fatalf("missing usage: outcome = %q, want the defined fallback %q", outcome, OutcomeContinueCurrentSession)
	}
}

// T6.11 end-to-end: the resolver feeds the P6-12 TransitionContextInput, so
// the P6-12 type is exercised through the resolver output.
func TestT611ResolverFeedsTransitionContextInput(t *testing.T) {
	outcome, err := ResolveContextThreshold(0.90, 0.80)
	if err != nil {
		t.Fatalf("ResolveContextThreshold: %v", err)
	}
	in := TransitionContextInput{
		Outcome:          outcome,
		ContextUsage:     0.90,
		ContextThreshold: 0.80,
	}
	ctx, err := NewTransitionContext(in)
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	if ctx.Outcome != OutcomeResetSession {
		t.Fatalf("context outcome = %q", ctx.Outcome)
	}
}

// T6.12.1 — Remaining work selects the next-unit path.
func TestT6121RemainingWorkSelectsNextUnit(t *testing.T) {
	outcome := ResolveRemainingWork([]WorkUnitStatusFact{
		{Status: wbs.WorkUnitStatusPENDING},
		{Status: wbs.WorkUnitStatusDONE},
	})
	if outcome != OutcomeNextUnit {
		t.Fatalf("one pending unit: outcome = %q, want %q", outcome, OutcomeNextUnit)
	}
}

// T6.12.2 — No remaining work selects the phase-validation path.
func TestT6122NoRemainingWorkSelectsPhaseValidation(t *testing.T) {
	if got := ResolveRemainingWork(nil); got != OutcomePhaseValidate {
		t.Fatalf("no units: outcome = %q, want %q", got, OutcomePhaseValidate)
	}
	if got := ResolveRemainingWork([]WorkUnitStatusFact{{Status: wbs.WorkUnitStatusDONE}}); got != OutcomePhaseValidate {
		t.Fatalf("all units DONE: outcome = %q, want %q", got, OutcomePhaseValidate)
	}
}

// T6.12.3 — DONE units are ignored when counting remaining work.
func TestT6123DoneUnitsIgnored(t *testing.T) {
	facts := []WorkUnitStatusFact{
		{Status: wbs.WorkUnitStatusDONE},
		{Status: wbs.WorkUnitStatusDONE},
		{Status: wbs.WorkUnitStatusEXECUTING},
	}
	if got := ResolveRemainingWork(facts); got != OutcomeNextUnit {
		t.Fatalf("one non-DONE unit among DONE: outcome = %q, want %q", got, OutcomeNextUnit)
	}
}

// T6.12.4 — A BLOCKED or FAILED unit prevents a false "no remaining work"
// result: with one DONE and one BLOCKED (resp. FAILED) unit the outcome is
// NOT PHASE_VALIDATE.
func TestT6124BlockedFailedPreventFalsePhaseCompletion(t *testing.T) {
	for _, s := range []wbs.WorkUnitStatus{wbs.WorkUnitStatusBLOCKED, wbs.WorkUnitStatusFAILED} {
		got := ResolveRemainingWork([]WorkUnitStatusFact{
			{Status: wbs.WorkUnitStatusDONE},
			{Status: s},
		})
		if got == OutcomePhaseValidate {
			t.Fatalf("unit %s present: outcome = PHASE_VALIDATE — a false phase completion", s)
		}
	}
}

// T6.13.1 — A pending next phase selects phase planning.
func TestT6131PendingPhaseSelectsPhasePlanning(t *testing.T) {
	got := ResolveRemainingPhases([]PhaseStatusFact{
		{Status: phase.StatusPENDING},
	})
	if got != OutcomePlanNextPhase {
		t.Fatalf("one pending phase: outcome = %q, want %q", got, OutcomePlanNextPhase)
	}
}

// T6.13.2 — No remaining phase selects project completion.
func TestT6132NoRemainingPhaseSelectsProjectCompletion(t *testing.T) {
	if got := ResolveRemainingPhases(nil); got != OutcomeProjectDone {
		t.Fatalf("no phases: outcome = %q, want %q", got, OutcomeProjectDone)
	}
}

// T6.13.3 (unit half) — phases are counted dynamically from the caller's
// list: a phase appended to the input after a prior query is discoverable
// by the next call (no snapshot inside the resolver).
func TestT6133DynamicallyCounted(t *testing.T) {
	facts := []PhaseStatusFact{{Status: phase.StatusDONE}}
	if got := ResolveRemainingPhases(facts); got != OutcomeProjectDone {
		t.Fatalf("pre-append: outcome = %q, want %q", got, OutcomeProjectDone)
	}
	facts = append(facts, PhaseStatusFact{Status: phase.StatusPENDING})
	if got := ResolveRemainingPhases(facts); got != OutcomePlanNextPhase {
		t.Fatalf("post-append: outcome = %q, want %q", got, OutcomePlanNextPhase)
	}
}

// T6.13.4 — DONE phases are skipped in the remaining count.
func TestT6134DonePhasesSkipped(t *testing.T) {
	if got := ResolveRemainingPhases([]PhaseStatusFact{
		{Status: phase.StatusDONE},
		{Status: phase.StatusDONE},
	}); got != OutcomeProjectDone {
		t.Fatalf("all phases DONE: outcome = %q, want %q", got, OutcomeProjectDone)
	}
}

// Contract guard: the declared surface of the P6-13 resolvers.
var _ = func() (string, error) {
	if _, err := ResolveContextThreshold(0, 0); err != nil {
		return "", errors.New("state: resolver signature drift")
	}
	return OutcomeContinueCurrentSession, nil
}
