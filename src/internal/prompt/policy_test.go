package prompt

import (
	"testing"

	"github.com/google/uuid"
)

// T8.30.1 — Component policy persisted: an explicitly chosen policy is
// retained on the component (in-domain, pure per PLAN — no DB half).
func TestT8301_PolicyRetained(t *testing.T) {
	for _, want := range []CompressionPolicy{PolicyPreserveExact, PolicyPreserveSemantics, PolicyCompressible, PolicyOptional} {
		comp, err := NewPromptComponent(ComponentTaskContext, SourceWorkUnit, 1, "content", uuid.Nil, 3, "h")
		if err != nil {
			t.Fatalf("NewPromptComponent: %v", err)
		}
		comp.Policy = want
		if comp.Policy != want {
			t.Fatalf("policy not retained: got %q want %q", comp.Policy, want)
		}
	}
}

// T8.30.1b — unknown policy rejected by the setter (construction check
// mirrors T8.8.5's unknown-type rejection).
func TestT8301_UnknownPolicyRejected(t *testing.T) {
	comp, err := NewPromptComponent(ComponentTaskContext, SourceWorkUnit, 1, "c", uuid.Nil, 1, "h")
	if err != nil {
		t.Fatalf("NewPromptComponent: %v", err)
	}
	if err := comp.SetPolicy(CompressionPolicy("BOGUS")); err == nil {
		t.Fatal("SetPolicy: expected error for unknown policy, got nil")
	}
	if comp.Policy != PolicyPreserveExact {
		t.Fatalf("rejected setter must not mutate: got %q", comp.Policy)
	}
}

// T8.30.2 — Acceptance criteria default exact. DefaultPolicy applies the
// WBS 8.30 example mapping: identity-bearing classes (work-unit context
// carrying acceptance criteria / identifiers, required tests) default to
// PRESERVE_EXACT.
func TestT8302_AcceptanceCriteriaDefaultExact(t *testing.T) {
	if got := DefaultPolicy(ComponentTaskContext); got != PolicyPreserveExact {
		t.Fatalf("task context (acceptance criteria) default: got %q want PRESERVE_EXACT", got)
	}
}

// T8.30.3 — Required tests default exact.
func TestT8303_RequiredTestsDefaultExact(t *testing.T) {
	if got := DefaultPolicy(ComponentTestContext); got != PolicyPreserveExact {
		t.Fatalf("test context (required tests) default: got %q want PRESERVE_EXACT", got)
	}
}

// T8.30.4 — Work-unit identifiers default exact: the work-unit source and
// the task context both carry unit identity.
func TestT8304_WorkUnitIdentifierDefaultExact(t *testing.T) {
	for _, typ := range []PromptComponentType{ComponentTaskContext, ComponentTestContext} {
		comp, err := NewPromptComponent(typ, SourceWorkUnit, 1, "unit-123", uuid.Nil, 1, "h")
		if err != nil {
			t.Fatalf("NewPromptComponent(%s): %v", typ, err)
		}
		if comp.Policy != PolicyPreserveExact {
			t.Fatalf("%s component: default policy %q, want PRESERVE_EXACT", typ, comp.Policy)
		}
	}
}

// T8.30.5 — Policy survives composition: ByOrder (a copy/reorder) must
// retain every component's policy, byte-identical.
func TestT8305_PolicySurvivesComposition(t *testing.T) {
	comps := Components{
		{Type: ComponentTestContext, Order: 3, Content: "tests", Policy: PolicyPreserveExact},
		{Type: ComponentProjectContext, Order: 1, Content: "history", Policy: PolicyCompressible},
		{Type: ComponentTaskContext, Order: 2, Content: "criteria", Policy: PolicyPreserveExact},
		{Type: ComponentContinuationContext, Order: 4, Content: "state", Policy: PolicyOptional},
	}
	got := comps.ByOrder()
	if len(got) != len(comps) {
		t.Fatalf("ByOrder length: got %d want %d", len(got), len(comps))
	}
	// Sorted by Order: 1=history, 2=criteria, 3=tests, 4=state.
	want := []struct {
		content string
		policy  CompressionPolicy
	}{
		{"history", PolicyCompressible},
		{"criteria", PolicyPreserveExact},
		{"tests", PolicyPreserveExact},
		{"state", PolicyOptional},
	}
	for i, c := range got {
		if c.Policy != want[i].policy || c.Content != want[i].content {
			t.Fatalf("ByOrder[%d]: got (%q, %q) want (%q, %q)", i, c.Content, c.Policy, want[i].content, want[i].policy)
		}
	}
	// Determinism: repeated composition is byte-identical.
	again := comps.ByOrder()
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("ByOrder not deterministic at %d", i)
		}
	}
}
