// P4-15 (TDD RED): tests for the phase-completion eligibility query (WBS 4.21).
package wbs

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func testActivePlan() *ExecutionPlan {
	p := &ExecutionPlan{
		ID:         uuid.New(),
		PhaseID:    uuid.New(),
		WBSVersion: 1,
		Version:    1,
		Status:     PlanStatusEXECUTING,
		CreatedAt:  time.Now().UTC(),
	}
	return p
}

// T4.21.1 — All DONE → eligible.
func TestT4211AllDoneEligible(t *testing.T) {
	got := PhaseCompletionEligible(testActivePlan(), unitsWithCount(5, WorkUnitStatusDONE))
	if !got {
		t.Fatal("all units DONE with an active plan: want eligible=true, got false")
	}
}

// T4.21.2 — PENDING unit → not eligible.
func TestT4212PendingNotEligible(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusDONE, WorkUnitStatusPENDING,
	}
	got := PhaseCompletionEligible(testActivePlan(), unitsWithStatuses(statuses...))
	if got {
		t.Fatal("a PENDING unit present: want eligible=false, got true")
	}
}

// T4.21.3 — EXECUTING unit → not eligible.
func TestT4213ExecutingNotEligible(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusEXECUTING,
	}
	got := PhaseCompletionEligible(testActivePlan(), unitsWithStatuses(statuses...))
	if got {
		t.Fatal("an EXECUTING unit present: want eligible=false, got true")
	}
}

// T4.21.4 — FAILED unit → not eligible.
func TestT4214FailedNotEligible(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusFAILED,
	}
	got := PhaseCompletionEligible(testActivePlan(), unitsWithStatuses(statuses...))
	if got {
		t.Fatal("a FAILED unit present: want eligible=false, got true")
	}
}

// T4.21.5 — BLOCKED unit → not eligible.
func TestT4215BlockedNotEligible(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusBLOCKED,
	}
	got := PhaseCompletionEligible(testActivePlan(), unitsWithStatuses(statuses...))
	if got {
		t.Fatal("a BLOCKED unit present: want eligible=false, got true")
	}
}

// T4.21.6 — No execution plan → not eligible.
func TestT4216NoPlanNotEligible(t *testing.T) {
	got := PhaseCompletionEligible(nil, unitsWithCount(3, WorkUnitStatusDONE))
	if got {
		t.Fatal("no execution plan: want eligible=false, got true")
	}
}

// Constraint: "all DONE" requires a non-empty set of units; a plan with zero
// units is NOT eligible (nothing completed).
func TestT421ZeroUnitsNotEligible(t *testing.T) {
	for _, units := range [][]*WorkUnit{nil, {}} {
		if got := PhaseCompletionEligible(testActivePlan(), units); got {
			t.Fatalf("zero units: want eligible=false, got true (units=%v)", units)
		}
	}
}

// Only DONE counts as complete, matching the P4-14 WBSProgress DONE-only rule:
// READY/INCOMPLETE/VALIDATING units are unfinished required work.
func TestT421NonDoneStatusesNotEligible(t *testing.T) {
	for _, s := range []WorkUnitStatus{
		WorkUnitStatusREADY, WorkUnitStatusINCOMPLETE, WorkUnitStatusVALIDATING,
	} {
		if got := PhaseCompletionEligible(testActivePlan(), unitsWithCount(2, s)); got {
			t.Fatalf("unit with status %s present: want eligible=false, got true", s)
		}
	}
}
