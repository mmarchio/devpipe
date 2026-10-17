package execution

import (
	"time"

	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// SummaryInput is the persisted data an execution summary derives from
// (WBS 16.161). Counts come from record rows, never from memory.
type SummaryInput struct {
	Phases            []*phase.Phase
	CurrentPlanUnits  []*wbs.WorkUnit
	Dispatches        int
	Continuations     int
	SessionResets     int
	ValidationRuns    int
	FixAttempts       int
	Checkpoints       int
	HardeningGroups   int
	DuplicatesRemoved int
	TestsConsolidated int
	StartedAt         *time.Time
	CompletedAt       *time.Time
	Now               time.Time
}

// ExecutionSummary is the execution's derived summary (WBS 16.161).
type ExecutionSummary struct {
	PhasesCompleted    int   `json:"phases_completed"`
	PhasesTotal        int   `json:"phases_total"`
	WorkUnitsCompleted int   `json:"work_units_completed"`
	WorkUnitsTotal     int   `json:"work_units_total"`
	WorkerInvocations  int   `json:"worker_invocations"`
	Continuations      int   `json:"continuations"`
	SessionResets      int   `json:"session_resets"`
	ValidationAttempts int   `json:"validation_attempts"`
	FixAttempts        int   `json:"fix_attempts"`
	Checkpoints        int   `json:"checkpoints"`
	HardeningGroups    int   `json:"hardening_groups"`
	DuplicatesRemoved  int   `json:"duplicates_removed"`
	TestsConsolidated  int   `json:"tests_consolidated"`
	DurationMS         int64 `json:"duration_ms"`
}

// Summarize derives the summary. Duration runs from StartedAt to
// CompletedAt (or Now while running).
func Summarize(in SummaryInput) ExecutionSummary {
	s := ExecutionSummary{
		WorkerInvocations:  in.Dispatches,
		Continuations:      in.Continuations,
		SessionResets:      in.SessionResets,
		ValidationAttempts: in.ValidationRuns,
		FixAttempts:        in.FixAttempts,
		Checkpoints:        in.Checkpoints,
		HardeningGroups:    in.HardeningGroups,
		DuplicatesRemoved:  in.DuplicatesRemoved,
		TestsConsolidated:  in.TestsConsolidated,
	}
	for _, p := range in.Phases {
		if p == nil {
			continue
		}
		s.PhasesTotal++
		if p.Status == phase.StatusDONE {
			s.PhasesCompleted++
		}
	}
	pr := wbs.PlanProgress(in.CurrentPlanUnits)
	s.WorkUnitsCompleted, s.WorkUnitsTotal = pr.Done, pr.Total
	if in.StartedAt != nil {
		end := in.Now
		if in.CompletedAt != nil {
			end = *in.CompletedAt
		}
		if d := end.Sub(*in.StartedAt); d > 0 {
			s.DurationMS = d.Milliseconds()
		}
	}
	return s
}
