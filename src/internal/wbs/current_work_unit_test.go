package wbs

import (
	"testing"

	"github.com/google/uuid"
)

// P7-10 (WBS 7.11): the current-work-unit selection rule for active-task
// derivation. The pure helper CurrentWorkUnit selects, within a plan's
// work-unit list (already in ascending sequence order), the FIRST unit whose
// status is not DONE — the unit the project is currently working on. It
// returns nil when the list is empty or every unit is DONE. The caller
// supplies the units of the project's active plan of its current phase
// (the phase/plan resolution rule is the WBS 2.19 CurrentPhase/ActivePlan
// composition), so this helper is the deterministic unit-level half.

func p710Unit(seq int, status WorkUnitStatus) *WorkUnit {
	return &WorkUnit{
		ID:       uuid.New(),
		Sequence: seq,
		Name:     "unit-" + string(rune('a'+seq%26)),
		Status:   status,
	}
}

// T7.11.1 (selection half) — the first non-DONE unit in sequence order is
// the current work unit.
func TestP710CurrentWorkUnitSelectsFirstNotDone(t *testing.T) {
	units := []*WorkUnit{
		p710Unit(1, WorkUnitStatusDONE),
		p710Unit(2, WorkUnitStatusEXECUTING),
		p710Unit(3, WorkUnitStatusPENDING),
	}
	got := CurrentWorkUnit(units)
	if got == nil {
		t.Fatal("want the first non-DONE unit, got nil")
	}
	if got.Sequence != 2 {
		t.Fatalf("current unit sequence = %d, want 2 (first non-DONE in sequence order)", got.Sequence)
	}
}

// A DONE head is skipped; a PENDING tail is not preselected over an earlier
// EXECUTING unit.
func TestP710CurrentWorkUnitSequenceOrderNotPendingPreemption(t *testing.T) {
	units := []*WorkUnit{
		p710Unit(1, WorkUnitStatusPENDING),
		p710Unit(2, WorkUnitStatusDONE),
		p710Unit(3, WorkUnitStatusPENDING),
	}
	got := CurrentWorkUnit(units)
	if got == nil || got.Sequence != 1 {
		t.Fatalf("current unit = %+v, want sequence 1 (first non-DONE)", got)
	}
}

// Every unit DONE -> nil: no current work unit (the derivation op reports
// service.ErrNotFound; the ACTIVE_TASK content is simply not re-derived).
func TestP710CurrentWorkUnitAllDoneIsNil(t *testing.T) {
	units := []*WorkUnit{
		p710Unit(1, WorkUnitStatusDONE),
		p710Unit(2, WorkUnitStatusDONE),
	}
	if got := CurrentWorkUnit(units); got != nil {
		t.Fatalf("all-DONE list: current unit = %+v, want nil", got)
	}
}

// An empty list is nil.
func TestP710CurrentWorkUnitEmptyIsNil(t *testing.T) {
	if got := CurrentWorkUnit(nil); got != nil {
		t.Fatalf("empty list: current unit = %+v, want nil", got)
	}
}
