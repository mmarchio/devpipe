package wbs

import (
	"testing"

	"github.com/google/uuid"
)

// T2.12.1 — Valid work unit created.
func TestT2121ValidWorkUnitCreated(t *testing.T) {
	wu, err := NewWorkUnit(uuid.New(), 1, "Implement email validation", "ATOMIC")
	if err != nil {
		t.Fatalf("valid work unit rejected: %v", err)
	}
	if wu.ID == uuid.Nil {
		t.Fatal("work unit has no ID")
	}
	if wu.ExecutionPlanID == uuid.Nil {
		t.Fatal("work unit lost its plan ID")
	}
	if wu.Sequence != 1 || wu.Name != "Implement email validation" {
		t.Fatalf("fields not set: %+v", wu)
	}
	if wu.Kind != WorkUnitKindATOMIC {
		t.Fatalf("kind %v, want ATOMIC", wu.Kind)
	}
}

// T2.12.2 — Work unit requires execution plan.
func TestT2122RequiresPlan(t *testing.T) {
	if _, err := NewWorkUnit(uuid.Nil, 1, "n", "ATOMIC"); err == nil {
		t.Fatal("nil plan accepted")
	}
}

// T2.12.3 — Sequence must be positive.
func TestT2123SequencePositive(t *testing.T) {
	if _, err := NewWorkUnit(uuid.New(), 0, "n", "ATOMIC"); err == nil {
		t.Fatal("sequence 0 accepted")
	}
	if _, err := NewWorkUnit(uuid.New(), -3, "n", "ATOMIC"); err == nil {
		t.Fatal("sequence -3 accepted")
	}
}

// T2.12.4 — Unknown kind rejected.
func TestT2124UnknownKindRejected(t *testing.T) {
	if _, err := NewWorkUnit(uuid.New(), 1, "n", "BOGUS"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, k := range []string{"ATOMIC", "COMPOSITION", "INTEGRATION", "VALIDATION"} {
		if _, err := ParseWorkUnitKind(k); err != nil {
			t.Fatalf("kind %s rejected: %v", k, err)
		}
	}
}

// T2.12.5 — Unknown status rejected.
func TestT2125UnknownStatusRejected(t *testing.T) {
	if _, err := ParseWorkUnitStatus("BOGUS"); err == nil {
		t.Fatal("unknown status accepted")
	}
	for _, s := range []string{"PENDING", "READY", "EXECUTING", "INCOMPLETE", "VALIDATING", "DONE", "FAILED", "BLOCKED"} {
		if _, err := ParseWorkUnitStatus(s); err != nil {
			t.Fatalf("status %s rejected: %v", s, err)
		}
	}
}

// T2.12.6 — Initial status defaults appropriately.
func TestT2126InitialStatusDefaults(t *testing.T) {
	wu, err := NewWorkUnit(uuid.New(), 1, "n", WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("valid work unit rejected: %v", err)
	}
	if wu.Status != WorkUnitStatusPENDING {
		t.Fatalf("initial status %v, want PENDING", wu.Status)
	}
	if wu.StartedAt != nil || wu.CompletedAt != nil {
		t.Fatalf("start/complete not nil: %+v", wu)
	}
	if wu.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}
}
