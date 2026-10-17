package wbs

// CurrentWorkUnit returns the plan's current work unit (P7-10, WBS 7.11
// selection rule): the FIRST unit, in ascending sequence order, whose
// status is not DONE — the unit the project is currently working on. It
// returns nil when the list is empty or every unit is DONE. The caller
// supplies the units of the project's current phase's active plan (the
// WBS 2.19 CurrentPhase/ActivePlan composition), so this helper is the
// deterministic unit-level half of the resolution.
func CurrentWorkUnit(units []*WorkUnit) *WorkUnit {
	for _, u := range units {
		if u == nil {
			continue
		}
		if u.Status != WorkUnitStatusDONE {
			return u
		}
	}
	return nil
}
