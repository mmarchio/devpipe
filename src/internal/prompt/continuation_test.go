package prompt

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// T8.20.1 — Continuation status carries the WBS 8.20 fields (pure): every
// field round-trips verbatim.
func TestT8201_ContinuationStatusCarriesFields(t *testing.T) {
	unit := uuid.New()
	captured := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	in := ContinuationStatusInput{
		WorkUnitID: unit,
		Summary:    "half done",
		Completed:  []string{"a", "b"},
		Remaining:  []string{"c", "d"},
		Blocking:   []string{"dep-missing"},
		NextAction: "write tests",
		CapturedAt: captured,
	}
	got, err := NewContinuationStatus(in)
	if err != nil {
		t.Fatalf("NewContinuationStatus: %v", err)
	}
	if got.WorkUnitID != in.WorkUnitID {
		t.Fatalf("WorkUnitID: got %s want %s", got.WorkUnitID, in.WorkUnitID)
	}
	if got.Summary != in.Summary {
		t.Fatalf("Summary: got %q want %q", got.Summary, in.Summary)
	}
	if got.NextAction != in.NextAction {
		t.Fatalf("NextAction: got %q want %q", got.NextAction, in.NextAction)
	}
	if !got.CapturedAt.Equal(in.CapturedAt) {
		t.Fatalf("CapturedAt: got %v want %v", got.CapturedAt, in.CapturedAt)
	}
}

// T8.20.2 — Association to the owning work unit validated: the status reports
// association to its own unit and passes validation; a status with no work
// unit is rejected at construction.
func TestT8202_OwningWorkUnitAssociationValidated(t *testing.T) {
	unit := uuid.New()
	got, err := NewContinuationStatus(ContinuationStatusInput{WorkUnitID: unit})
	if err != nil {
		t.Fatalf("NewContinuationStatus: %v", err)
	}
	if !got.AssociatedTo(unit) {
		t.Fatal("status must be associated with its owning work unit")
	}
	if err := got.ValidateAssociation(unit); err != nil {
		t.Fatalf("ValidateAssociation(owning unit): %v", err)
	}
	if _, err := NewContinuationStatus(ContinuationStatusInput{WorkUnitID: uuid.Nil}); err == nil {
		t.Fatal("NewContinuationStatus: expected rejection of a status with no work unit")
	}
}

// T8.20.3 — Ordered completed/remaining/blocking lists retained: order is
// preserved exactly and the stored lists are independent copies.
func TestT8203_OrderedListsRetained(t *testing.T) {
	unit := uuid.New()
	completed := []string{"step-1", "step-2", "step-3"}
	remaining := []string{"step-4", "step-5"}
	blocking := []string{"block-a", "block-b"}
	got, err := NewContinuationStatus(ContinuationStatusInput{
		WorkUnitID: unit,
		Completed:  completed,
		Remaining:  remaining,
		Blocking:   blocking,
	})
	if err != nil {
		t.Fatalf("NewContinuationStatus: %v", err)
	}
	if !sameStrings(got.Completed, completed) || !sameStrings(got.Remaining, remaining) || !sameStrings(got.Blocking, blocking) {
		t.Fatalf("ordered lists not retained: completed=%v remaining=%v blocking=%v", got.Completed, got.Remaining, got.Blocking)
	}
	completed[0] = "MUTATED"
	if got.Completed[0] == "MUTATED" {
		t.Fatal("Completed list must be an independent copy")
	}
}

// T8.20.4 — Cross-work-unit status rejected: a status built from one work unit
// is not associated with, and fails validation against, a different work unit.
func TestT8204_CrossWorkUnitStatusRejected(t *testing.T) {
	own := uuid.New()
	other := uuid.New()
	got, err := NewContinuationStatus(ContinuationStatusInput{WorkUnitID: own})
	if err != nil {
		t.Fatalf("NewContinuationStatus: %v", err)
	}
	if got.AssociatedTo(other) {
		t.Fatal("status must not be associated with a different work unit")
	}
	if err := got.ValidateAssociation(other); err == nil {
		t.Fatal("ValidateAssociation: expected cross-work-unit rejection, got nil")
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
