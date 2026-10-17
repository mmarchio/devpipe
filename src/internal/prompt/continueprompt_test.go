package prompt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// P8-28 — WBS 8.21 same-session continuation (CONTINUE_WORK_UNIT) prompt
// composition tests.
//
// The composition is minimal (WBS 8.21): the captured worker status (the
// P8-08 ContinuationStatus — identity + remaining work) and the
// implementation response schema (P8-23). It consumes ONLY the
// caller-supplied captured values (the P8-16..P8-19 / P8-22/P8-23
// captured-values discipline) — no "latest" input — and repeated
// composition over the same input is byte-identical. Phase 8 includes the
// schema/instructions only; parsing/acceptance occurs later.
//
// Chosen composition (stated for TESTER/PM to verify):
//   - T8.21.1: the captured status fields (summary, completed, remaining,
//     blocking, next action) render verbatim (captured order) on exactly one
//     CONTINUATION_CONTEXT component.
//   - T8.21.2: the work-unit identity (the status's WorkUnitID) is named in
//     the render AND is the continuation component's SourceID provenance.
//   - T8.21.3: the remaining work (the status's Remaining list) renders
//     verbatim (captured order) as the "remaining:" section.
//   - T8.21.4: the response protocol is included as a RESPONSE_PROTOCOL
//     component (the P8-23 implementation response schema).
//   - T8.21.5: the prompt does not accidentally select the next task — a
//     fixed "complete the remaining acceptance criteria" line (a "next
//     task" pin) names the CURRENT unit's remaining work; the delegated-
//     selection phrasing is absent (bounded absence-scan); and the input
//     carries no next-task channel at all.

// contUnitID is the fixture's FIXED continued-unit id (deterministic
// composition input; a fresh uuid per construction would change the
// rendered ID).
var contUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p828-unit-a"))

// contProtocolID is the fixture's FIXED protocol id (deterministic
// composition input).
var contProtocolID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p828-protocol"))

// contInput is the canonical continue-prompt input: a captured worker status
// associated with the continued unit (identity + remaining work) and the
// implementation response schema's captured identity/version + allowed
// statuses.
func contInput() ContinuePromptInput {
	return ContinuePromptInput{
		Status: ContinuationStatus{
			WorkUnitID: contUnitID,
			Summary:    "Schema drafted; persistence pending",
			Completed:  []string{"domain types"},
			Remaining:  []string{"append repository", "advisory-locked MAX+1"},
			Blocking:   []string{"none"},
			NextAction: "Implement the append repository",
			CapturedAt: time.Unix(1, 0).UTC(),
		},
		ProtocolID:      contProtocolID,
		ProtocolVersion: 3,
		AllowedStatuses: []string{"DONE", "INCOMPLETE", "BLOCKED"},
	}
}

// contContinuationComponent returns the composed continuation component
// (the one whose provenance is the CONTINUATION_STATUS source).
func contContinuationComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Type == ComponentContinuationContext {
			return c
		}
	}
	t.Fatalf("no continuation component in continue composition")
	return PromptComponent{}
}

// contFullRender returns the concatenation of every component's content, so
// exclusion/presence assertions scan the whole render (not just the
// continuation component).
func contFullRender(t *testing.T, comps Components) string {
	t.Helper()
	var b strings.Builder
	for _, c := range comps {
		b.WriteString(c.Content)
		b.WriteByte(0) // unit separator between components
	}
	return b.String()
}

// T8.21.1 — Captured status included: the status summary, completed,
// blocking, and next-action values render verbatim on exactly one
// CONTINUATION_CONTEXT component (captured order).
func TestT8211_CapturedStatusIncluded(t *testing.T) {
	comps, err := ComposeContinuePrompt(contInput())
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	var contComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentContinuationContext {
			contComps = append(contComps, c)
		}
	}
	if len(contComps) != 1 {
		t.Fatalf("want exactly 1 CONTINUATION_CONTEXT component, got %d", len(contComps))
	}
	cc := contComps[0]
	for _, marker := range []string{
		"Schema drafted; persistence pending", // summary
		"domain types",                        // completed
		"none",                                // blocking
		"Implement the append repository",     // next action
	} {
		if !strings.Contains(cc.Content, marker) {
			t.Fatalf("captured status field %q missing from render:\n%s", marker, cc.Content)
		}
	}
}

// T8.21.2 — Work-unit identity included: the status's WorkUnitID is named
// in the render AND is the continuation component's SourceID provenance; a
// different continued unit yields a different render.
func TestT8212_WorkUnitIdentityIncluded(t *testing.T) {
	comps, err := ComposeContinuePrompt(contInput())
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	cc := contContinuationComponent(t, comps)
	if cc.SourceID != contUnitID {
		t.Fatalf("continuation component SourceID = %s, want %s", cc.SourceID, contUnitID)
	}
	if !strings.Contains(cc.Content, contUnitID.String()) {
		t.Fatalf("render missing continued unit ID; content:\n%s", cc.Content)
	}
	// A distinct continued unit renders with that unit's id, not the other's.
	in := contInput()
	in.Status.WorkUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p828-unit-other"))
	comps2, err := ComposeContinuePrompt(in)
	if err != nil {
		t.Fatalf("ComposeContinuePrompt(other): %v", err)
	}
	cc2 := contContinuationComponent(t, comps2)
	if !strings.Contains(cc2.Content, in.Status.WorkUnitID.String()) {
		t.Fatalf("render missing other unit ID; content:\n%s", cc2.Content)
	}
	if strings.Contains(cc2.Content, contUnitID.String()) {
		t.Fatalf("stale continued unit ID present; content:\n%s", cc2.Content)
	}
}

// T8.21.3 — Remaining work included: the status's Remaining list renders
// verbatim (captured order) as the "remaining:" section of the continuation
// component.
func TestT8213_RemainingWorkIncluded(t *testing.T) {
	comps, err := ComposeContinuePrompt(contInput())
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	cc := contContinuationComponent(t, comps)
	i1 := strings.Index(cc.Content, "append repository")
	i2 := strings.Index(cc.Content, "advisory-locked MAX+1")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("remaining work missing from render:\n%s", cc.Content)
	}
	if i1 > i2 {
		t.Fatalf("remaining work order not preserved:\n%s", cc.Content)
	}
}

// T8.21.4 — Response protocol included: the implementation response schema
// is composed (P8-23) as a RESPONSE_PROTOCOL component; the continuing
// worker reports in the required format.
func TestT8214_ResponseProtocolIncluded(t *testing.T) {
	comps, err := ComposeContinuePrompt(contInput())
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	var protoComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentResponseProtocol {
			protoComps = append(protoComps, c)
		}
	}
	if len(protoComps) != 1 {
		t.Fatalf("want exactly 1 RESPONSE_PROTOCOL component, got %d", len(protoComps))
	}
	pc := protoComps[0]
	if pc.SourceID != contProtocolID {
		t.Fatalf("response-protocol component SourceID = %s, want %s", pc.SourceID, contProtocolID)
	}
	if pc.SourceVersion != 3 {
		t.Fatalf("response-protocol component SourceVersion = %d, want 3", pc.SourceVersion)
	}
	if !strings.Contains(pc.Content, "Implementation response schema") {
		t.Fatalf("response protocol missing schema; content:\n%s", pc.Content)
	}
}

// T8.21.5 — Prompt does not accidentally select next task: a fixed "complete
// the remaining acceptance criteria" line (a "next task" pin) tells the
// worker to finish the CURRENT unit's remaining work; the delegated-
// selection phrasing is ABSENT from the whole render (bounded absence-scan);
// and the input carries no next-task channel at all (the compile-level guard
// below). The fixed transition-authority note (carried by the response
// schema) names the overseer as the workflow-transition authority.
func TestT8215_DoesNotSelectNextTask(t *testing.T) {
	comps, err := ComposeContinuePrompt(contInput())
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	render := strings.ToLower(contFullRender(t, comps))
	// Positive: the "next task" pin names finishing the CURRENT unit's
	// remaining work.
	if !strings.Contains(render, "complete the remaining acceptance criteria") {
		t.Fatalf("render missing the complete-remaining-acceptance pin:\n%s", render)
	}
	// Positive: the transition-authority note (the model reports state, does
	// not control workflow state) is present.
	if !strings.Contains(render, "the overseer decides workflow transitions") {
		t.Fatalf("render missing the transition-authority note:\n%s", render)
	}
	// Negative: the bounded list of delegated-selection phrases that must be
	// ABSENT — the worker reports state, the overseer selects the next task.
	for _, phrase := range []string{
		"you may advance",
		"you may choose",
		"you may select",
		"advance the phase",
		"choose the next state",
		"select the next unit",
		"choose the next unit",
		"move to the next work unit",
		"start the next work unit",
	} {
		if strings.Contains(render, phrase) {
			t.Fatalf("delegated-selection phrase %q present in render:\n%s", phrase, render)
		}
	}
}

// Compile-level guard (the T8.17.5/T8.14.6 channel-absence class):
// ComposeContinuePrompt's input shape is exactly ContinuePromptInput — the
// captured status and the schema's captured identity/version/statuses — with
// NO next-task channel. Adding one breaks this build.
var _ = func(in ContinuePromptInput) (Components, error) { return ComposeContinuePrompt(in) }

// Failure contract (the P8-10/P8-11 choice): the captured status must be
// addressable to a work unit — a cleared work-unit association (a zero
// WorkUnitID) fails the composition with the typed sentinel (no silent
// fallback).
func TestP828_ClearedContinuationUnitIsFailure(t *testing.T) {
	in := contInput()
	in.Status.WorkUnitID = uuid.Nil
	if _, err := ComposeContinuePrompt(in); !errors.Is(err, ErrMissingContinuationUnit) {
		t.Fatalf("cleared work-unit association err = %v, want ErrMissingContinuationUnit", err)
	}
}

// Failure contract: a cleared response-protocol version (<= 0) is a
// composition failure (the P8-23 contract propagated through the
// composition).
func TestP828_ClearedProtocolVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := contInput()
		in.ProtocolVersion = v
		if _, err := ComposeContinuePrompt(in); !errors.Is(err, ErrMissingProtocolVersion) {
			t.Fatalf("cleared protocol version %d err = %v, want ErrMissingProtocolVersion", v, err)
		}
	}
}

// Failure contract: an empty allowed-status capture is a composition
// failure (the P8-23 implementation-response-schema contract).
func TestP828_EmptyAllowedStatusesIsFailure(t *testing.T) {
	in := contInput()
	in.AllowedStatuses = nil
	if _, err := ComposeContinuePrompt(in); !errors.Is(err, ErrMissingAllowedStatuses) {
		t.Fatalf("empty statuses err = %v, want ErrMissingAllowedStatuses", err)
	}
}

// Determinism: repeated composition over the same input is byte-identical
// (the composition-input requirement shared with the other P8 tasks).
func TestP828_RepeatedCompositionByteIdentical(t *testing.T) {
	in := contInput()
	first, err := ComposeContinuePrompt(in)
	if err != nil {
		t.Fatalf("ComposeContinuePrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposeContinuePrompt(in)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(got) != len(first) {
			t.Fatalf("repeat %d: component count %d, want %d", i, len(got), len(first))
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("repeat %d: component %d not byte-identical", i, j)
			}
		}
	}
}
