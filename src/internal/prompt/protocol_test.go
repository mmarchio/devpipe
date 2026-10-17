package prompt

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var protocolTS = time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)

var (
	protocolA = uuid.MustParse("a0000000-0000-0000-0000-000000000001")
	stateX    = uuid.MustParse("b0000000-0000-0000-0000-000000000001")
)

const (
	protocolV1Schema = `{
  "status": "complete|incomplete|blocked|failed",
  "summary": "...",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "..."
}`
	protocolV2Schema = protocolV1Schema + "\n// v2: added explicit status enumeration\n"
)

// T8.23.1 — Protocol persisted (pure half): the model carries the full
// protocol content byte-for-byte.
func TestT8231ProtocolPersistedPure(t *testing.T) {
	p, err := NewResponseProtocolSeries(ResponseProtocolInput{
		Kind:    KindPlanResponse,
		Name:    "Worker Protocol",
		Content: protocolV1Schema,
		Ts:      protocolTS,
	})
	if err != nil {
		t.Fatalf("NewResponseProtocolSeries: %v", err)
	}
	if p.Kind != KindPlanResponse {
		t.Fatalf("kind = %q", p.Kind)
	}
	cur, err := p.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur.Content != protocolV1Schema {
		t.Fatalf("protocol content mangled:\n got %q\nwant %q", cur.Content, protocolV1Schema)
	}
}

// T8.23.2 — Protocol versioned: initial v1, edit -> v2, historical
// retrievable, current advances.
func TestT8232ProtocolVersioned(t *testing.T) {
	p, err := NewResponseProtocolSeries(ResponseProtocolInput{
		Kind: KindImplementationResponse, Name: "impl", Content: protocolV1Schema, Ts: protocolTS,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := p.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("initial version = %d, want 1", v1.Version)
	}

	v2, err := p.Append(protocolV2Schema, protocolTS)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("edit version = %d, want 2", v2.Version)
	}

	hist, err := p.GetVersion(1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if hist.Content != protocolV1Schema {
		t.Fatalf("historical v1 = %q, want original", hist.Content)
	}
	if _, err := p.GetVersion(3); err == nil {
		t.Fatalf("unknown version must be rejected")
	}

	cur, err := p.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur.Version != 2 {
		t.Fatalf("current = %d, want 2", cur.Version)
	}
}

// T8.23.3 — State can reference a protocol: by protocol identity + version.
func TestT8233StateCanReferenceProtocol(t *testing.T) {
	p, err := NewResponseProtocolSeries(ResponseProtocolInput{
		Kind: KindValidationResponse, Name: "val", Content: protocolV1Schema, Ts: protocolTS,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ref, err := NewStateProtocolReference(stateX, p.ID, 1)
	if err != nil {
		t.Fatalf("NewStateProtocolReference: %v", err)
	}
	if err := ref.Validate(p); err != nil {
		t.Fatalf("Validate against owning protocol: %v", err)
	}

	// A different protocol must be rejected.
	other, err := NewResponseProtocolSeries(ResponseProtocolInput{
		Kind: KindReviewResponse, Name: "rev", Content: protocolV1Schema, Ts: protocolTS,
	})
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := ref.Validate(other); err == nil {
		t.Fatalf("cross-protocol reference must be rejected")
	}

	// An unsatisfied version (protocol has only v1) must be rejected.
	ref2, err := NewStateProtocolReference(stateX, p.ID, 2)
	if err != nil {
		t.Fatalf("NewStateProtocolReference v2: %v", err)
	}
	if err := ref2.Validate(p); err == nil {
		t.Fatalf("reference to a version the protocol lacks must be rejected")
	}

	// A reference with no protocol id is invalid at construction.
	if _, err := NewStateProtocolReference(stateX, uuid.Nil, 1); err == nil {
		t.Fatalf("nil protocol id must be rejected")
	}
	if _, err := NewStateProtocolReference(stateX, p.ID, 0); err == nil {
		t.Fatalf("version 0 must be rejected")
	}
}

// T8.23.4 — Historical protocol retained: a version referenced by a state
// stays readable after the protocol advances.
func TestT8234HistoricalProtocolRetained(t *testing.T) {
	p, err := NewResponseProtocolSeries(ResponseProtocolInput{
		Kind: KindPlanResponse, Name: "plan", Content: protocolV1Schema, Ts: protocolTS,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ref, err := NewStateProtocolReference(stateX, p.ID, 1)
	if err != nil {
		t.Fatalf("NewStateProtocolReference: %v", err)
	}
	if _, err := p.Append(protocolV2Schema, protocolTS); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// The state's pinned v1 is still readable after the advance.
	got, err := ref.Resolved(p)
	if err != nil {
		t.Fatalf("Resolved after advance: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("resolved version = %d, want the pinned 1", got.Version)
	}
	if got.Content != protocolV1Schema {
		t.Fatalf("historical content = %q, want the v1 schema", got.Content)
	}

	// A reference to the new version resolves to the new content — the
	// reference is by (protocol, version), never "latest".
	refV2, err := NewStateProtocolReference(stateX, p.ID, 2)
	if err != nil {
		t.Fatalf("NewStateProtocolReference v2: %v", err)
	}
	got2, err := refV2.Resolved(p)
	if err != nil {
		t.Fatalf("Resolved v2: %v", err)
	}
	if got2.Version != 2 || got2.Content != protocolV2Schema {
		t.Fatalf("v2 reference resolved to v=%d content=%q", got2.Version, got2.Content)
	}
}
