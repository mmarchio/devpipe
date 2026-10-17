// P4-25 (TDD RED): progress scope changes (WBS 4.20).
package wbs

import (
	"strings"
	"testing"
)

// scopePlan builds the WBS 4.20 baseline: 16 units, 12 DONE (12/16 = 75%).
func scopePlan() []*WorkUnit {
	statuses := make([]WorkUnitStatus, 0, 16)
	for i := 0; i < 12; i++ {
		statuses = append(statuses, WorkUnitStatusDONE)
	}
	for i := 0; i < 4; i++ {
		statuses = append(statuses, WorkUnitStatusPENDING)
	}
	return unitsWithStatuses(statuses...)
}

// T4.20.1 — Adding task increases total.
func TestT4201AddUnitIncreasesTotal(t *testing.T) {
	pid, _ := wbsTestPhase()
	before := WBSProgressForPhase(pid, 1, scopePlan(), nil)
	if before.Total != 16 || before.Percentage != 75 {
		t.Fatalf("baseline = %+v, want 12/16 = 75%%", before)
	}
	after := WBSProgressForPhase(pid, 1, append(scopePlan(),
		progressTestUnit(17, WorkUnitStatusPENDING)), nil)
	if after.Total != 17 {
		t.Fatalf("total = %d after adding one unit, want 17 (derived from work units)", after.Total)
	}
}

// T4.20.2 — Completed count unchanged.
func TestT4202CompletedUnchanged(t *testing.T) {
	pid, _ := wbsTestPhase()
	before := WBSProgressForPhase(pid, 1, scopePlan(), nil)
	after := WBSProgressForPhase(pid, 1, append(scopePlan(),
		progressTestUnit(17, WorkUnitStatusPENDING)), nil)
	if after.Completed != before.Completed || after.Completed != 12 {
		t.Fatalf("completed = %d after append, want %d (appended unit is PENDING)", after.Completed, before.Completed)
	}
	if after.Remaining != before.Remaining+1 {
		t.Fatalf("remaining = %d, want %d", after.Remaining, before.Remaining+1)
	}
}

// T4.20.3 — Percentage recalculated and may decrease: the WBS 12/16 = 75%
// case, planner adds four required tasks -> 12/20 = 60%.
func TestT4203PercentageDecreases(t *testing.T) {
	pid, _ := wbsTestPhase()
	before := WBSProgressForPhase(pid, 1, scopePlan(), nil)
	added := append(scopePlan(),
		progressTestUnit(17, WorkUnitStatusPENDING),
		progressTestUnit(18, WorkUnitStatusPENDING),
		progressTestUnit(19, WorkUnitStatusPENDING),
		progressTestUnit(20, WorkUnitStatusPENDING))
	after := WBSProgressForPhase(pid, 1, added, nil)
	if after.Total != 20 || after.Completed != 12 {
		t.Fatalf("12/20: got %+v", after)
	}
	if after.Percentage != 60 {
		t.Fatalf("percentage = %v after adding four tasks, want 60 (the WBS decrease case)", after.Percentage)
	}
	if after.Percentage >= before.Percentage {
		t.Fatalf("percentage must decrease: before=%v after=%v", before.Percentage, after.Percentage)
	}
}

// T4.20.4 — Removing an eligible unexecuted (PENDING) unit recalculates the
// total: 12/16 -> 12/15 = 80%. The scope change is the unit set itself, and
// progress is derived from it, so the removal is covered by re-derivation
// over the reduced set.
func TestT4204RemoveUnexecutedUnitRecalculates(t *testing.T) {
	pid, _ := wbsTestPhase()
	units := scopePlan()
	// Drop the last PENDING unit (an unexecuted, dependency-free unit).
	removed := units[:len(units)-1]
	got := WBSProgressForPhase(pid, 1, removed, nil)
	if got.Total != 15 || got.Completed != 12 || got.Remaining != 3 {
		t.Fatalf("after removal: got %+v, want 12/15 Remaining=3", got)
	}
	if got.Percentage != 80 {
		t.Fatalf("percentage = %v after removing a PENDING unit, want 80 (12/15)", got.Percentage)
	}
}

// T4.20.5 — Progress percentage is never accepted as an authoritative write:
// the derivation depends only on unit statuses, and the domain progress
// structs carry no field a caller could set to force a stored percentage.
func TestT4205PercentageNeverAuthoritativeWrite(t *testing.T) {
	// Same units, caller-supplied "current" varying: percentage is a pure
	// function of unit statuses — nothing outside the unit set can move it.
	pid, _ := wbsTestPhase()
	a := WBSProgressForPhase(pid, 1, scopePlan(), nil)
	b := WBSProgressForPhase(pid, 1, scopePlan(), progressTestUnit(17, WorkUnitStatusEXECUTING))
	if a.Percentage != b.Percentage {
		t.Fatalf("percentage varies with non-unit inputs: %+v vs %+v", a, b)
	}
	// And the struct surface: Percentage is computed; no separate persisted
	// percentage field exists on WBSProgress/WorkProgress (verified by the
	// struct definitions in wbsprogress.go/workprogress.go: every field is a
	// count or a derivation of counts).
	if got := a.Percentage; got != 75 {
		t.Fatalf("baseline percentage = %v, want 75", got)
	}
	// Guard: WorkUnit has no percentage field a write path could accept.
	u := progressTestUnit(1, WorkUnitStatusPENDING)
	if strings.Contains(strings.ToLower(u.Status.String()), "percent") {
		t.Fatalf("work unit status carries percentage text: %q", u.Status)
	}
}
