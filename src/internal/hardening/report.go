package hardening

// GroupStatus is a duplicate group's processing state.
type GroupStatus string

// Group statuses.
const (
	GroupAnalyzed     GroupStatus = "ANALYZED"
	GroupSkipped      GroupStatus = "SKIPPED"
	GroupPlanned      GroupStatus = "PLANNED"
	GroupInProgress   GroupStatus = "IN_PROGRESS"
	GroupConsolidated GroupStatus = "CONSOLIDATED"
	GroupFailed       GroupStatus = "FAILED"
	// GroupRemaining: SAFE but left unprocessed at MAX_ROUNDS (16.138).
	GroupRemaining GroupStatus = "REMAINING"
)

// Lineage records what a consolidation did (WBS 16.147).
type Lineage struct {
	Sources     []string `json:"sources"`
	Destination string   `json:"destination"`
}

// GroupRecord is the persisted view of one group used by the report.
type GroupRecord struct {
	ID             string         `json:"id"`
	Round          int            `json:"round"`
	Classification Classification `json:"classification"`
	Reason         string         `json:"reason"`
	Status         GroupStatus    `json:"status"`
	Members        int            `json:"members"`
	Lineage        *Lineage       `json:"lineage,omitempty"`
}

// Progress is hardening progress, separate from WBS progress
// (WBS 16.145).
type Progress struct {
	Round              int    `json:"round"`
	GroupsAnalyzed     int    `json:"groups_analyzed"`
	SafeGroups         int    `json:"safe_groups"`
	GroupsConsolidated int    `json:"groups_consolidated"`
	GroupsSkipped      int    `json:"groups_skipped"`
	CurrentGroup       string `json:"current_group,omitempty"`
	CurrentOperation   string `json:"current_operation,omitempty"`
}

// ComputeProgress derives progress from group records.
func ComputeProgress(round int, groups []GroupRecord, currentGroup, currentOp string) Progress {
	p := Progress{Round: round, CurrentGroup: currentGroup, CurrentOperation: currentOp}
	for _, g := range groups {
		p.GroupsAnalyzed++
		if g.Classification == ClassSafe {
			p.SafeGroups++
		}
		switch g.Status {
		case GroupConsolidated:
			p.GroupsConsolidated++
		case GroupSkipped:
			p.GroupsSkipped++
		}
	}
	return p
}

// Report is the persisted hardening report (WBS 16.146).
type Report struct {
	FunctionsScanned     int           `json:"functions_scanned"`
	DuplicateGroups      int           `json:"duplicate_groups"`
	SafeGroups           int           `json:"safe_groups"`
	ConsolidatedGroups   int           `json:"consolidated_groups"`
	SkippedGroups        int           `json:"skipped_groups"`
	RemainingSafeGroups  int           `json:"remaining_safe_groups"`
	FunctionsRemoved     int           `json:"functions_removed"`
	ReferencesMigrated   int           `json:"references_migrated"`
	TestsBefore          int           `json:"tests_before"`
	TestsAfter           int           `json:"tests_after"`
	UniqueCasesPreserved int           `json:"unique_cases_preserved"`
	ImportCycleFailures  int           `json:"import_cycle_failures"`
	Rounds               int           `json:"rounds"`
	Termination          RoundVerdict  `json:"termination,omitempty"`
	Warning              string        `json:"warning,omitempty"`
	ValidationResult     string        `json:"validation_result"`
	Groups               []GroupRecord `json:"groups"`
}

// HardeningDone is the completion rule (WBS 16.149): every SAFE group of
// the final round is consolidated or reported remaining, every required
// validation passed, and no recovery condition is unresolved. Skipped
// (unsafe) groups never block when documented with a reason.
func HardeningDone(groups []GroupRecord, validationsPass, recoveryOpen bool) (bool, string) {
	if recoveryOpen {
		return false, "unresolved hardening recovery condition"
	}
	if !validationsPass {
		return false, "required hardening validation has not passed"
	}
	for _, g := range groups {
		switch g.Status {
		case GroupConsolidated, GroupRemaining:
		case GroupSkipped:
			if g.Reason == "" {
				return false, "skipped group " + g.ID + " has no recorded reason"
			}
		default:
			if g.Classification == ClassSafe {
				return false, "safe group " + g.ID + " not processed"
			}
		}
	}
	return true, ""
}
