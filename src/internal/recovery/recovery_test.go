package recovery

import (
	"encoding/json"
	"reflect"
	"testing"
)

// T18.7 — unknown worker delivery is never SAFE_TO_RETRY, and no plan for
// it offers or recommends a resend.
func TestT187UnknownDeliveryNeverRetry(t *testing.T) {
	for _, c := range []Condition{DeliveryUnknown, ResponseUnpersisted, InflightDispatch, Panic} {
		p := PlanFor(Facts{ExecutionID: "e", Condition: c})
		if p.Classification == SafeToRetry || p.Safe(ActResend) || p.RecommendedAutomatic == ActResend {
			t.Fatalf("%s: %+v", c, p)
		}
	}
	for _, c := range Conditions() {
		p := PlanFor(Facts{Condition: c})
		if p.Classification == "" {
			t.Fatalf("%s unclassified", c)
		}
		for _, u := range p.UnsafeActions {
			if p.Safe(u) {
				t.Fatalf("%s: %s both safe and unsafe", c, u)
			}
		}
		if p.RecommendedAutomatic != "" && !p.Safe(p.RecommendedAutomatic) {
			t.Fatalf("%s recommends an action that is not safe", c)
		}
		if p.RequiresApproval && p.RecommendedAutomatic != "" {
			t.Fatalf("%s needs approval but recommends automation", c)
		}
	}
	if PlanFor(Facts{Condition: "SOMETHING_NEW"}).Classification != ManualReviewRequired {
		t.Fatal("unknown condition not conservative")
	}
}

// T18.8 — the same durable evidence produces the same plan.
func TestT188Deterministic(t *testing.T) {
	f := Facts{ExecutionID: "e1", Condition: DeliveryUnknown, Evidence: map[string]any{"b": 2, "a": "x", "head": "abc"}}
	g := Facts{ExecutionID: "e1", Condition: DeliveryUnknown, Evidence: map[string]any{"head": "abc", "a": "x", "b": 2}}
	if PlanFor(f).Hash() != PlanFor(g).Hash() {
		t.Fatal("plan hash depends on evidence order")
	}
	g.Evidence["head"] = "def"
	if PlanFor(f).Hash() == PlanFor(g).Hash() {
		t.Fatal("different evidence, same hash")
	}
}

// T18.9 — round-trip persistence.
func TestT189RoundTrip(t *testing.T) {
	p := PlanFor(Facts{ExecutionID: "e", Condition: PendingCheckpoint, Evidence: map[string]any{"commit": "abc", "n": 2.0}})
	b, _ := json.Marshal(p)
	var q Plan
	if err := json.Unmarshal(b, &q); err != nil || !reflect.DeepEqual(p, q) || q.Hash() != p.Hash() {
		t.Fatalf("round trip: %+v", q)
	}
}

// T18.16 — every dispatch state maps to a safe classification; only a
// provably unsent dispatch may be resent.
func TestT1816DispatchStates(t *testing.T) {
	cases := []struct {
		status, delivery string
		phase            DispatchPhase
		class            Classification
	}{
		{"PENDING", "", PhasePrepared, SafeToRetry},
		{"FAILED", "DEFINITELY_NOT_SENT", PhasePrepared, SafeToRetry},
		{"SENDING", "", PhaseSendStarted, ReconciliationNeeded},
		{"WAITING", "", PhaseDelivered, ReconciliationNeeded},
		{"FAILED", "DELIVERY_UNKNOWN", PhaseDeliveryUnknown, ReconciliationNeeded},
		{"UNKNOWN", "DELIVERY_UNKNOWN", PhaseDeliveryUnknown, ReconciliationNeeded},
		{"FAILED", "", PhaseDeliveryUnknown, ReconciliationNeeded},
		{"COMPLETED", "SENT_RESPONSE_RECEIVED", PhaseResponseReceived, SafeToResume},
	}
	for _, c := range cases {
		ph := DispatchPhaseOf(c.status, c.delivery)
		cond := ConditionForDispatch(ph)
		if ph != c.phase || ClassOf(cond) != c.class {
			t.Fatalf("%s/%s → %s %s", c.status, c.delivery, ph, ClassOf(cond))
		}
		if PlanFor(Facts{Condition: cond}).Safe(ActResend) != (ph == PhasePrepared) {
			t.Fatalf("%s resend safety wrong", ph)
		}
	}
}

// T18.19 — DELIVERY_UNKNOWN ≠ FAILED: an unknown delivery maps to its own
// condition requiring human approval.
func TestT1819DeliveryUnknownDistinct(t *testing.T) {
	p := PlanFor(Facts{Condition: ConditionForDispatch(DispatchPhaseOf("FAILED", "DELIVERY_UNKNOWN"))})
	if p.Condition != DeliveryUnknown || !p.RequiresApproval || p.RecommendedAutomatic != "" {
		t.Fatalf("%+v", p)
	}
}

// T18.222 (table) — every operation × location has exactly one entry.
func TestT18222CrashMatrixComplete(t *testing.T) {
	seen := map[string]int{}
	for _, c := range CrashMatrix() {
		seen[string(c.Operation)+"|"+string(c.Location)]++
		if c.Recovery == "" || (c.Outcome != ConvergesOnce && c.Outcome != StopsDeliveryUnknown) {
			t.Fatalf("incomplete case %+v", c)
		}
	}
	for _, op := range CrashOperations() {
		for _, loc := range CrashLocations() {
			if seen[string(op)+"|"+string(loc)] != 1 {
				t.Fatalf("%s × %s: %d entries", op, loc, seen[string(op)+"|"+string(loc)])
			}
		}
	}
}
