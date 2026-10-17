package prompt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// P8-23 — WBS 8.41/8.42/8.43 per-state response schema composition.
//
// Each function renders its state's required response schema as exactly
// one RESPONSE_PROTOCOL component, built on the P8-22 response-protocol
// composition: the captured protocol version is recorded in the content
// and on the component's provenance, and the fixed transition-authority
// note is appended so the model REPORTS its state while the overseer
// decides workflow transitions. Composition consumes ONLY the
// caller-supplied configuration values (work-unit kinds, allowed
// statuses) — there is no "latest" input and no hard-coded list — and
// repeated composition over the same input is byte-identical. Phase 8
// includes the schema/instructions only; parsing/acceptance occurs
// later.

// ErrMissingWorkUnitKinds reports a composition failure: the planning
// composition was supplied no work-unit kinds from configuration (the
// P8-10/P8-11 cleared-captured-value failure contract — no silent
// fallback to a hard-coded list).
var ErrMissingWorkUnitKinds = errors.New("prompt: planning response schema carries no configured work-unit kinds")

// ErrMissingAllowedStatuses reports a composition failure: the
// implementation composition was supplied no allowed statuses from
// configuration (same cleared-captured-value contract).
var ErrMissingAllowedStatuses = errors.New("prompt: implementation response schema carries no captured allowed statuses")

// ComposePlanningResponseSchema renders the planning-state response
// schema (WBS 8.41) as exactly one RESPONSE_PROTOCOL component: the
// required unit fields (id, kind, objective, dependencies, acceptance,
// tests, constraints — each marked required), with the work-unit kinds
// rendered from the caller-supplied configuration in captured order
// (T8.41.1–T8.41.5). An empty kind capture is a composition failure
// (ErrMissingWorkUnitKinds); a cleared captured protocol version is a
// failure (ErrMissingProtocolVersion — the P8-22 contract).
func ComposePlanningResponseSchema(protocolID uuid.UUID, capturedVersion int, workUnitKinds []string) (Components, error) {
	if capturedVersion <= 0 {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingProtocolVersion)
	}
	if len(workUnitKinds) == 0 {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingWorkUnitKinds)
	}
	kindsList := strings.Join(workUnitKinds, " | ")
	schema := "Planning response schema (required fields for every unit):\n" +
		`{
  "phase": "<phase>",
  "units": [
    {
      "id": "<unit id, required>",
      "kind": "` + kindsList + ` (required; one of the configured work-unit kinds)",
      "objective": "<required>",
      "dependencies": ["<unit ids, required>"],
      "acceptance": ["<acceptance criterion, required>"],
      "tests": ["<test, required>"],
      "constraints": ["<constraint, required>"]
    }
  ]
}`
	content := "Response protocol v" + strconv.Itoa(capturedVersion) + " (required response schema; work-unit kinds as configured):\n" +
		schema + "\n" + transitionAuthorityNote
	c, err := NewPromptComponent(
		ComponentResponseProtocol,
		SourceResponseProtocol,
		0,
		content,
		protocolID,
		capturedVersion,
		ContentHash(schema),
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}

// ComposeImplementationResponseSchema renders the implementation-state
// response schema (WBS 8.42) as exactly one RESPONSE_PROTOCOL
// component: a required status field whose allowed values are the
// caller-supplied statuses (joined as the WBS 8.24 enumeration), a
// required summary, and the remaining/completed/blocking/next_action
// fields (T8.42.1–T8.42.5). The schema supports the next_action field
// without any directive that tells the worker to select/choose the next
// WBS unit — the transition-authority note (T8.24.3) is the only
// instruction, and it names the overseer as the transition authority.
// An empty allowed-status capture is a composition failure
// (ErrMissingAllowedStatuses); a cleared captured version is a failure
// (ErrMissingProtocolVersion).
func ComposeImplementationResponseSchema(protocolID uuid.UUID, capturedVersion int, allowedStatuses []string) (Components, error) {
	if capturedVersion <= 0 {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingProtocolVersion)
	}
	if len(allowedStatuses) == 0 {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingAllowedStatuses)
	}
	statusSet := strings.Join(allowedStatuses, "|")
	schema := "Implementation response schema:\n" +
		`{
  "status": "` + statusSet + `" (required; one of the allowed statuses above),
  "summary": "<required, per protocol>",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "<describe the next action without selecting the next unit>"
}`
	content := "Response protocol v" + strconv.Itoa(capturedVersion) + " (required response schema; allowed status values as defined by the schema's status field):\\n" +
		schema + "\\n" + transitionAuthorityNote
	c, err := NewPromptComponent(
		ComponentResponseProtocol,
		SourceResponseProtocol,
		0,
		content,
		protocolID,
		capturedVersion,
		ContentHash(schema),
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}

// ComposeValidationResponseSchema renders the validation-state response
// schema (WBS 8.43) as exactly one RESPONSE_PROTOCOL component: a
// status field carrying pass|fail, a required summary, and the
// criteria/tests/failures result fields (T8.43.1–T8.43.5). The schema
// is fixed (no caller-supplied status capture) so no allowed-statuses
// failure path exists; a cleared captured version is a failure
// (ErrMissingProtocolVersion).
func ComposeValidationResponseSchema(protocolID uuid.UUID, capturedVersion int) (Components, error) {
	if capturedVersion <= 0 {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingProtocolVersion)
	}
	schema := "Validation response schema:\n" +
		`{
  "status": "pass|fail" (required),
  "summary": "<required>",
  "criteria": [],
  "tests": [],
  "failures": []
}`
	content := "Response protocol v" + strconv.Itoa(capturedVersion) + " (required response schema; status is pass|fail):\\n" +
		schema + "\\n" + transitionAuthorityNote
	c, err := NewPromptComponent(
		ComponentResponseProtocol,
		SourceResponseProtocol,
		0,
		content,
		protocolID,
		capturedVersion,
		ContentHash(schema),
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}
