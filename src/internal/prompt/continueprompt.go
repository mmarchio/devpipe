package prompt

import (
	"errors"

	"github.com/google/uuid"
)

// P8-28 — WBS 8.21 same-session continuation (CONTINUE_WORK_UNIT) prompt
// composition.
//
// ComposeContinuePrompt renders the minimal same-session continuation
// components (WBS 8.21): the captured worker status (the P8-08
// ContinuationStatus — its work-unit association is the work-unit
// identity, and its remaining list is the remaining work), and the
// implementation response schema (P8-23 ComposeImplementationResponseSchema
// — the continuing worker reports through the implementation protocol).
// The composition is deliberately MINIMAL (WBS 8.21: "send a minimal
// continuation prompt rather than rebuilding all context") — it does not
// re-compose the full work unit, state files, or role context. It consumes
// ONLY the caller-supplied captured values (the P8-16..P8-19 / P8-22/P8-23
// captured-values discipline) — no "latest" input — so repeated composition
// over the same input is byte-identical.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - T8.21.1: the captured status is included — the status summary,
//     completed, remaining, blocking, and next-action values are rendered
//     verbatim (captured order) on exactly one CONTINUATION_CONTEXT
//     component.
//   - T8.21.2: the work-unit identity is included — the status's work-unit
//     association (WorkUnitID) names the continued unit in the render AND
//     is the component's SourceID provenance.
//   - T8.21.3: the remaining work is included — the status's Remaining list
//     renders verbatim (captured order) as the "remaining:" section of the
//     continuation component.
//   - T8.21.4: the response protocol is included — the implementation
//     response schema is composed (P8-23) as a RESPONSE_PROTOCOL component,
//     so the continuing worker reports in the required format.
//   - T8.21.5: the prompt does not accidentally select the next task —
//     CHOSEN POLICY: a fixed "complete the remaining acceptance criteria"
//     line (a "next task" pin) tells the worker to finish the CURRENT unit's
//     remaining work; the delegated-selection phrasing is absent (bounded
//     absence-scan), and the fixed transition-authority note (carried by the
//     response schema) names the overseer as the workflow-transition
//     authority. The input carries no next-task channel at all.
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the captured
// status must be addressable to a work unit — a cleared work-unit
// association (a zero WorkUnitID) fails the composition (ErrMissingContinuationUnit).
// A cleared response-protocol version (<= 0) and an empty allowed-status
// capture are composition failures (ErrMissingProtocolVersion /
// ErrMissingAllowedStatuses — the P8-23 contract, propagated).

// ErrMissingContinuationUnit reports a composition failure: the captured
// continuation status carries no work-unit association, so the continued
// unit cannot be identified (the P8-10/P8-11 cleared-captured-value failure
// contract — no silent fallback).
var ErrMissingContinuationUnit = errors.New("prompt: continuation status carries no work-unit association")

// ContinuePromptInput carries the caller-supplied captured values for
// ComposeContinuePrompt (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline): the captured worker status (the P8-08 ContinuationStatus —
// its WorkUnitID is the continued unit's identity, its Remaining list is
// the remaining work) and the implementation response schema's
// identity/version + allowed statuses. There is deliberately NO next-task /
// next-unit field (T8.21.5 — the prompt does not select the next task).
type ContinuePromptInput struct {
	Status          ContinuationStatus
	ProtocolID      uuid.UUID
	ProtocolVersion int
	AllowedStatuses []string
}

// ComposeContinuePrompt renders the CONTINUE_WORK_UNIT prompt components
// (WBS 8.21): the captured worker status (the continued unit's identity +
// the remaining work) and the implementation response schema (P8-23). It
// consumes ONLY the caller-supplied captured values — no "latest" input, no
// re-composition of the full context — so repeated composition over the
// same input is byte-identical (the Phase 8 determinism requirement).
//
// The captured status is REQUIRED: a cleared work-unit association fails the
// composition (ErrMissingContinuationUnit). The response schema's cleared
// version / empty statuses fail via the P8-23 contract. The next work unit is
// never composed — the input carries no channel for it (T8.21.5).
func ComposeContinuePrompt(in ContinuePromptInput) (Components, error) {
	st := in.Status
	if st.WorkUnitID == uuid.Nil {
		return nil, ErrMissingContinuationUnit
	}

	// T8.21.1/T8.21.2/T8.21.3: the captured status — the continued unit's
	// identity (WorkUnitID), the reported status fields, and the remaining
	// work (Remaining) — on exactly one CONTINUATION_CONTEXT component.
	// P14-24: delimited authoritative instruction + escaped prior worker report
	// (the "next task" pin, T8.21.5, is the closing instruction).
	content := renderSameSessionContinuation(st)
	cc, err := NewPromptComponent(
		ComponentContinuationContext,
		SourceContinuation,
		0,
		content,
		st.WorkUnitID,
		0,
		"",
	)
	if err != nil {
		return nil, err
	}

	// T8.21.4: the implementation response schema (P8-23) — the continuing
	// worker reports through the implementation protocol.
	schema, err := ComposeImplementationResponseSchema(in.ProtocolID, in.ProtocolVersion, in.AllowedStatuses)
	if err != nil {
		return nil, err
	}

	out := make(Components, 0, 1+len(schema))
	out = append(out, cc)
	out = append(out, schema...)
	return out, nil
}
