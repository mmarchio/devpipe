package recovery

import (
	"strings"
	"testing"

	"overseer/internal/resilience"
)

// T18.211 — the planner agrees with the decision table for every row.
func TestT18211PolicyMatchesTable(t *testing.T) {
	reg := resilience.MustDefault()
	for _, r := range DecisionTable() {
		if r.RetryPolicy != "" {
			if _, ok := reg.Policy(r.RetryPolicy); !ok {
				t.Fatalf("%s: unknown policy %s", r.Situation, r.RetryPolicy)
			}
		}
		if r.Condition == "" {
			continue
		}
		p := PlanFor(Facts{Condition: r.Condition})
		switch r.AutoRetry {
		case RetryNo, RetryNoResend:
			if p.Classification == SafeToRetry || p.Safe(ActResend) {
				t.Fatalf("%s: plan retries: %+v", r.Situation, p)
			}
		case RetryYes, RetryBounded:
			if p.Classification != SafeToRetry || p.RecommendedAutomatic == "" {
				t.Fatalf("%s: plan does not retry: %+v", r.Situation, p)
			}
		case RetryIfRepeatable:
			if p.RecommendedAutomatic == "" {
				t.Fatalf("%s: no automatic repeat", r.Situation)
			}
		}
		switch r.HumanReview {
		case ReviewNo:
			if p.RequiresApproval {
				t.Fatalf("%s: requires approval", r.Situation)
			}
		case ReviewReconcile:
			if !p.RequiresApproval || p.Classification != ReconciliationNeeded {
				t.Fatalf("%s: not reconcile + review", r.Situation)
			}
		}
	}
	if !strings.Contains(DecisionTableMarkdown(), "| Delivery unknown | DELIVERY_UNKNOWN | No | No | Yes/reconcile | — |") {
		t.Fatal(DecisionTableMarkdown())
	}
}

// T18.223 — no external operation lacks failure semantics, and only
// operations that are safe to retry carry a retry policy.
func TestT18223SideEffectMatrix(t *testing.T) {
	want := map[string]bool{"Worker dispatch": true, "Compression call": true, "Validation command": true, "Git checkpoint": true, "Filesystem materialization": true}
	reg := resilience.MustDefault()
	for _, s := range SideEffectMatrix() {
		delete(want, s.Operation)
		if s.SafeToRetry == "" || s.Ambiguity == "" {
			t.Fatalf("%s incomplete", s.Operation)
		}
		if s.SafeToRetry == RetryNever && s.RetryPolicy != "" {
			t.Fatalf("%s: never-retry op has a policy", s.Operation)
		}
		if s.RetryPolicy != "" {
			if _, ok := reg.Policy(s.RetryPolicy); !ok {
				t.Fatalf("%s: unknown policy", s.Operation)
			}
		}
		if !s.Idempotent && s.SafeToRetry == RetryAlways {
			t.Fatalf("%s: non-idempotent op always retried", s.Operation)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing operations %v", want)
	}
	if !strings.Contains(SideEffectMarkdown(), "Worker dispatch | no | yes | yes | ONLY_IF_PROVABLY_NOT_PERFORMED") {
		t.Fatal(SideEffectMarkdown())
	}
}
