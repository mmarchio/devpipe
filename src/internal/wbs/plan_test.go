package wbs

import (
	"testing"

	"github.com/google/uuid"
)

// T2.11.1 — Plan requires phase.
func TestT2111PlanRequiresPhase(t *testing.T) {
	if _, err := NewExecutionPlan(uuid.Nil, 1); err == nil {
		t.Fatal("nil phase accepted")
	}
}

// T2.11.2 — Plan records source WBS version.
func TestT2112RecordsWBSVersion(t *testing.T) {
	p, err := NewExecutionPlan(uuid.New(), 3)
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if p.WBSVersion != 3 {
		t.Fatalf("WBSVersion %d, want 3", p.WBSVersion)
	}
	if p.Version != 1 {
		t.Fatalf("plan version %d, want 1", p.Version)
	}
	if p.Status != StatusREADY {
		t.Fatalf("status %v, want READY", p.Status)
	}
}

// T2.11.3 — Plan version persisted (field present and positive).
func TestT2113VersionPositive(t *testing.T) {
	p, _ := NewExecutionPlan(uuid.New(), 1)
	if p.Version <= 0 {
		t.Fatalf("version %d, want > 0", p.Version)
	}
	if _, err := NewExecutionPlan(uuid.New(), 0); err == nil {
		t.Fatal("wbs version 0 accepted")
	}
	if _, err := NewExecutionPlan(uuid.New(), -1); err == nil {
		t.Fatal("wbs version -1 accepted")
	}
}

// T2.11.4 — Invalid status rejected.
func TestT2114InvalidStatusRejected(t *testing.T) {
	if _, err := ParsePlanStatus("BOGUS"); err == nil {
		t.Fatal("unknown status accepted")
	}
	for _, valid := range []string{"GENERATING", "READY", "EXECUTING", "COMPLETED", "SUPERSEDED", "FAILED"} {
		if _, err := ParsePlanStatus(valid); err != nil {
			t.Fatalf("status %s rejected: %v", valid, err)
		}
	}
}
