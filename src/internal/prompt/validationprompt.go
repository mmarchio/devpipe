package prompt

import (
	"strings"

	"github.com/google/uuid"
)

// P8-26 — WBS 8.18 validation (VALIDATE_WORK_UNIT) prompt composition.
//
// ComposeValidationPrompt renders the validation-state prompt components:
// the work unit (its acceptance criteria and required tests ride on the
// composed work-unit component, P8-14), the captured test state (one
// TEST_CONTEXT component when available), the captured implementation
// status/artifacts (one additional TASK_CONTEXT component when
// available), and the validation response schema (P8-23
// ComposeValidationResponseSchema). The composition consumes ONLY the
// caller-supplied captured values (the P8-16..P8-19 / P8-22/P8-23
// captured-values discipline) — no "latest" input — so repeated
// composition over the same input is byte-identical.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - T8.18.1/T8.18.2: the unit's acceptance criteria and required tests
//     are rendered verbatim (captured order) on the work-unit component.
//   - T8.18.3: the captured test state composes to exactly one
//     TEST_CONTEXT component carrying the content verbatim; an empty
//     capture composes to ZERO components (no failure, no placeholder —
//     the "when available" choice, mirroring P8-18).
//   - T8.18.4: "implementation context correctly scoped" — the captured
//     status and artifacts fields (and only those) render verbatim on a
//     dedicated TASK_CONTEXT component followed by a fixed scoping note:
//     "scope: validate this work unit only; report findings via the
//     response protocol; the OVERSEER owns workflow transitions." The
//     note is the fixed boundary: the TESTER validates and REPORTS; the
//     WORKER is never told to implement unrelated fixes (WBS 8.18: "Do
//     not tell TESTER to implement unrelated fixes unless workflow
//     explicitly uses that behavior" — this workflow does not use it).
//     When both status and artifacts are empty the component is absent
//     entirely (zero components, no failure — the "when available" half).
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the
// active unit is REQUIRED — a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective, propagated from the P8-14
// work-unit composition). A cleared response-protocol version (<= 0) is a
// composition failure (ErrMissingProtocolVersion, the P8-23 contract).

// ImplementationStatusInput carries the captured implementation
// status/artifacts fed to ComposeValidationPrompt (WBS 8.18 "implementation
// status/artifacts when available"): the worker-reported status and the
// artifact identifiers produced by the implementation.
type ImplementationStatusInput struct {
	Status    string
	Artifacts []string
}

// ValidationPromptInput carries the caller-supplied captured values for
// ComposeValidationPrompt (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline): the active work unit, the captured test state, the
// captured implementation status/artifacts (when available), and the
// validation response schema's identity/version.
type ValidationPromptInput struct {
	ActiveUnit      WorkUnitContextInput
	TestState       string
	Implementation  ImplementationStatusInput
	ProtocolID      uuid.UUID
	ProtocolVersion int
}

// ComposeValidationPrompt renders the VALIDATE_WORK_UNIT prompt
// components (WBS 8.18): the work unit (P8-14 — carries the acceptance
// criteria and required tests), the test state (T8.18.3), the
// implementation context correctly scoped (T8.18.4), and the validation
// response schema (P8-23 — T8.18.5). It consumes ONLY the
// caller-supplied captured values — no "latest" input — so repeated
// composition over the same input is byte-identical (the Phase 8
// determinism requirement).
//
// The active unit is REQUIRED: a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective). The response schema's
// cleared version fails via the P8-23 contract (ErrMissingProtocolVersion).
func ComposeValidationPrompt(in ValidationPromptInput) (Components, error) {
	unit, err := NewWorkUnitContext(in.ActiveUnit)
	if err != nil {
		return nil, err
	}
	// T8.18.1/T8.18.2: the acceptance criteria and required tests ride on
	// the composed work-unit component (WBS 8.14 — rendered verbatim,
	// captured order).
	unitComps, err := ComposeWorkUnit(unit)
	if err != nil {
		return nil, err
	}

	out := make(Components, 0, 4)
	out = append(out, unitComps...)

	// T8.18.3: the captured test state — one TEST_CONTEXT component when
	// available; an empty capture composes to zero components (no
	// failure, no placeholder).
	if strings.TrimSpace(in.TestState) != "" {
		c, err := NewPromptComponent(
			ComponentTestContext,
			SourceStateFile,
			0,
			in.TestState,
			uuid.Nil,
			0,
			"",
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	// T8.18.4: the implementation context — the captured status and
	// artifacts (and only those) on one TASK_CONTEXT component, followed
	// by the fixed scoping note. Both empty -> no component (the "when
	// available" half). The scoping note keeps the TESTER in a validate-
	// and-report role; it never delegates unrelated-fix work.
	if strings.TrimSpace(in.Implementation.Status) != "" || len(in.Implementation.Artifacts) > 0 {
		var b strings.Builder
		if strings.TrimSpace(in.Implementation.Status) != "" {
			b.WriteString("implementation status: " + in.Implementation.Status + "\n")
		}
		if len(in.Implementation.Artifacts) > 0 {
			b.WriteString("artifacts: " + strings.Join(in.Implementation.Artifacts, "; ") + "\n")
		}
		b.WriteString("scope: validate this work unit only; report findings via the response protocol; the OVERSEER owns workflow transitions.")
		c, err := NewPromptComponent(
			ComponentTaskContext,
			SourceWorkUnit,
			0,
			b.String(),
			unit.ID,
			0,
			"",
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	// T8.18.5: the validation response schema (P8-23).
	schema, err := ComposeValidationResponseSchema(in.ProtocolID, in.ProtocolVersion)
	if err != nil {
		return nil, err
	}
	out = append(out, schema...)
	return out, nil
}
