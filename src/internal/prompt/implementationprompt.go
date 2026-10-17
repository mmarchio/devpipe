package prompt

import (
	"github.com/google/uuid"
)

// P8-25 — WBS 8.17 implementation (IMPLEMENT_WORK_UNIT) prompt composition.
//
// ComposeImplementationPrompt renders the implementation-state prompt
// components built on the P8-14 (work unit), P8-15 (dependency contracts),
// and P8-23 (implementation response schema) primitives: the active work
// unit, the active unit's dependency contracts, and the implementation
// response schema. The composition consumes ONLY the caller-supplied
// captured values (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline) — no "latest" input — so repeated composition over the
// same input is byte-identical.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - The active unit is composed via ComposeWorkUnit (WBS 8.14): its ID,
//     objective, dependencies, acceptance criteria, required tests, and
//     constraints appear verbatim on the composed work-unit component (the
//     "active work unit / acceptance criteria / required tests" sections).
//   - The active unit's dependency contracts are composed via
//     ComposeDependencyContracts (WBS 8.15): only the contracts the unit's
//     captured dependency set references are included (a referenced contract
//     is "explicitly needed"; an unreferenced one is excluded).
//   - A "completion target:" line naming the active unit is appended to the
//     work-unit component's content, so a worker can identify the EXACT
//     completion target (the active unit + its acceptance criteria + its
//     required tests) without unrelated conversation history. The line
//     names the ACTIVE unit — a future unit is never composed (T8.17.5).
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the active
// unit is REQUIRED — a cleared captured objective fails the composition
// (ErrMissingWorkUnitObjective, propagated from the P8-14 work-unit
// composition). A cleared response-protocol version (<= 0) and an empty
// allowed-status capture are composition failures (ErrMissingProtocolVersion
// / ErrMissingAllowedStatuses, the P8-23 contract).

// ImplementationPromptInput carries the caller-supplied captured values for
// ComposeImplementationPrompt (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline): the active work unit, the dependency contracts available to
// the active unit (only the referenced ones are composed), and the
// implementation response schema's identity/version + allowed statuses.
type ImplementationPromptInput struct {
	ActiveUnit          WorkUnitContextInput
	DependencyContracts []DependencyContract
	ProtocolID          uuid.UUID
	ProtocolVersion     int
	AllowedStatuses     []string
}

// ComposeImplementationPrompt renders the IMPLEMENT_WORK_UNIT prompt
// components (WBS 8.17): the active work unit (P8-14), the active unit's
// dependency contracts (P8-15), and the implementation response schema
// (P8-23). It consumes ONLY the caller-supplied captured values — no
// "latest" input, no hard-coded list — so repeated composition over the
// same input is byte-identical (the Phase 8 determinism requirement).
//
// The active unit is REQUIRED: a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective). The response schema's cleared
// version / empty statuses fail via the P8-23 contract. Future work units
// (not the active unit, not an explicit dependency contract) are never
// composed — they are excluded from the render (T8.17.5).
func ComposeImplementationPrompt(in ImplementationPromptInput) (Components, error) {
	unit, err := NewWorkUnitContext(in.ActiveUnit)
	if err != nil {
		return nil, err
	}
	// Compose the active unit (WBS 8.14). ComposeWorkUnit returns exactly one
	// work-unit component.
	unitComps, err := ComposeWorkUnit(unit)
	if err != nil {
		return nil, err
	}
	// Append the "completion target:" line (T8.17.6) to the work-unit
	// component's content, so the exact completion target is identifiable:
	// the active unit + its acceptance criteria + its required tests.
	unitComps[0].Content += "\ncompletion target: " +
		"complete work unit " + unit.ID.String() +
		" — all acceptance criteria met and all required tests passing."
	// Compose the active unit's dependency contracts (WBS 8.15): only the
	// contracts whose dependency name is in the unit's captured dependency
	// set are included (the "unless explicitly needed" half of T8.17.5).
	depComps, err := ComposeDependencyContracts(unit.Dependencies, in.DependencyContracts)
	if err != nil {
		return nil, err
	}
	// Compose the implementation response schema (P8-23).
	schema, err := ComposeImplementationResponseSchema(in.ProtocolID, in.ProtocolVersion, in.AllowedStatuses)
	if err != nil {
		return nil, err
	}
	out := make(Components, 0, len(unitComps)+len(depComps)+len(schema))
	out = append(out, unitComps...)
	out = append(out, depComps...)
	out = append(out, schema...)
	return out, nil
}
