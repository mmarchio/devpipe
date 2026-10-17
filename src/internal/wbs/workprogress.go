// P2-18 scratch: deterministic WorkProgress calculation (WBS 2.18).
package wbs

// WorkProgress reports completion of an execution plan's work units
// (WBS 2.18). Only DONE work units count as completed.
type WorkProgress struct {
	Total      int
	Completed  int
	Remaining  int
	Percentage float64
}

// CalculateWorkProgress computes deterministic progress for a set of work
// units: progress = DONE / total × 100 (WBS 2.18). It is a pure domain
// function with no DB access; an empty set yields 0/0 and 0%.
func CalculateWorkProgress(units []*WorkUnit) WorkProgress {
	total := len(units)
	completed := 0
	for _, u := range units {
		if u == nil {
			continue
		}
		if u.Status == WorkUnitStatusDONE {
			completed++
		}
	}
	pct := 0.0
	if total > 0 {
		pct = float64(completed) / float64(total) * 100
	}
	return WorkProgress{
		Total:      total,
		Completed:  completed,
		Remaining:  total - completed,
		Percentage: pct,
	}
}
