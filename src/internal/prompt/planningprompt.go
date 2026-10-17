package prompt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// P8-24 — WBS 8.16 planning (PHASE_PLAN) prompt composition.
//
// ComposePlanningPrompt renders the planning-state prompt components built
// on the P8-15..P8-19 and P8-22/P8-23 primitives: a planning-configuration
// component (the current WBS version, the planning policy, and the project
// granularity setting — the WBS 8.16 "project context / planning policy"
// inputs) and the P8-23 planning response schema (the structured planning
// response requirements). The composition consumes ONLY caller-supplied
// configuration values (the P8-22/P8-23 captured-values discipline) — the
// WBS version, policy, granularity, and work-unit kinds come in from the
// caller, never from a hard-coded list — so repeated composition over the
// same input is byte-identical.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - The current WBS version is recorded as a "WBS v<N>" line in the
//     content (worker-visible) and on the configuration component's
//     SourceVersion (traceable).
//   - The planning policy is rendered verbatim; an empty policy renders the
//     explicit "(not configured)" marker (the non-atomic-project case).
//   - The project granularity setting is rendered verbatim and honored —
//     never overridden by an implementation default (WBS 8.16: planning
//     granularity remains project configuration).
//   - The planning configuration component carries NO work-unit content (the
//     planner CREATES units; it does not consume a captured unit), so no
//     work-unit field labels appear in it.
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): a cleared
// WBS version (<= 0) is a composition FAILURE (ErrMissingWBSVersion). An
// empty policy or granularity is NOT a failure — both are project
// configuration values a project may leave unset (rendered with an explicit
// marker), mirroring the WBS 8.16 conditional "for projects configured for
// atomic planning, include the configured policy". An empty work-unit-kind
// capture fails via the P8-23 planning-response-schema contract.

// ErrMissingWBSVersion reports a composition failure: the planning
// composition was supplied no usable current WBS version (WBS 8.16,
// T8.16.1 — the P8-10/P8-11/P8-22 cleared-captured-value failure contract:
// a cleared version is a failure, not a silent fallback to a default).
var ErrMissingWBSVersion = errors.New("prompt: planning composition carries no captured WBS version")

// PlanningPromptInput carries the caller-supplied planning-configuration
// values for ComposePlanningPrompt (the P8-22/P8-23 captured-values
// discipline): the current WBS version, the planning policy, the project
// granularity setting, the work-unit kinds the response schema renders, and
// the response-protocol identity/version the schema pins.
type PlanningPromptInput struct {
	WBSVersion      int
	PlanningPolicy  string
	Granularity     string
	WorkUnitKinds   []string
	ProtocolID      uuid.UUID
	ProtocolVersion int
}

// ComposePlanningPrompt renders the PHASE_PLAN prompt components (WBS
// 8.16): a planning-configuration component and the P8-23 planning response
// schema. It consumes ONLY the caller-supplied configuration values — no
// "latest" input, no hard-coded list — so repeated composition over the
// same input is byte-identical (the Phase 8 determinism requirement).
func ComposePlanningPrompt(in PlanningPromptInput) (Components, error) {
	if in.WBSVersion <= 0 {
		return nil, fmt.Errorf("planning: %w", ErrMissingWBSVersion)
	}
	policy := in.PlanningPolicy
	if strings.TrimSpace(policy) == "" {
		policy = "(not configured)"
	}
	granularity := in.Granularity
	if strings.TrimSpace(granularity) == "" {
		granularity = "(not configured)"
	}
	var b strings.Builder
	b.WriteString("WBS v" + strconv.Itoa(in.WBSVersion) + "\n")
	b.WriteString("planning policy: " + policy + "\n")
	b.WriteString("planning granularity (project setting): " + granularity + "\n")
	cfg, err := NewPromptComponent(
		ComponentPhaseContext,
		SourceSystemDefault,
		0,
		b.String(),
		uuid.Nil,
		in.WBSVersion,
		"",
	)
	if err != nil {
		return nil, err
	}
	schema, err := ComposePlanningResponseSchema(in.ProtocolID, in.ProtocolVersion, in.WorkUnitKinds)
	if err != nil {
		return nil, err
	}
	out := make(Components, 0, len(schema)+1)
	out = append(out, cfg)
	out = append(out, schema...)
	return out, nil
}
