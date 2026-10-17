package execution

import (
	"fmt"
	"strings"
)

// ProjectStateInput is the DB-authoritative PROJECT_STATE content
// (WBS 15.45).
type ProjectStateInput struct {
	ProjectName     string
	ProjectID       string
	ExecutionNumber int
	ExecutionStatus string
	PhaseOrder      int
	PhaseName       string
	WorkflowState   string
	Repository      RepositorySnapshot
}

// RenderProjectState renders PROJECT_STATE deterministically.
func RenderProjectState(in ProjectStateInput) string {
	var b strings.Builder
	b.WriteString("# Project State\n\n")
	fmt.Fprintf(&b, "Project: %s (%s)\n", in.ProjectName, in.ProjectID)
	fmt.Fprintf(&b, "Execution: #%d\n", in.ExecutionNumber)
	fmt.Fprintf(&b, "Status: %s\n", in.ExecutionStatus)
	fmt.Fprintf(&b, "Current Phase: %d — %s\n", in.PhaseOrder, in.PhaseName)
	fmt.Fprintf(&b, "Workflow State: %s\n", in.WorkflowState)
	b.WriteString("Repository: ")
	r := in.Repository
	switch {
	case r.IsGitRepository:
		head, branch := "(no commits)", "(detached)"
		if r.HeadCommit != nil {
			head = *r.HeadCommit
		}
		if r.Branch != nil {
			branch = *r.Branch
		}
		dirty := "clean"
		if r.Dirty {
			dirty = "dirty"
		}
		fmt.Fprintf(&b, "%s (git, branch %s, HEAD %s, %s)\n", r.WorkingDirectory, branch, head, dirty)
	default:
		fmt.Fprintf(&b, "%s (not a git repository)\n", r.WorkingDirectory)
	}
	return b.String()
}

// PhaseStateInput is the DB-authoritative PHASE_STATE content (WBS 15.46).
type PhaseStateInput struct {
	PhaseID     string
	PhaseOrder  int
	PhaseName   string
	PhaseStatus string
	WBSVersion  int
	PlanStatus  PlanState
	PlanVersion int
	DoneUnits   int
	TotalUnits  int
}

// RenderPhaseState renders PHASE_STATE for the current phase only.
func RenderPhaseState(in PhaseStateInput) string {
	var b strings.Builder
	b.WriteString("# Phase State\n\n")
	fmt.Fprintf(&b, "Phase: %d — %s (%s)\n", in.PhaseOrder, in.PhaseName, in.PhaseID)
	fmt.Fprintf(&b, "Phase Status: %s\n", in.PhaseStatus)
	fmt.Fprintf(&b, "WBS Version: v%d\n", in.WBSVersion)
	switch in.PlanStatus {
	case PlanCurrent:
		fmt.Fprintf(&b, "Execution Plan: v%d (current)\n", in.PlanVersion)
	case PlanStale:
		fmt.Fprintf(&b, "Execution Plan: v%d (stale — planning required)\n", in.PlanVersion)
	case PlanInvalid:
		fmt.Fprintf(&b, "Execution Plan: v%d (not usable — planning required)\n", in.PlanVersion)
	default:
		b.WriteString("Execution Plan: missing — planning required\n")
	}
	fmt.Fprintf(&b, "Progress: %d / %d work units DONE\n", in.DoneUnits, in.TotalUnits)
	return b.String()
}

// TestRecord is one persisted deterministic validation record. Worker
// claims are never TestRecords (WBS 15.49).
type TestRecord struct {
	Name    string
	Passed  bool
	Summary string
}

// RenderTestState renders TEST_STATE from deterministic records only.
func RenderTestState(phaseName string, records []TestRecord) string {
	var b strings.Builder
	b.WriteString("# Test State\n\n")
	fmt.Fprintf(&b, "Phase: %s\n\n", phaseName)
	if len(records) == 0 {
		b.WriteString("No deterministic validation recorded.\n")
		return b.String()
	}
	for _, r := range records {
		res := "FAIL"
		if r.Passed {
			res = "PASS"
		}
		fmt.Fprintf(&b, "- %s: %s", r.Name, res)
		if r.Summary != "" {
			fmt.Fprintf(&b, " — %s", r.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}
