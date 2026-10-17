package wbs

import "github.com/google/uuid"

// WorkUnitSummary is the progress-bar view of a single work unit (WBS 4.19).
// It carries just the fields the UI needs to identify the unit currently
// being worked, without exposing the full domain record.
type WorkUnitSummary struct {
	ID       uuid.UUID
	Sequence int
	Name     string
	Status   WorkUnitStatus
}

// WBSProgress is the formalized current-WBS progress snapshot that drives the
// requested progress bar (WBS 4.19). It is derived purely from a phase's work
// units and is never persisted. Only DONE units count as complete.
type WBSProgress struct {
	PhaseID    uuid.UUID
	PhaseOrder int

	Completed int
	Total     int
	Remaining int

	Percentage float64

	CurrentWorkUnit *WorkUnitSummary
}

// progressSummary maps a work unit to its progress-bar summary; nil yields nil.
func progressSummary(u *WorkUnit) *WorkUnitSummary {
	if u == nil {
		return nil
	}
	return &WorkUnitSummary{
		ID:       u.ID,
		Sequence: u.Sequence,
		Name:     u.Name,
		Status:   u.Status,
	}
}

// WBSProgressForPhase computes the WBS 4.19 progress record for a phase from
// its work units. Completed counts DONE units only (EXECUTING/VALIDATING/
// FAILED excluded); Total is all units; Remaining = Total − Completed;
// Percentage = Completed/Total (0% when Total is 0). current is the unit being
// worked (or nil when there is none).
func WBSProgressForPhase(phaseID uuid.UUID, phaseOrder int, units []*WorkUnit, current *WorkUnit) WBSProgress {
	base := CalculateWorkProgress(units)
	return WBSProgress{
		PhaseID:         phaseID,
		PhaseOrder:      phaseOrder,
		Completed:       base.Completed,
		Total:           base.Total,
		Remaining:       base.Remaining,
		Percentage:      base.Percentage,
		CurrentWorkUnit: progressSummary(current),
	}
}
