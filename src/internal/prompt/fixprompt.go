package prompt

import (
	"strings"

	"github.com/google/uuid"
)

// P8-27 — WBS 8.19 fix (FIX_WORK_UNIT) prompt composition.
//
// ComposeFixPrompt renders the fix-state prompt components: the RETAINED
// work unit (the same unit that failed validation — its original
// acceptance criteria and required tests ride on the composed work-unit
// component, P8-14), the captured failed validation results (the failed
// criteria, the failed tests, and the relevant error output — the
// emphasized context, T8.19.2), and the implementation response schema
// (P8-23 ComposeImplementationResponseSchema — the fix request reports
// through the implementation protocol). The composition consumes ONLY
// the caller-supplied captured values (the P8-16..P8-19 / P8-22/P8-23
// captured-values discipline) — no "latest" input — so repeated
// composition over the same input is byte-identical.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - T8.19.1: the SAME work unit is retained — it is composed via
//     ComposeWorkUnit and a fixed "fix target:" line naming the unit is
//     appended to the work-unit component, so the fix request is pinned
//     to exactly the failed unit.
//   - T8.19.2: the captured failed tests render verbatim (captured
//     order) on exactly one TEST_CONTEXT component, with the captured
//     relevant error output as the "error output:" section of that
//     component; the captured failed criteria render verbatim (captured
//     order) on exactly one TASK_CONTEXT component.
//   - T8.19.3: "passed unrelated tests need not dominate context" —
//     CHOSEN POLICY: only the FAILED result items and the
//     caller-captured RELEVANT error output enter the composition;
//     passed tests have no input channel at all, so they can never
//     dominate the context. An empty failed-result capture composes to
//     ZERO failure components (no failure, no placeholder — the "when
//     available" choice, mirroring P8-26).
//   - T8.19.4: the unit's ORIGINAL acceptance criteria ride verbatim on
//     the composed work-unit component (WBS 8.14) — retained, not
//     replaced by the failure results.
//   - T8.19.5: the NEXT work unit is excluded — FixPromptInput carries
//     no next-unit channel (pinned at the compile level in the test).
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the
// retained unit is REQUIRED — a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective, propagated from the P8-14
// work-unit composition). A cleared response-protocol version (<= 0)
// and an empty allowed-status capture are composition failures
// (ErrMissingProtocolVersion / ErrMissingAllowedStatuses, the P8-23
// contract).

// FailedValidationInput carries the caller-supplied CAPTURED failed
// validation results fed to ComposeFixPrompt (WBS 8.19 "Failed Criteria
// + Failed Tests + Relevant Error Output"): the acceptance criteria that
// failed validation, the tests that failed, and the relevant error
// output captured for them. The policy (T8.19.3): these FAILED items and
// the relevant error output are the emphasis — passed tests have no
// input channel and never enter the composition.
type FailedValidationInput struct {
	FailedCriteria []string
	FailedTests    []string
	RelevantErrors []string
}

// FixPromptInput carries the caller-supplied captured values for
// ComposeFixPrompt (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline): the retained work unit, the captured failed validation
// results (when available), and the implementation response schema's
// identity/version + allowed statuses. There is deliberately NO next-unit
// field (T8.19.5 — the next work unit is excluded).
type FixPromptInput struct {
	RetainedUnit    WorkUnitContextInput
	FailedResult    FailedValidationInput
	ProtocolID      uuid.UUID
	ProtocolVersion int
	AllowedStatuses []string
}

// ComposeFixPrompt renders the FIX_WORK_UNIT prompt components (WBS
// 8.19): the retained work unit (P8-14 — carries the ORIGINAL acceptance
// criteria and required tests), the captured failed validation results
// (T8.19.2/T8.19.3), and the implementation response schema (P8-23). It
// consumes ONLY the caller-supplied captured values — no "latest"
// input, no hard-coded list — so repeated composition over the same
// input is byte-identical (the Phase 8 determinism requirement).
//
// The retained unit is REQUIRED: a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective). The response schema's
// cleared version / empty statuses fail via the P8-23 contract. The next
// work unit is never composed — the input carries no channel for it
// (T8.19.5).
func ComposeFixPrompt(in FixPromptInput) (Components, error) {
	unit, err := NewWorkUnitContext(in.RetainedUnit)
	if err != nil {
		return nil, err
	}
	// Compose the retained unit (WBS 8.14). ComposeWorkUnit returns
	// exactly one work-unit component.
	unitComps, err := ComposeWorkUnit(unit)
	if err != nil {
		return nil, err
	}
	// T8.19.1: append the fixed "fix target:" line naming the retained
	// unit, so the fix request is pinned to exactly the failed unit.
	unitComps[0].Content += "\nfix target: " +
		"fix work unit " + unit.ID.String() +
		" — satisfy all acceptance criteria and pass all required tests."

	out := make(Components, 0, 3)
	out = append(out, unitComps...)

	// T8.19.2: the captured failed criteria — verbatim, captured order,
	// on exactly one TASK_CONTEXT component when any are captured.
	if len(in.FailedResult.FailedCriteria) > 0 {
		b := "failed acceptance criteria: " + strings.Join(in.FailedResult.FailedCriteria, "; ") + "\n"
		c, err := NewPromptComponent(
			ComponentTaskContext,
			SourceWorkUnit,
			0,
			b,
			unit.ID,
			0,
			"",
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	// T8.19.2/T8.19.3: the captured failed tests (verbatim, captured
	// order) plus the caller-captured relevant error output, on exactly
	// one TEST_CONTEXT component when any failed item is captured.
	// Passed tests have no input channel (the T8.19.3 policy) and an
	// empty capture composes to zero components (no failure, no
	// placeholder).
	if len(in.FailedResult.FailedTests) > 0 || len(in.FailedResult.RelevantErrors) > 0 {
		var b strings.Builder
		if len(in.FailedResult.FailedTests) > 0 {
			b.WriteString("failed tests: " + strings.Join(in.FailedResult.FailedTests, "; ") + "\n")
		}
		if len(in.FailedResult.RelevantErrors) > 0 {
			b.WriteString("error output: " + strings.Join(in.FailedResult.RelevantErrors, "\n") + "\n")
		}
		c, err := NewPromptComponent(
			ComponentTestContext,
			SourceWorkUnit,
			0,
			strings.TrimRight(b.String(), "\n"),
			unit.ID,
			0,
			"",
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	// The implementation response schema (P8-23) — the fix request
	// reports through the implementation protocol.
	schema, err := ComposeImplementationResponseSchema(in.ProtocolID, in.ProtocolVersion, in.AllowedStatuses)
	if err != nil {
		return nil, err
	}
	out = append(out, schema...)
	return out, nil
}
