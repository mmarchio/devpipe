package wbs

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// P4-13 (WBS 4.14): staleness derivation unit tests. Staleness is always
// DERIVED from the active plan's stored source version vs the current WBS
// version — never persisted, so a WBS update marks the previous plan
// stale on the same tick.

func newStalenessPlan(t *testing.T, wbsVersion int) *ExecutionPlan {
	t.Helper()
	p, err := NewExecutionPlan(uuid.New(), wbsVersion)
	if err != nil {
		t.Fatalf("NewExecutionPlan: %v", err)
	}
	_ = time.Now()
	return p
}

// No plan for the phase → Unplanned, whatever the WBS state.
func TestPlanStalenessNoPlanUnplanned(t *testing.T) {
	if got := PlanStaleness(nil, 3); got != PlanStaleUnplanned {
		t.Fatalf("PlanStaleness(nil, 3) = %v, want %v", got, PlanStaleUnplanned)
	}
	if got := PlanStaleness(nil, 0); got != PlanStaleUnplanned {
		t.Fatalf("PlanStaleness(nil, 0) = %v, want %v", got, PlanStaleUnplanned)
	}
}

// Plan bound to the current WBS version → Current.
func TestPlanStalenessSameVersionCurrent(t *testing.T) {
	for _, v := range []int{1, 2, 7} {
		if got := PlanStaleness(newStalenessPlan(t, v), v); got != PlanStaleCurrent {
			t.Fatalf("PlanStaleness(v%d plan, current v%d) = %v, want %v", v, v, got, PlanStaleCurrent)
		}
	}
}

// Plan bound to an older WBS version → Stale.
func TestPlanStalenessOlderVersionStale(t *testing.T) {
	if got := PlanStaleness(newStalenessPlan(t, 2), 3); got != PlanStaleStale {
		t.Fatalf("PlanStaleness(v2 plan, current v3) = %v, want %v", got, PlanStaleStale)
	}
	if got := PlanStaleness(newStalenessPlan(t, 1), 5); got != PlanStaleStale {
		t.Fatalf("PlanStaleness(v1 plan, current v5) = %v, want %v", got, PlanStaleStale)
	}
}

// Updating the WBS immediately marks the previous plan stale — the same
// plan object with only the current version changed flips Current → Stale,
// with no persisted state.
func TestPlanStalenessDerivedOnSameTick(t *testing.T) {
	plan := newStalenessPlan(t, 2)
	if got := PlanStaleness(plan, 2); got != PlanStaleCurrent {
		t.Fatalf("before WBS update: got %v, want %v", got, PlanStaleCurrent)
	}
	// The WBS is updated to v3; the plan row is untouched (WBS 4.13) and
	// the derivation is re-run.
	if got := PlanStaleness(plan, 3); got != PlanStaleStale {
		t.Fatalf("after WBS update: got %v, want %v", got, PlanStaleStale)
	}
}
