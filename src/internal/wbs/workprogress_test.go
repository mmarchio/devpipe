// P2-18 scratch (TDD RED): tests for WorkProgress calculation.
package wbs

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// progressTestUnit builds a work unit with the given status directly (the
// domain model exposes Status as a field).
func progressTestUnit(seq int, status WorkUnitStatus) *WorkUnit {
	return &WorkUnit{
		ID:              uuid.New(),
		ExecutionPlanID: uuid.New(),
		Sequence:        seq,
		Name:            "unit",
		Kind:            WorkUnitKindATOMIC,
		Status:          status,
		CreatedAt:       time.Now().UTC(),
	}
}

func unitsWithStatuses(statuses ...WorkUnitStatus) []*WorkUnit {
	units := make([]*WorkUnit, 0, len(statuses))
	for i, s := range statuses {
		units = append(units, progressTestUnit(i+1, s))
	}
	return units
}

func unitsWithCount(n int, status WorkUnitStatus) []*WorkUnit {
	statuses := make([]WorkUnitStatus, n)
	for i := range statuses {
		statuses[i] = status
	}
	return unitsWithStatuses(statuses...)
}

// T2.18.1 — Empty plan: 0/0, 0%.
func TestT2181EmptyPlan(t *testing.T) {
	got := CalculateWorkProgress(nil)
	want := WorkProgress{Total: 0, Completed: 0, Remaining: 0, Percentage: 0}
	if got != want {
		t.Fatalf("empty plan: got %+v, want %+v", got, want)
	}
	gotEmpty := CalculateWorkProgress([]*WorkUnit{})
	if gotEmpty != want {
		t.Fatalf("empty slice: got %+v, want %+v", gotEmpty, want)
	}
}

// T2.18.2 — No tasks completed: 0/10 = 0%.
func TestT2182NoTasksCompleted(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(10, WorkUnitStatusPENDING))
	want := WorkProgress{Total: 10, Completed: 0, Remaining: 10, Percentage: 0}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// T2.18.3 — Half completed: 5/10 = 50%.
func TestT2183HalfCompleted(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusDONE, WorkUnitStatusDONE,
		WorkUnitStatusDONE, WorkUnitStatusDONE,
		WorkUnitStatusPENDING, WorkUnitStatusPENDING, WorkUnitStatusPENDING,
		WorkUnitStatusPENDING, WorkUnitStatusPENDING,
	}
	got := CalculateWorkProgress(unitsWithStatuses(statuses...))
	want := WorkProgress{Total: 10, Completed: 5, Remaining: 5, Percentage: 50}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// T2.18.4 — All completed: 10/10 = 100%.
func TestT2184AllCompleted(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(10, WorkUnitStatusDONE))
	want := WorkProgress{Total: 10, Completed: 10, Remaining: 0, Percentage: 100}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// T2.18.5 — EXECUTING doesn't count as complete.
func TestT2185ExecutingNotComplete(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(1, WorkUnitStatusEXECUTING))
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("EXECUTING counted as complete: %+v", got)
	}
}

// T2.18.6 — VALIDATING doesn't count as complete.
func TestT2186ValidatingNotComplete(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(1, WorkUnitStatusVALIDATING))
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("VALIDATING counted as complete: %+v", got)
	}
}

// T2.18.7 — INCOMPLETE doesn't count as complete.
func TestT2187IncompleteNotComplete(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(1, WorkUnitStatusINCOMPLETE))
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("INCOMPLETE counted as complete: %+v", got)
	}
}

// T2.18.8 — FAILED doesn't count as complete.
func TestT2188FailedNotComplete(t *testing.T) {
	got := CalculateWorkProgress(unitsWithCount(1, WorkUnitStatusFAILED))
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("FAILED counted as complete: %+v", got)
	}
}

// T2.18.9 — Added task changes denominator: 8/10 = 80%, then 8/12 ≈ 66.67%.
func TestT2189AddedTaskChangesDenominator(t *testing.T) {
	statuses := []WorkUnitStatus{
		WorkUnitStatusDONE, WorkUnitStatusDONE, WorkUnitStatusDONE,
		WorkUnitStatusDONE, WorkUnitStatusDONE, WorkUnitStatusDONE,
		WorkUnitStatusDONE, WorkUnitStatusDONE,
		WorkUnitStatusPENDING, WorkUnitStatusPENDING,
	}
	units := unitsWithStatuses(statuses...)

	before := CalculateWorkProgress(units)
	if before.Total != 10 || before.Completed != 8 || before.Remaining != 2 {
		t.Fatalf("before: %+v", before)
	}
	if before.Percentage != 80 {
		t.Fatalf("before: percentage %v, want 80", before.Percentage)
	}

	units = append(units,
		progressTestUnit(11, WorkUnitStatusPENDING),
		progressTestUnit(12, WorkUnitStatusPENDING),
	)
	after := CalculateWorkProgress(units)
	if after.Total != 12 || after.Completed != 8 || after.Remaining != 4 {
		t.Fatalf("after: %+v", after)
	}
	wantPct := 8.0 / 12.0 * 100 // 66.6666...
	if after.Percentage < wantPct-0.01 || after.Percentage > wantPct+0.01 {
		t.Fatalf("after: percentage %v, want ≈%v", after.Percentage, wantPct)
	}
}
