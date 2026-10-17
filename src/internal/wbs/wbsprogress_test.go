// P4-14 (TDD RED): tests for the formalized WBSProgress query (WBS 4.19).
package wbs

import (
	"testing"

	"github.com/google/uuid"
)

func wbsTestPhase() (uuid.UUID, int) {
	return uuid.New(), 3
}

// T4.19.1 — 0/0 returns 0%.
func TestT4191ZeroUnits(t *testing.T) {
	pid, order := wbsTestPhase()
	got := WBSProgressForPhase(pid, order, nil, nil)
	if got.Total != 0 || got.Completed != 0 || got.Remaining != 0 || got.Percentage != 0 {
		t.Fatalf("0/0: got %+v, want all zero", got)
	}
	if got.PhaseID != pid || got.PhaseOrder != order {
		t.Fatalf("phase identity not carried: %+v", got)
	}
	if got.CurrentWorkUnit != nil {
		t.Fatalf("no units: CurrentWorkUnit must be nil, got %+v", got.CurrentWorkUnit)
	}
}

// T4.19.2 — 0/20 returns 0%.
func TestT4192NothingComplete(t *testing.T) {
	pid, order := wbsTestPhase()
	got := WBSProgressForPhase(pid, order, unitsWithCount(20, WorkUnitStatusPENDING), nil)
	if got.Total != 20 || got.Completed != 0 || got.Remaining != 20 || got.Percentage != 0 {
		t.Fatalf("0/20: got %+v, want Total=20 Completed=0 Remaining=20 Percentage=0", got)
	}
}

// T4.19.3 — 10/20 returns 50%.
func TestT4193HalfComplete(t *testing.T) {
	pid, _ := wbsTestPhase()
	statuses := make([]WorkUnitStatus, 0, 20)
	for i := 0; i < 10; i++ {
		statuses = append(statuses, WorkUnitStatusDONE)
	}
	for i := 0; i < 10; i++ {
		statuses = append(statuses, WorkUnitStatusPENDING)
	}
	got := WBSProgressForPhase(pid, 1, unitsWithStatuses(statuses...), nil)
	if got.Total != 20 || got.Completed != 10 || got.Remaining != 10 || got.Percentage != 50 {
		t.Fatalf("10/20: got %+v, want Total=20 Completed=10 Remaining=10 Percentage=50", got)
	}
}

// T4.19.4 — 20/20 returns 100%.
func TestT4194AllComplete(t *testing.T) {
	pid, order := wbsTestPhase()
	got := WBSProgressForPhase(pid, order, unitsWithCount(20, WorkUnitStatusDONE), nil)
	if got.Total != 20 || got.Completed != 20 || got.Remaining != 0 || got.Percentage != 100 {
		t.Fatalf("20/20: got %+v, want Total=20 Completed=20 Remaining=0 Percentage=100", got)
	}
}

// T4.19.5 — EXECUTING units are excluded from Completed.
func TestT4195ExecutingExcluded(t *testing.T) {
	pid, _ := wbsTestPhase()
	// 10 DONE + 10 EXECUTING: only the DONE count.
	statuses := make([]WorkUnitStatus, 0, 20)
	for i := 0; i < 10; i++ {
		statuses = append(statuses, WorkUnitStatusDONE)
	}
	for i := 0; i < 10; i++ {
		statuses = append(statuses, WorkUnitStatusEXECUTING)
	}
	got := WBSProgressForPhase(pid, 1, unitsWithStatuses(statuses...), nil)
	if got.Completed != 10 || got.Percentage != 50 {
		t.Fatalf("EXECUTING counted as complete: %+v (want Completed=10 Percentage=50)", got)
	}
}

// T4.19.6 — VALIDATING units are excluded from Completed.
func TestT4196ValidatingExcluded(t *testing.T) {
	pid, _ := wbsTestPhase()
	got := WBSProgressForPhase(pid, 1, unitsWithCount(1, WorkUnitStatusVALIDATING), nil)
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("VALIDATING counted as complete: %+v (want Completed=0 Percentage=0)", got)
	}
}

// T4.19.7 — FAILED units are excluded from Completed.
func TestT4197FailedExcluded(t *testing.T) {
	pid, _ := wbsTestPhase()
	got := WBSProgressForPhase(pid, 1, unitsWithCount(1, WorkUnitStatusFAILED), nil)
	if got.Completed != 0 || got.Percentage != 0 {
		t.Fatalf("FAILED counted as complete: %+v (want Completed=0 Percentage=0)", got)
	}
}

// T4.19.8 — DONE units are included in Completed.
func TestT4198DoneIncluded(t *testing.T) {
	pid, _ := wbsTestPhase()
	got := WBSProgressForPhase(pid, 1, unitsWithCount(1, WorkUnitStatusDONE), nil)
	if got.Completed != 1 || got.Percentage != 100 {
		t.Fatalf("DONE not counted as complete: %+v (want Completed=1 Percentage=100)", got)
	}
}

// Non-vacuity: CurrentWorkUnit maps the supplied unit into a WorkUnitSummary
// and is nil when none is supplied.
func TestT419CurrentWorkUnitMapping(t *testing.T) {
	pid, order := wbsTestPhase()
	current := progressTestUnit(2, WorkUnitStatusEXECUTING)
	got := WBSProgressForPhase(pid, order, unitsWithCount(4, WorkUnitStatusPENDING), current)
	if got.CurrentWorkUnit == nil {
		t.Fatal("CurrentWorkUnit must be non-nil when a current unit is supplied")
	}
	if got.CurrentWorkUnit.ID != current.ID {
		t.Fatalf("CurrentWorkUnit.ID mismatch: got %v, want %v", got.CurrentWorkUnit.ID, current.ID)
	}
	if got.CurrentWorkUnit.Sequence != current.Sequence || got.CurrentWorkUnit.Status != current.Status {
		t.Fatalf("CurrentWorkUnit fields not mapped: %+v", got.CurrentWorkUnit)
	}
}
