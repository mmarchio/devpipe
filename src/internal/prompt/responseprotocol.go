package prompt

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// ErrMissingProtocolVersion reports a composition failure: the
// snapshot-captured response protocol carries no usable version or
// content (WBS 8.24; the P8-10/P8-11 failure contract chosen for this
// task — a cleared capture is a failure, not a silent fallback).
var ErrMissingProtocolVersion = errors.New("prompt: snapshot carries no captured response protocol version or content")

// transitionAuthorityNote is the fixed WBS 8.24 transition-authority
// wording (T8.24.3): it lets the model REPORT its status and names the
// overseer as the workflow-transition authority. It deliberately
// contains no directive that tells the model to advance phases or to
// choose/select its own next state — the model reports state, it does
// not control workflow state.
const transitionAuthorityNote = "Report your status using the response schema below. The overseer decides workflow transitions from your report; do not advance phases or choose your own next state."

// ComposeResponseProtocol renders the snapshot-captured response
// protocol (WBS 8.24) as exactly one RESPONSE_PROTOCOL component:
// the required schema (the captured protocol content, byte-for-byte)
// and the allowed status values it carries (T8.24.1/T8.24.2), the
// fixed transition-authority note (T8.24.3), and the captured protocol
// version recorded in the content (T8.24.4).
//
// The composition consumes ONLY the captured values: the caller
// supplies the protocol content at the snapshot's pinned version
// (captures like StateProtocolReference carry no content — the pinned
// version row does), so a later protocol edit is invisible (there is
// no "latest" input to this function). A cleared captured version
// (<= 0) or empty captured content is a composition failure
// (ErrMissingProtocolVersion — the P8-10/P8-11 contract; the WBS 8.24
// task-level choice). Provenance: SourceVersion is the captured
// version, SourceID the protocol id, and the source hash is retained
// on the component. Repeated composition over the same input is
// byte-identical.
func ComposeResponseProtocol(protocolID uuid.UUID, capturedVersion int, capturedContent string) (Components, error) {
	if capturedVersion <= 0 || capturedContent == "" {
		return nil, fmt.Errorf("protocol %s: %w", protocolID, ErrMissingProtocolVersion)
	}
	content := "Response protocol v" + strconv.Itoa(capturedVersion) + " (required response schema; allowed status values as defined by the schema's status field):\n" +
		capturedContent + "\n" +
		transitionAuthorityNote
	c, err := NewPromptComponent(
		ComponentResponseProtocol,
		SourceResponseProtocol,
		0,
		content,
		protocolID,
		capturedVersion,
		ContentHash(capturedContent),
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}
