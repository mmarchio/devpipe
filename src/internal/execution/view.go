package execution

import (
	"fmt"
	"time"
)

// categoryLabels are the UI labels (WBS 15.101).
var categoryLabels = map[ReadinessCategory]string{
	CategoryProject:            "Project configuration",
	CategoryWorkingDir:         "Working directory",
	CategoryRepository:         "Repository",
	CategoryPhase:              "Phase configuration",
	CategoryWBS:                "WBS",
	CategoryStateMachine:       "State machine",
	CategoryRoles:              "Roles",
	CategoryRoleInstr:          "Role instructions",
	CategoryStateInstr:         "State instructions",
	CategoryStateFiles:         "State files",
	CategoryTemplates:          "Prompt templates",
	CategoryCompression:        "Compression provider",
	CategoryWorker:             "Worker provider",
	CategoryCompressionSession: "Compression session ready",
	CategoryWorkerSession:      "Worker session ready",
	CategoryProtocol:           "Response protocol",
	CategoryContext:            "Context configuration",
	CategoryContinuation:       "Continuation policy",
}

// ReadinessCheck is one checklist row.
type ReadinessCheck struct {
	Category ReadinessCategory `json:"category"`
	Label    string            `json:"label"`
	OK       bool              `json:"ok"`
	Messages []string          `json:"messages"`
}

// ReadinessView is the execution-readiness panel (WBS 15.101/15.102).
type ReadinessView struct {
	Ready              bool             `json:"ready"`
	Headline           string           `json:"headline"`
	Checks             []ReadinessCheck `json:"checks"`
	Issues             []string         `json:"issues"`
	Warnings           []string         `json:"warnings"`
	CompletionEligible bool             `json:"completion_eligible"`
}

// NewReadinessView renders the report: one row per category in report
// order, every failure listed together.
func NewReadinessView(r *ExecutionReadinessReport) ReadinessView {
	v := ReadinessView{Ready: r.Ready, CompletionEligible: r.CompletionEligible, Issues: []string{}, Warnings: []string{}}
	byCat := map[ReadinessCategory][]string{}
	for _, e := range r.Errors {
		byCat[e.Category] = append(byCat[e.Category], e.Message)
		v.Issues = append(v.Issues, e.Message)
	}
	for _, w := range r.Warnings {
		v.Warnings = append(v.Warnings, w.Message)
	}
	for _, c := range categoryOrder {
		msgs := byCat[c]
		if msgs == nil {
			msgs = []string{}
		}
		v.Checks = append(v.Checks, ReadinessCheck{Category: c, Label: categoryLabels[c], OK: len(msgs) == 0, Messages: msgs})
	}
	switch {
	case r.CompletionEligible:
		v.Headline = "All phases complete — nothing to start"
	case r.Ready:
		v.Headline = "Ready to Start"
	default:
		noun := "issues"
		if len(r.Errors) == 1 {
			noun = "issue"
		}
		v.Headline = fmt.Sprintf("Cannot Start Project — %d configuration %s", len(r.Errors), noun)
	}
	return v
}

// PreviewView is the bootstrap preview (WBS 15.100).
type PreviewView struct {
	Project          string   `json:"project"`
	Status           string   `json:"status"`
	InitialPhase     string   `json:"initial_phase"`
	WBSVersion       int      `json:"wbs_version"`
	ExecutionPlan    string   `json:"execution_plan"`
	InitialState     string   `json:"initial_state"`
	FirstActionable  string   `json:"first_actionable_state"`
	Role             string   `json:"role"`
	WorkingDirectory string   `json:"working_directory"`
	Readiness        string   `json:"readiness"`
	PlanHash         string   `json:"plan_hash"`
	Steps            []string `json:"steps"`
}

// PlanStateLabel renders a plan state for humans.
func PlanStateLabel(s PlanState, version int) string {
	switch s {
	case PlanCurrent:
		return fmt.Sprintf("v%d (current)", version)
	case PlanStale:
		return fmt.Sprintf("v%d stale → planning required", version)
	case PlanInvalid:
		return fmt.Sprintf("v%d not usable → planning required", version)
	}
	return "Missing → planning required"
}

// NewPreviewView renders the preview from a plan and readiness.
func NewPreviewView(projectName, projectStatus string, p *BootstrapPlan, r *ExecutionReadinessReport) PreviewView {
	v := PreviewView{Project: projectName, Status: projectStatus, Readiness: "NOT READY", Steps: []string{}}
	if r != nil && r.Ready {
		v.Readiness = "READY"
	}
	if p == nil {
		return v
	}
	v.InitialPhase = fmt.Sprintf("%d — %s", p.PhaseOrder, p.PhaseName)
	v.WBSVersion = p.WBSVersion
	v.ExecutionPlan = PlanStateLabel(p.PlanState, p.PlanVersion)
	v.InitialState = p.InitialState
	v.FirstActionable = p.FirstActionable
	v.Role = p.Role
	v.WorkingDirectory = p.WorkingDirectory
	v.PlanHash = p.Hash()
	v.Steps = p.Steps()
	return v
}

// ActiveExecutionInput is the data of the active-execution panel.
type ActiveExecutionInput struct {
	Execution      Execution
	PhaseOrder     int
	PhaseName      string
	WorkflowState  string
	Role           string
	DoneUnits      int
	TotalUnits     int
	Dispatches     int
	Continuations  int
	SessionResets  int
	NoProgress     int
	BootstrapStage BootstrapStage
	ConfigHash     string
	StartedAt      time.Time
}

// ActiveExecutionView is the active-execution panel (WBS 15.103).
type ActiveExecutionView struct {
	Title           string  `json:"title"`
	Number          int     `json:"number"`
	Status          string  `json:"status"`
	Phase           string  `json:"phase"`
	WorkflowState   string  `json:"workflow_state"`
	Role            string  `json:"role"`
	DoneUnits       int     `json:"done_units"`
	TotalUnits      int     `json:"total_units"`
	ProgressPercent float64 `json:"progress_percent"`
	ProgressLabel   string  `json:"progress_label"`
	Worker          string  `json:"worker"`
	Continuations   int     `json:"continuations"`
	SessionResets   int     `json:"session_resets"`
	NoProgress      int     `json:"no_progress"`
	BootstrapStage  string  `json:"bootstrap_stage"`
	ConfigHash      string  `json:"configuration_hash"`
}

// NewActiveExecutionView derives the panel; progress comes only from DONE
// units (WBS 15.103/15.144).
func NewActiveExecutionView(in ActiveExecutionInput) ActiveExecutionView {
	v := ActiveExecutionView{
		Title:          fmt.Sprintf("Execution #%d", in.Execution.Number),
		Number:         in.Execution.Number,
		Status:         string(in.Execution.Status),
		Phase:          fmt.Sprintf("%d — %s", in.PhaseOrder, in.PhaseName),
		WorkflowState:  in.WorkflowState,
		Role:           in.Role,
		DoneUnits:      in.DoneUnits,
		TotalUnits:     in.TotalUnits,
		Continuations:  in.Continuations,
		SessionResets:  in.SessionResets,
		NoProgress:     in.NoProgress,
		BootstrapStage: string(in.BootstrapStage),
		ConfigHash:     in.ConfigHash,
	}
	if in.TotalUnits > 0 {
		v.ProgressPercent = float64(in.DoneUnits*100) / float64(in.TotalUnits)
	}
	v.ProgressLabel = fmt.Sprintf("%d / %d tasks complete", in.DoneUnits, in.TotalUnits)
	if in.Dispatches == 0 {
		v.Worker = "Not yet dispatched"
	} else {
		v.Worker = fmt.Sprintf("%d dispatches", in.Dispatches)
	}
	return v
}
