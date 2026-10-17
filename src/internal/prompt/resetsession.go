package prompt

import (
	"github.com/google/uuid"

	"overseer/internal/role"
	"overseer/internal/statefile"
)

// P8-29 — WBS 8.22 reset-session continuation (CONTINUE_WORK_UNIT) prompt
// composition.
//
// ComposeResetSessionPrompt renders the FULL reset-session continuation
// components (WBS 8.22): after a context-reset the new worker session has
// none of the prior conversation, so the prompt restores everything it needs
// to resume: the full relevant role context (the captured system prompt,
// P8-16, and the captured role instructions, P8-17), the architecture/state
// files (P8-19), the current project/phase/task state — the SAME work unit
// (P8-14, the ACTIVE_TASK), the captured continuation status (the P8-08
// ContinuationStatus, whose WorkUnitID is the continued unit's identity and
// whose Remaining list is the remaining work), a continuation-identification
// instruction, and the implementation response schema (P8-23).
//
// The composition consumes ONLY the caller-supplied captured values (the
// P8-16..P8-19 / P8-22/P8-23 captured-values discipline) — no "latest" input,
// no conversation-history channel — so repeated composition over the same
// input is byte-identical (the Phase 8 determinism requirement).
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - T8.22.1: the SAME work unit is included — the active work unit is
//     composed via ComposeWorkUnit (P8-14) with the continued unit's ID, and
//     the continuation component names that same unit; the work-unit
//     component's SourceID pins the continued unit.
//   - T8.22.2: the full required state context is restored — the captured
//     system prompt (P8-16), the captured role instructions (P8-17), and the
//     architecture/state files (P8-19) are all composed, so the new session
//     has the complete role context without any prior conversation.
//   - T8.22.3: the captured status is included — the status summary,
//     completed, blocking, and next-action values render verbatim (captured
//     order) on exactly one CONTINUATION_CONTEXT component.
//   - T8.22.4: the remaining work is included — the status's Remaining list
//     renders verbatim (captured order) as the "remaining:" section of the
//     continuation component.
//   - T8.22.5: the prompt has no dependency on previous conversation content
//     — the input carries NO conversation-history/transcript channel at all
//     (compile-level guard), and a bounded absence-scan pins that the render
//     carries no conversation-reliance phrasing.
//   - T8.22.6: the prompt clearly identifies this as a continuation, not a
//     fresh implementation — a fixed continuation-identification marker
//     ("This is a continuation of work unit <id> — resume the in-progress
//     work, do not start fresh") is rendered in the continuation component.
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the captured
// status must be addressable to a work unit (a cleared WorkUnitID fails —
// ErrMissingContinuationUnit). A cleared captured system-prompt version
// fails (ErrMissingSystemPromptVersion, the P8-16 contract); a cleared
// captured work-unit objective fails (ErrMissingWorkUnitObjective, the
// P8-14 contract); a cleared response-protocol version (<= 0) and an empty
// allowed-status capture fail (ErrMissingProtocolVersion /
// ErrMissingAllowedStatuses, the P8-23 contract).

// ResetSessionPromptInput carries the caller-supplied captured values for
// ComposeResetSessionPrompt (the P8-16..P8-19 / P8-22/P8-23 captured-values
// discipline): the role snapshot (system prompt + instructions), the pinned
// state files (+ their required keys), the current work unit (the
// ACTIVE_TASK), the captured continuation status, and the implementation
// response schema's captured identity/version + allowed statuses. There is
// deliberately NO conversation-history/transcript field (T8.22.5 — the
// prompt must not rely on prior conversation content).
type ResetSessionPromptInput struct {
	Role       role.RoleSnapshot
	StateFiles []statefile.StateFileSnapshot
	Required   []string

	ActiveUnit WorkUnitContextInput

	Status ContinuationStatus

	ProtocolID      uuid.UUID
	ProtocolVersion int
	AllowedStatuses []string
}

// ComposeResetSessionPrompt renders the reset-session CONTINUE_WORK_UNIT
// prompt components (WBS 8.22): the full role context (captured system
// prompt + role instructions), the architecture/state files, the current
// work unit (the ACTIVE_TASK), the captured continuation status, and the
// implementation response schema. It consumes ONLY the caller-supplied
// captured values — no "latest" input, no conversation channel — so
// repeated composition over the same input is byte-identical.
//
// The components are appended in the WBS 8.9 canonical relative order (system
// prompt, role instructions, state files, task context, continuation
// context, response protocol). The captured status is REQUIRED: a cleared
// work-unit association fails the composition (ErrMissingContinuationUnit).
func ComposeResetSessionPrompt(in ResetSessionPromptInput) (Components, error) {
	st := in.Status
	if st.WorkUnitID == uuid.Nil {
		return nil, ErrMissingContinuationUnit
	}

	out := make(Components, 0, 8)

	// T8.22.2: the full role context — the exact captured system prompt
	// (P8-16). A cleared captured version fails here.
	sys, err := ResolveSystemPrompt(in.Role)
	if err != nil {
		return nil, err
	}
	out = append(out, sys)

	// T8.22.2: the role's assigned instructions (P8-17), in captured
	// assignment order.
	insComps, err := ComposeRoleInstructions(in.Role)
	if err != nil {
		return nil, err
	}
	out = append(out, insComps...)

	// T8.22.2: the architecture/state files (P8-19), at their captured
	// versions, in deterministic (section, type-key, id) order.
	fileComps, err := ComposeStateFiles(in.StateFiles, in.Required)
	if err != nil {
		return nil, err
	}
	out = append(out, fileComps...)

	// T8.22.1: the current work unit (the ACTIVE_TASK) — the SAME unit the
	// continuation status is associated with (P8-14).
	unit, err := NewWorkUnitContext(in.ActiveUnit)
	if err != nil {
		return nil, err
	}
	unitComps, err := ComposeWorkUnit(unit)
	if err != nil {
		return nil, err
	}
	out = append(out, unitComps...)

	// T8.22.3/T8.22.4/T8.22.6: the captured continuation status (identity,
	// reported fields, remaining work) plus the continuation-identification
	// marker.
	cc, err := newResetContinuationComponent(st)
	if err != nil {
		return nil, err
	}
	out = append(out, cc)

	// The implementation response schema (P8-23) — the continuing worker
	// reports through the implementation protocol.
	schema, err := ComposeImplementationResponseSchema(in.ProtocolID, in.ProtocolVersion, in.AllowedStatuses)
	if err != nil {
		return nil, err
	}
	out = append(out, schema...)

	return out, nil
}

// newResetContinuationComponent renders the reset-session continuation
// component (T8.22.3/T8.22.4/T8.22.6): a continuation-identification marker
// naming the continued unit, the captured status fields (summary, completed,
// remaining, blocking, next action) verbatim in captured order, and a resume
// instruction. The captured status's WorkUnitID is the component's SourceID
// provenance.
func newResetContinuationComponent(st ContinuationStatus) (PromptComponent, error) {
	// T8.22.6: the continuation-identification marker — this is a
	// continuation of the named unit, and the worker resumes the in-progress
	// work rather than starting fresh. P14-24: delimited authoritative
	// instruction + escaped prior worker report.
	content := renderContinuation(st,
		"This is a continuation of work unit "+st.WorkUnitID.String()+" — resume the in-progress work, do not start fresh.",
		"Captured status before the session reset:",
		"Resume from this captured status and complete the remaining acceptance criteria using the required response format.")
	return NewPromptComponent(
		ComponentContinuationContext,
		SourceContinuation,
		0,
		content,
		st.WorkUnitID,
		0,
		"",
	)
}
