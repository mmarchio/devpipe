package prompt

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-22 — WBS 8.24 response-protocol composition, TDD T8.24.1-T8.24.4.
//
// Composition consumes ONLY the snapshot-captured protocol (version +
// content supplied by the caller at the pinned version — the capture
// carries no content), never "latest", and renders the protocol's
// required schema + allowed status values as a RESPONSE_PROTOCOL
// component whose provenance records the captured version (T8.24.4).
//
// Transition authority (T8.24.3, WBS 8.24 principle: "The model reports
// state; the model does not control workflow state"): the render carries
// a fixed transition-authority note that tells the worker to REPORT its
// status through the schema while the OVERSEER decides transitions. The
// chosen pin: the note must name the overseer as the workflow-transition
// authority, and the render must contain no directive that tells the
// model to advance phases or choose its own next state (the test scans
// for a bounded set of delegation phrases). A cleared captured version
// is a composition FAILURE (ErrMissingProtocolVersion) — the P8-10/P8-11
// failure contract (this task's per-section choice), unlike P8-12.

// p822Schema is the WBS 8.23 initial worker protocol schema, used as the
// captured protocol content in these tests.
const p822Schema = `{
  "status": "complete|incomplete|blocked|failed",
  "summary": "...",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "..."
}`

// T8.24.1 — Required response schema included: the composition renders
// exactly one RESPONSE_PROTOCOL component whose content carries the
// captured protocol content VERBATIM (the schema is not paraphrased,
// trimmed, or re-keyed).
func TestT8241_RequiredSchemaIncluded(t *testing.T) {
	comps, err := ComposeResponseProtocol(uuid.New(), 3, p822Schema)
	if err != nil {
		t.Fatalf("ComposeResponseProtocol: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 RESPONSE_PROTOCOL component, got %d", len(comps))
	}
	c := comps[0]
	if c.Type != ComponentResponseProtocol {
		t.Fatalf("want RESPONSE_PROTOCOL, got %s", c.Type)
	}
	if c.Source != SourceResponseProtocol {
		t.Fatalf("want RESPONSE_PROTOCOL source, got %s", c.Source)
	}
	if !strings.Contains(c.Content, p822Schema) {
		t.Fatalf("captured schema not included verbatim; content:\n%s", c.Content)
	}
}

// T8.24.2 — Allowed status values included: the rendered content names
// every value of the schema's status field, spelled exactly.
func TestT8242_AllowedStatusValuesIncluded(t *testing.T) {
	comps, err := ComposeResponseProtocol(uuid.New(), 1, p822Schema)
	if err != nil {
		t.Fatalf("ComposeResponseProtocol: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 component, got %d", len(comps))
	}
	for _, status := range []string{"complete", "incomplete", "blocked", "failed"} {
		if !strings.Contains(comps[0].Content, status) {
			t.Fatalf("allowed status %q not included in render", status)
		}
	}
}

// T8.24.3 — Workflow transition authority not delegated to worker: the
// render lets the model REPORT a status, and the fixed transition-
// authority note names the OVERSEER as the workflow-transition
// authority (the model reports state; it does not control workflow
// state — WBS 8.24). A bounded set of delegation phrases that would
// hand transition authority to the worker must be absent from the
// render.
func TestT8243_TransitionAuthorityNotDelegated(t *testing.T) {
	comps, err := ComposeResponseProtocol(uuid.New(), 2, p822Schema)
	if err != nil {
		t.Fatalf("ComposeResponseProtocol: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 component, got %d", len(comps))
	}
	note := strings.ToLower(comps[0].Content)
	if !strings.Contains(note, "overseer") {
		t.Fatalf("transition-authority note must name the overseer as the workflow-transition authority; content:\n%s", comps[0].Content)
	}
	for _, delegated := range []string{
		"you may advance",
		"you may choose",
		"you may select",
		"you decide the next",
		"you control the workflow",
		"advance the phase",
		"choose the next state",
		"select the next state",
	} {
		if strings.Contains(note, delegated) {
			t.Fatalf("render delegates workflow transition authority to the worker (contains %q)", delegated)
		}
	}
}

// T8.24.4 — Correct protocol version recorded: the component's
// SourceVersion carries the CAPTURED version (the snapshot pin), the
// protocol id is the SourceID, and the rendered content records the
// same captured version. A later current version has no input to the
// composition.
func TestT8244_CorrectVersionRecorded(t *testing.T) {
	pid := uuid.New()
	for _, want := range []int{1, 2, 7} {
		comps, err := ComposeResponseProtocol(pid, want, p822Schema)
		if err != nil {
			t.Fatalf("ComposeResponseProtocol(v%d): %v", want, err)
		}
		c := comps[0]
		if c.SourceVersion != want {
			t.Fatalf("SourceVersion = %d, want captured %d", c.SourceVersion, want)
		}
		if c.SourceID != pid {
			t.Fatalf("SourceID = %s, want %s", c.SourceID, pid)
		}
		if !strings.Contains(c.Content, "v"+strconv.Itoa(want)) {
			t.Fatalf("render must record the captured version v%d; content:\n%s", want, c.Content)
		}
	}
}

// The missing-captured-content policy (this task's choice — the P8-10/
// P8-11 failure contract): a cleared captured version (zero) or empty
// captured content is a composition FAILURE with the typed sentinel
// ErrMissingProtocolVersion (no silent fallback to any default schema).
func TestP822_MissingCapturedProtocolIsFailure(t *testing.T) {
	pid := uuid.New()
	if _, err := ComposeResponseProtocol(pid, 0, p822Schema); !errors.Is(err, ErrMissingProtocolVersion) {
		t.Fatalf("cleared captured version err = %v, want ErrMissingProtocolVersion", err)
	}
	if _, err := ComposeResponseProtocol(pid, 2, ""); !errors.Is(err, ErrMissingProtocolVersion) {
		t.Fatalf("empty captured content err = %v, want ErrMissingProtocolVersion", err)
	}
}

// Determinism: repeated composition over the same captured input is
// byte-identical (the composition-input requirement shared with the
// other P8 composition tasks).
func TestP822_RepeatedCompositionByteIdentical(t *testing.T) {
	pid := uuid.New()
	first, err := ComposeResponseProtocol(pid, 3, p822Schema)
	if err != nil {
		t.Fatalf("ComposeResponseProtocol: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ComposeResponseProtocol(pid, 3, p822Schema)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) || again[0] != first[0] {
			t.Fatalf("repeated composition not byte-identical at %d", i)
		}
	}
}
