package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/readmodel"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// ConsoleDeps wires the console read service.
type ConsoleDeps struct {
	Web           persistence.WebReadRepository
	Execs         persistence.ExecutionRepository
	Workers       *WorkerService
	Results       *ResultService
	Continuations *ContinuationService
	Engine        engineSettingsStore
	Recovery      *RecoveryActionService
	Inspector     execution.RepositoryInspector
	Settings      *SettingsService
	// PauseAfterTask reports a requested pause after the current task
	// (nil = never).
	PauseAfterTask interface {
		PauseAfterTaskPending(ctx context.Context, executionID uuid.UUID) (bool, error)
	}
}

// changedPathser is the optional read-only change listing of the inspector.
type changedPathser interface {
	ChangedPaths(ctx context.Context, dir string) ([]string, error)
}

// ConsoleReadService assembles the console read models from persisted
// state with bounded queries (WBS 17.4–17.31, 17.80–17.97, 17.158–17.160;
// P17-48 .. P17-61). It never mutates anything.
type ConsoleReadService struct {
	d ConsoleDeps
}

// NewConsoleReadService builds the service.
func NewConsoleReadService(d ConsoleDeps) *ConsoleReadService { return &ConsoleReadService{d: d} }

func uid(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// mapRead translates repository errors at the read boundary.
func mapRead(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrConflict) {
		return err
	}
	return translate(err)
}

// ---------------------------------------------------------------------------
// Timeline labels (WBS 17.80).

var eventLabels = map[execution.EventType]string{
	execution.EventBootstrapRequested:       "Bootstrap requested",
	execution.EventReadinessValidated:       "Readiness validated",
	execution.EventConfigurationSnapshotted: "Configuration snapshotted",
	execution.EventStateFilesInitialized:    "State files initialized",
	execution.EventRuntimeInitialized:       "Runtime initialized",
	execution.EventContextPrepared:          "Context prepared",
	execution.EventPromptPrepared:           "Initial prompt prepared",
	execution.EventBootstrapCompleted:       "Bootstrap completed",
	execution.EventBootstrapFailed:          "Bootstrap failed",
	execution.EventStarted:                  "Execution started",
	execution.EventStepIntent:               "Engine step started",
	execution.EventStepApplied:              "Workflow transition applied",
	execution.EventPlanActivated:            "Plan activated",
	execution.EventPlanRejected:             "Plan candidate rejected",
	execution.EventPlanAmended:              "Plan amended",
	execution.EventUnitClaimed:              "Work unit started",
	execution.EventUnitRequeueRequested:     "Task requeue requested",
	execution.EventUnitRequeued:             "Task requeued",
	execution.EventUnitAnalysisStarted:      "Task analysis started",
	execution.EventUnitAnalyzed:             "Task analyzed",
	execution.EventUnitFixStarted:           "Task fix started",
	execution.EventUnitFixed:                "Task fix finished",
	execution.EventUnitRepeatDetected:       "Worker repeated an action",
	execution.EventPauseAfterTaskRequested:  "Pause after task requested",
	execution.EventPauseAfterTaskCancelled:  "Pause after task cancelled",
	execution.EventPauseAfterTaskApplied:    "Paused after task",
	execution.EventDispatchIntent:           "Worker dispatch",
	execution.EventValidationStarted:        "Validation started",
	execution.EventValidationPassed:         "Validation PASS",
	execution.EventValidationFailed:         "Validation FAIL",
	execution.EventValidationInfraError:     "Validation infrastructure error",
	execution.EventFixAttempt:               "FIX attempt",
	execution.EventCheckpointCreated:        "Checkpoint created",
	execution.EventCheckpointFailed:         "Checkpoint failed",
	execution.EventCheckpointSkipped:        "Checkpoint skipped",
	execution.EventUnitDone:                 "Work unit DONE",
	execution.EventPhaseValidationFailed:    "Phase validation FAIL",
	execution.EventPhaseDone:                "Phase DONE",
	execution.EventExecutionWaiting:         "Waiting for worker",
	execution.EventExecutionPaused:          "Execution paused",
	execution.EventExecutionResumed:         "Execution resumed",
	execution.EventExecutionCancelled:       "Execution cancelled",
	execution.EventRecoveryRequired:         "RECOVERY REQUIRED",
	execution.EventRecoveryResolved:         "Recovery resolved",
	execution.EventHardeningStarted:         "Final hardening started",
	execution.EventHardeningAnalyzed:        "Hardening analysis",
	execution.EventHardeningOperation:       "Hardening operation",
	execution.EventHardeningGroupDone:       "Duplicate group consolidated",
	execution.EventHardeningGroupSkipped:    "Duplicate group skipped",
	execution.EventHardeningCompleted:       "Final hardening completed",
	execution.EventFinalValidationPassed:    "Final validation PASS",
	execution.EventFinalValidationFailed:    "Final validation FAIL",
	execution.EventProjectCompleted:         "Project COMPLETED",
}

// TimelineRowOf converts a persisted event into a minimized row.
func TimelineRowOf(e execution.Event) readmodel.TimelineRow {
	le := readmodel.FromExecutionEvent(e.ProjectSeq, e)
	label := eventLabels[e.Type]
	if label == "" {
		label = string(e.Type)
	}
	return readmodel.TimelineRow{Seq: e.ExecSeq, ProjectSeq: e.ProjectSeq, At: e.CreatedAt.UTC(), Type: string(e.Type),
		Category: le.Category, Label: label, Status: le.Status, Refs: le.Refs, ExecutionID: le.ExecutionID}
}

// ---------------------------------------------------------------------------
// Dashboard (P17-48).

// Dashboard returns the dashboard aggregates (WBS 17.4).
func (s *ConsoleReadService) Dashboard(ctx context.Context) (*readmodel.Dashboard, error) {
	c, err := s.d.Web.DashboardCounts(ctx)
	if err != nil {
		return nil, mapRead(err)
	}
	d := &readmodel.Dashboard{ProjectsTotal: c.ProjectsTotal, Running: c.ProjectsByStatus["RUNNING"], Paused: c.ProjectsByStatus["PAUSED"],
		RecoveryRequired: c.RecoveryRequired, Completed: c.ProjectsByStatus["COMPLETED"], Failed: c.ProjectsByStatus["FAILED"],
		ActiveExecutions: c.ActiveExecutions, ActiveSessions: c.ActiveSessions, ActiveDispatches: c.ActiveDispatches,
		RunningValidations: c.RunningValidations, RecentEvents: []readmodel.TimelineRow{}}
	for _, st := range []project.ProjectStatus{project.StatusDRAFT, project.StatusREADY, project.StatusRUNNING, project.StatusPAUSED,
		project.StatusCOMPLETED, project.StatusFAILED, project.StatusCANCELLED} {
		d.ByStatus = append(d.ByStatus, readmodel.StatusCount{Status: string(st), Count: c.ProjectsByStatus[string(st)]})
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Project list (P17-49).

// ProjectQuery is the raw list query (server-side filters, WBS 17.6).
type ProjectQuery struct {
	Status   string
	Name     string
	Active   string // "", "active", "inactive"
	Recovery bool
	Limit    int
	Offset   int
}

// Projects returns one filtered page.
func (s *ConsoleReadService) Projects(ctx context.Context, q ProjectQuery) (*readmodel.ProjectList, error) {
	f := persistence.ProjectPageFilter{Name: strings.TrimSpace(q.Name), Recovery: q.Recovery, Limit: q.Limit, Offset: q.Offset}
	fields := map[string]string{}
	if q.Status != "" {
		if _, err := project.ParseStatus(q.Status); err != nil {
			fields["status"] = "unknown status"
		}
		f.Status = q.Status
	}
	switch q.Active {
	case "":
	case "active":
		t := true
		f.Active = &t
	case "inactive":
		v := false
		f.Active = &v
	default:
		fields["active"] = "must be active or inactive"
	}
	if f.Limit == 0 {
		f.Limit = 25
	}
	if f.Limit < 0 || f.Limit > 200 {
		fields["limit"] = "must be 1..200"
	}
	if f.Offset < 0 {
		fields["offset"] = "must not be negative"
	}
	if len(fields) > 0 {
		return nil, &FieldErrors{Fields: fields}
	}
	rows, total, err := s.d.Web.ProjectListPage(ctx, f)
	if err != nil {
		return nil, mapRead(err)
	}
	out := &readmodel.ProjectList{Rows: make([]readmodel.ProjectRow, 0, len(rows)), Page: readmodel.Page{Total: total, Limit: f.Limit, Offset: f.Offset}}
	for _, r := range rows {
		row := readmodel.ProjectRow{ID: r.ID.String(), Name: r.Name, Status: r.Status, PhaseStatus: r.PhaseStatus,
			Progress: readmodel.NewProgress(r.Done, r.Total), CurrentState: r.StateKey, LastActivity: r.LastActivity.UTC(),
			ExecutionStatus: r.ExecutionStatus, Active: r.ActiveExecution, Recovery: r.RecoveryRequired,
			Deletable: ProjectDeletable(r.Status)}
		if r.PhaseID != nil {
			row.CurrentPhase = strconv.Itoa(r.PhaseOrder) + " " + r.PhaseName
		}
		if r.UnitID != nil {
			row.CurrentTask = strings.TrimSpace(r.UnitKey + " " + r.UnitName)
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Overview (P17-50) + current task (P17-54).

func stalenessOf(planID *uuid.UUID, planWBS, currentWBS int) readmodel.Staleness {
	st := readmodel.Staleness{CurrentWBS: currentWBS, PlanSource: planWBS}
	switch {
	case planID == nil || currentWBS <= 0:
		st.State = "UNPLANNED"
	case planWBS == currentWBS:
		st.State = "CURRENT"
	default:
		st.State, st.Stale = "STALE", true
	}
	return st
}

// snapshotContent decodes an execution's captured configuration (nil
// when absent).
func (s *ConsoleReadService) snapshotContent(ctx context.Context, executionID uuid.UUID) *execution.ConfigurationContent {
	snap, err := s.d.Execs.GetSnapshot(ctx, executionID)
	if err != nil || snap == nil {
		return nil
	}
	c, err := execution.DecodeContent(snap.Canonical)
	if err != nil {
		return nil
	}
	return &c
}

// roleFor returns the role(s) of a state in the captured machine.
func roleFor(c *execution.ConfigurationContent, stateKey string) string {
	if c == nil {
		return ""
	}
	for _, st := range c.StateMachine.States {
		if st.Key == stateKey {
			return strings.Join(st.Roles, ", ")
		}
	}
	return ""
}

// Overview returns the project overview; every value comes from the
// server read model (WBS 17.10).
func (s *ConsoleReadService) Overview(ctx context.Context, projectID uuid.UUID) (*readmodel.Overview, error) {
	x, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	o := &readmodel.Overview{ProjectID: x.ProjectID.String(), Name: x.Name, Description: x.Description, Status: x.Status,
		Deletable:      ProjectDeletable(x.Status),
		ProjectVersion: x.ProjectVersion, PhasesDone: x.PhasesDone, PhasesTotal: x.PhasesTotal,
		Progress: readmodel.NewProgress(x.UnitsDone, x.UnitsTotal), UpdatedAt: x.UpdatedAt.UTC(),
		Runtime: readmodel.RuntimeRef{StateKey: x.StateKey, StateName: x.StateName, Kind: x.StateKind, EnteredAt: x.StateEntered}}
	var content *execution.ConfigurationContent
	if x.ExecutionID != nil {
		o.Execution = &readmodel.ExecutionSummary{ID: x.ExecutionID.String(), Number: x.ExecutionNumber, Status: x.ExecutionStatus,
			Generation: x.Generation, Active: x.ExecutionActive, WaitReason: x.WaitReason, RecoveryReason: x.RecoveryReason,
			StartedAt: x.ExecutionStarted, CompletedAt: x.ExecutionComplete}
		o.Recovery = x.ExecutionStatus == string(execution.ExecutionRecovery)
		content = s.snapshotContent(ctx, *x.ExecutionID)
	}
	if x.PhaseID != nil {
		o.Phase = &readmodel.PhaseRef{ID: x.PhaseID.String(), Order: x.PhaseOrder, Name: x.PhaseName, Status: x.PhaseStatus}
	}
	if x.PlanID != nil {
		o.Plan = &readmodel.PlanRef{ID: x.PlanID.String(), Version: x.PlanVersion, SourceWBS: x.PlanWBS, Status: x.PlanStatus, Kind: x.PlanKind}
	}
	o.Staleness = stalenessOf(x.PlanID, x.PlanWBS, x.WBSVersion)
	// Worker context (WBS 17.13 / 17.14): the reset threshold comes from
	// the execution configuration snapshot, never current settings.
	used, window := 0, 0
	if x.ContextTokens != nil {
		used = int(*x.ContextTokens)
	}
	if x.ContextWindow != nil {
		window = int(*x.ContextWindow)
	}
	threshold := 0.0
	if content != nil {
		threshold = content.Continuation.ContextThreshold
		if window == 0 {
			window = content.Continuation.ContextWindow
		}
	}
	o.Context = readmodel.NewContextUsage(used, window, threshold)
	if x.ContextTokens == nil {
		o.Context.Known = false
	}
	facts := readmodel.UnitFacts{Status: x.UnitStatus, WorkerResult: x.UnitWorkerResult, ValidationOutcome: x.UnitValidation,
		CheckpointPending:    x.UnitValidation == "PASS" && x.UnitCheckpoint != "CREATED" && x.UnitCheckpoint != "SKIPPED",
		ContinuationInFlight: x.ContinuationActive}
	if x.UnitID != nil {
		o.CurrentUnit = &readmodel.UnitRef{ID: x.UnitID.String(), Key: x.UnitKey, Name: x.UnitName, Status: x.UnitStatus, Kind: x.UnitKind,
			Stage: readmodel.StageOf(facts), StartedAt: x.UnitStartedAt, CompletedAt: x.UnitCompletedAt}
		// A DONE unit is not a current task (between units).
		if x.UnitStatus != "DONE" {
			o.CurrentTask = &readmodel.CurrentTask{UnitID: x.UnitID.String(), Key: x.UnitKey, Name: x.UnitName, Status: x.UnitStatus,
				Stage: o.CurrentUnit.Stage, Role: roleFor(content, x.StateKey), Attempt: x.UnitAttempts, SessionID: uid(x.SessionID),
				StartedAt: x.UnitStartedAt}
		}
	}
	execStatus := execution.ExecutionStatus(x.ExecutionStatus)
	statefile := content != nil && statefileMachine(content.StateMachine)
	requeueable := o.CurrentTask != nil && statefile
	pauseAfter := false
	if x.ExecutionID != nil && statefile && s.d.PauseAfterTask != nil {
		if pauseAfter, err = s.d.PauseAfterTask.PauseAfterTaskPending(ctx, *x.ExecutionID); err != nil {
			return nil, mapRead(err)
		}
	}
	for _, a := range execution.AllowedActions(execution.ControlFacts{ProjectStatus: x.Status, HasExecution: x.ExecutionID != nil,
		ExecutionStatus: execStatus, ReadinessOK: true, Requeueable: requeueable, Statefile: statefile, PauseAfterTask: pauseAfter}) {
		o.Actions = append(o.Actions, string(a))
	}
	if o.Actions == nil {
		o.Actions = []string{}
	}
	if seq, err := s.d.Web.MaxProjectSeq(ctx, projectID); err == nil {
		o.LatestEventSeq = seq
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// Phases (P17-51).

func phaseRowOf(p persistence.PhaseRow, current bool) readmodel.PhaseRow {
	r := readmodel.PhaseRow{ID: p.ID.String(), Order: p.Order, Name: p.Name, Description: p.Description, Status: p.Status,
		Progress: readmodel.NewProgress(p.Done, p.Total), Current: current, WBSVersion: p.WBSVersion, WBSHash: p.WBSHash,
		Staleness: stalenessOf(p.PlanID, p.PlanWBS, p.WBSVersion), StartedAt: p.StartedAt, CompletedAt: p.CompletedAt}
	if p.PlanID != nil {
		r.Plan = &readmodel.PlanRef{ID: p.PlanID.String(), Version: p.PlanVersion, SourceWBS: p.PlanWBS, Status: p.PlanStatus, Kind: p.PlanKind}
	}
	return r
}

// Phases lists phases by persisted phase_order with persisted statuses
// (never inferred from progress, WBS 17.15 / 17.16).
func (s *ConsoleReadService) Phases(ctx context.Context, projectID uuid.UUID) ([]readmodel.PhaseRow, error) {
	x, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	rows, err := s.d.Web.PhaseRows(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	activePhase, err := s.activeExecutionPhase(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]readmodel.PhaseRow, 0, len(rows))
	for _, p := range rows {
		out = append(out, withMovability(phaseRowOf(p, x.PhaseID != nil && *x.PhaseID == p.ID), p, activePhase))
	}
	return out, nil
}

// activeExecutionPhase is the phase the project's active execution is on
// (nil when none).
func (s *ConsoleReadService) activeExecutionPhase(ctx context.Context, projectID uuid.UUID) (*uuid.UUID, error) {
	if s.d.Execs == nil {
		return nil, nil
	}
	ex, err := s.d.Execs.GetActive(ctx, nil, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, mapRead(err)
	}
	return ex.CurrentPhaseID, nil
}

// withMovability sets whether the phase can be reordered (PhaseLockReason).
func withMovability(r readmodel.PhaseRow, p persistence.PhaseRow, activePhase *uuid.UUID) readmodel.PhaseRow {
	r.LockReason = PhaseLockReason(p.Status, p.PlanID != nil, activePhase != nil && *activePhase == p.ID)
	r.Movable = r.LockReason == ""
	return r
}

// Phase returns one phase's detail (phase-scoped data only, WBS 17.18).
func (s *ConsoleReadService) Phase(ctx context.Context, projectID, phaseID uuid.UUID) (*readmodel.PhaseDetail, error) {
	x, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	rows, err := s.d.Web.PhaseRows(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	var row *persistence.PhaseRow
	for i := range rows {
		if rows[i].ID == phaseID {
			row = &rows[i]
		}
	}
	if row == nil {
		return nil, ErrNotFound
	}
	activePhase, err := s.activeExecutionPhase(ctx, projectID)
	if err != nil {
		return nil, err
	}
	d := &readmodel.PhaseDetail{Phase: withMovability(phaseRowOf(*row, x.PhaseID != nil && *x.PhaseID == phaseID), *row, activePhase), KindCounts: []readmodel.KindCount{},
		Validations: []readmodel.ValidationRun{}, Events: []readmodel.TimelineRow{}}
	if row.PlanID != nil {
		kinds, err := s.d.Web.PlanKindCounts(ctx, *row.PlanID)
		if err != nil {
			return nil, mapRead(err)
		}
		for _, k := range []string{"ATOMIC", "COMPOSITION", "INTEGRATION", "VALIDATION"} {
			d.KindCounts = append(d.KindCounts, readmodel.KindCount{Kind: k, Count: kinds[k]})
		}
	}
	runs, err := s.d.Web.ValidationRuns(ctx, projectID, persistence.ValidationFilter{PhaseID: &phaseID, Scope: "PHASE", Limit: 50})
	if err != nil {
		return nil, mapRead(err)
	}
	for _, r := range runs {
		d.Validations = append(d.Validations, runOf(r))
	}
	if x.ExecutionID != nil {
		d.Execution = &readmodel.ExecutionSummary{ID: x.ExecutionID.String(), Number: x.ExecutionNumber, Status: x.ExecutionStatus}
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Work units (P17-52 / P17-53).

// UnitQuery filters a work-unit page (WBS 17.26).
type UnitQuery struct {
	Status     string
	Kind       string
	Readiness  string
	Validation string
	Limit      int
	Offset     int
}

func readinessLabel(status string, blocking []string) string {
	switch {
	case status == "DONE":
		return "DONE"
	case len(blocking) > 0:
		return "BLOCKED BY: " + strings.Join(blocking, ", ")
	}
	return "READY"
}

func unitRowOf(w persistence.WorkUnitRow, current *uuid.UUID) readmodel.UnitRow {
	facts := readmodel.UnitFacts{Status: w.Status, ValidationOutcome: w.Validation,
		CheckpointPending: w.Validation == "PASS" && w.Checkpoint != "CREATED" && w.Checkpoint != "SKIPPED"}
	deps, blocking := w.Deps, w.Blocking
	if deps == nil {
		deps = []string{}
	}
	if blocking == nil {
		blocking = []string{}
	}
	return readmodel.UnitRow{ID: w.ID.String(), Sequence: w.Sequence, Key: w.Key, Kind: w.Kind, Name: w.Name, Status: w.Status,
		Stage: readmodel.StageOf(facts), Deps: deps, Blocking: blocking, Readiness: readinessLabel(w.Status, w.Blocking),
		Attempts: w.Attempts, Validation: w.Validation, Checkpoint: w.Checkpoint, Current: current != nil && *current == w.ID,
		StartedAt: w.StartedAt, CompletedAt: w.CompletedAt}
}

// Units returns one page of a phase's current-plan units; readiness and the
// current-unit flag are computed server-side (WBS 17.29 / 17.30).
func (s *ConsoleReadService) Units(ctx context.Context, projectID, phaseID uuid.UUID, q UnitQuery) (*readmodel.UnitPage, error) {
	x, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	rows, err := s.d.Web.PhaseRows(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	var row *persistence.PhaseRow
	for i := range rows {
		if rows[i].ID == phaseID {
			row = &rows[i]
		}
	}
	if row == nil {
		return nil, ErrNotFound
	}
	fields := map[string]string{}
	if q.Status != "" && !contains([]string{"PENDING", "READY", "EXECUTING", "INCOMPLETE", "VALIDATING", "DONE", "FAILED", "BLOCKED"}, q.Status) {
		fields["status"] = "unknown status"
	}
	if q.Kind != "" && !contains([]string{"ATOMIC", "COMPOSITION", "INTEGRATION", "VALIDATION"}, q.Kind) {
		fields["kind"] = "unknown kind"
	}
	if q.Readiness != "" && q.Readiness != "READY" && q.Readiness != "BLOCKED" {
		fields["readiness"] = "must be READY or BLOCKED"
	}
	if q.Validation != "" && !contains([]string{"RUNNING", "PASS", "FAIL", "INFRA_ERROR", "NOT_CONFIGURED", "ABANDONED"}, q.Validation) {
		fields["validation"] = "unknown validation result"
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 0 || q.Limit > 200 {
		fields["limit"] = "must be 1..200"
	}
	if q.Offset < 0 {
		fields["offset"] = "must not be negative"
	}
	if len(fields) > 0 {
		return nil, &FieldErrors{Fields: fields}
	}
	page := &readmodel.UnitPage{ProjectID: projectID.String(), PhaseID: phaseID.String(), Rows: []readmodel.UnitRow{}, Page: readmodel.Page{Limit: q.Limit, Offset: q.Offset}}
	if row.PlanID == nil {
		return page, nil
	}
	page.Plan = &readmodel.PlanRef{ID: row.PlanID.String(), Version: row.PlanVersion, SourceWBS: row.PlanWBS, Status: row.PlanStatus, Kind: row.PlanKind}
	units, total, err := s.d.Web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: *row.PlanID, Status: q.Status, Kind: q.Kind,
		Readiness: q.Readiness, Validation: q.Validation, Limit: q.Limit, Offset: q.Offset})
	if err != nil {
		return nil, mapRead(err)
	}
	var current *uuid.UUID
	if x.ExecutionActive {
		current = x.UnitID
	}
	page.Page.Total = total
	for _, u := range units {
		page.Rows = append(page.Rows, unitRowOf(u, current))
	}
	return page, nil
}

// Edges returns a page of a plan's persisted dependency edges (WBS 17.28).
func (s *ConsoleReadService) Edges(ctx context.Context, projectID, phaseID uuid.UUID, limit, offset int) ([]readmodel.Edge, readmodel.Page, error) {
	rows, err := s.d.Web.PhaseRows(ctx, projectID)
	if err != nil {
		return nil, readmodel.Page{}, mapRead(err)
	}
	var planID *uuid.UUID
	found := false
	for _, r := range rows {
		if r.ID == phaseID {
			planID, found = r.PlanID, true
		}
	}
	if !found {
		return nil, readmodel.Page{}, ErrNotFound
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	out := []readmodel.Edge{}
	if planID == nil {
		return out, readmodel.Page{Limit: limit, Offset: offset}, nil
	}
	edges, total, err := s.d.Web.PlanEdges(ctx, *planID, limit, offset)
	if err != nil {
		return nil, readmodel.Page{}, mapRead(err)
	}
	for _, e := range edges {
		out = append(out, readmodel.Edge{FromID: e.FromID.String(), FromKey: e.FromKey, ToID: e.ToID.String(), ToKey: e.ToKey})
	}
	return out, readmodel.Page{Total: total, Limit: limit, Offset: offset}, nil
}

func acceptanceItems(a *execution.AcceptanceResult, texts []string) []readmodel.AcceptanceItem {
	out := []readmodel.AcceptanceItem{}
	if a != nil && len(a.Verdicts) > 0 {
		for _, v := range a.Verdicts {
			out = append(out, acceptanceItem(v.Index, v.Text, string(v.Verification), v.Verdict))
		}
		return out
	}
	for i, t := range texts {
		out = append(out, acceptanceItem(i, t, "", ""))
	}
	return out
}

// acceptanceItem labels a criterion; manual / unverified criteria are never
// shown as passed (WBS 17.91).
func acceptanceItem(i int, text, verification, verdict string) readmodel.AcceptanceItem {
	it := readmodel.AcceptanceItem{Index: i, Text: text, Verification: verification, Verdict: verdict}
	switch {
	case verdict == execution.VerdictPass && verification == "MANUAL":
		it.Label, it.Symbol = "Manual (approved)", "✓"
	case verdict == execution.VerdictPass:
		it.Label, it.Symbol = "Deterministic PASS", "✓"
	case verdict == execution.VerdictFail:
		it.Label, it.Symbol = "Deterministic FAIL", "✗"
	case verification == "MANUAL":
		it.Label, it.Symbol = "Manual review required", "!"
	case verdict == execution.VerdictUnverified:
		it.Label, it.Symbol = "Review", "⧗"
	default:
		it.Label, it.Symbol = "Not yet evaluated", "·"
	}
	return it
}

func runOf(r persistence.ValidationRunRow) readmodel.ValidationRun {
	v := readmodel.ValidationRun{ID: r.ID.String(), ExecutionID: r.ExecutionID.String(), Scope: r.Scope, UnitID: uid(r.WorkUnitID),
		PhaseID: uid(r.PhaseID), Label: r.Label, Attempt: r.Attempt, Status: r.Status, Commands: r.Commands, Failed: r.Failed,
		CWD: r.CWD, Acceptance: acceptanceItems(r.Acceptance, nil), Overall: r.Status, StartedAt: r.StartedAt.UTC(), FinishedAt: r.FinishedAt}
	return v
}

// deliveryLabel keeps DELIVERY_UNKNOWN visibly distinct from FAILED
// (WBS 17.83).
func deliveryLabel(status, delivery string) string {
	switch {
	case delivery == string(worker.DeliveryUnknown) || status == string(worker.DispatchUnknown):
		return "DELIVERY UNKNOWN (not resent)"
	case status == string(worker.DispatchFailed) && delivery == "DEFINITELY_NOT_SENT":
		return "FAILED (not sent)"
	case status == string(worker.DispatchFailed):
		return "FAILED"
	case status == string(worker.DispatchCompleted):
		return "RESPONSE RECEIVED"
	}
	return status
}

func dispatchRowOf(d worker.WorkerDispatch) readmodel.DispatchRow {
	r := readmodel.DispatchRow{ID: d.ID.String(), State: d.StateKey, Role: d.RoleKey, PromptID: d.WorkerPromptID.String(),
		Status: string(d.Status), Attempt: d.Attempt, Model: d.WorkerModel, CreatedAt: d.CreatedAt.UTC(), CompletedAt: d.CompletedAt,
		UnitID: uid(d.WorkUnitID), SessionID: d.SessionID.String()}
	if d.Delivery != nil {
		r.Delivery = string(*d.Delivery)
	}
	r.DeliveryTag = deliveryLabel(r.Status, r.Delivery)
	if d.CompletedAt != nil {
		r.DurationMS = d.CompletedAt.Sub(d.CreatedAt).Milliseconds()
	}
	return r
}

func resultOf(r persistence.StoredWorkerResult, overseerStatus string) readmodel.WorkerResult {
	w := r.Result
	claim := "not reported"
	if w.Tests != nil {
		switch {
		case !w.Tests.ReportedRun:
			claim = "worker reports tests not run"
		case w.Tests.ReportedPassed != nil && *w.Tests.ReportedPassed:
			claim = "worker claims tests passed (not deterministic)"
		case w.Tests.ReportedPassed != nil:
			claim = "worker claims tests failed (not deterministic)"
		default:
			claim = "worker reports tests run"
		}
	}
	status := strings.ToUpper(string(w.Status))
	return readmodel.WorkerResult{ID: r.ID.String(), DispatchID: w.Metadata.DispatchID.String(), Status: status, Summary: w.Summary,
		Completed: nonNil(w.Completed), Remaining: nonNil(w.Remaining), Blocking: nonNil(w.Blocking), NextAction: w.NextAction,
		TestsClaim: claim, Advisory: true, Report: readmodel.Report{Worker: status, Overseer: overseerStatus},
		UnitID: uid(w.Metadata.WorkUnitID), CreatedAt: r.CreatedAt.UTC()}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func strategyLabel(s continuation.Strategy) string {
	switch s {
	case continuation.ContinueSameSession:
		return "Same session"
	case continuation.ContinueFreshSession:
		return "Session reset (fresh session)"
	}
	return "Undecided"
}

func continuationOf(c continuation.Continuation) readmodel.ContinuationRow {
	r := readmodel.ContinuationRow{ID: c.ID.String(), Number: c.Attempt, Trigger: string(c.Trigger), Status: string(c.Status),
		Strategy: string(c.Strategy), StrategyLabel: strategyLabel(c.Strategy), Completed: len(c.Captured.Completed),
		Remaining: len(c.Captured.Remaining), Blocking: len(c.Captured.Blocking), Reset: c.Strategy == continuation.ContinueFreshSession,
		OldSession: uid(c.OldSessionID), NewSession: uid(c.NewSessionID), UnitID: c.WorkUnitID.String(), DispatchID: uid(c.DispatchID),
		RecoveryReason: string(c.RecoveryReason), CreatedAt: c.CreatedAt.UTC(), CompletedAt: c.CompletedAt}
	switch c.Trigger {
	case continuation.TriggerResultIncomplete:
		r.Reason = "Worker incomplete"
	case continuation.TriggerContextLimit:
		r.Reason = "Context limit reached"
	case continuation.TriggerSessionLost:
		r.Reason = "Worker session lost"
	default:
		r.Reason = string(c.Trigger)
	}
	if c.Decision != nil {
		if c.Decision.Utilization != nil {
			p := *c.Decision.Utilization * 100
			p = float64(int64(p*10+0.5)) / 10
			r.ContextBefore = &p
		}
		if c.Decision.Reason == continuation.ReasonAboveThreshold || c.Decision.Reason == continuation.ReasonAtThreshold {
			r.Reason += " — context threshold exceeded"
		}
	}
	return r
}

// Unit returns a work unit's full lineage (WBS 17.27).
func (s *ConsoleReadService) Unit(ctx context.Context, projectID, unitID uuid.UUID) (*readmodel.UnitDetail, error) {
	x, err := s.d.Web.UnitDetail(ctx, projectID, unitID)
	if err != nil {
		return nil, mapRead(err)
	}
	ov, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err != nil {
		return nil, mapRead(err)
	}
	var current *uuid.UUID
	if ov.ExecutionActive {
		current = ov.UnitID
	}
	d := &readmodel.UnitDetail{Unit: unitRowOf(x.WorkUnitRow, current), PhaseID: x.PhaseID.String(), PlanID: x.PlanID.String(),
		PlanVersion: x.PlanVersion, Objective: x.Objective, Instructions: x.Instructions, Description: x.Description,
		Tests: nonNil(x.RequiredTests), Constraints: nonNil(x.Constraints), Paths: nonNil(x.Paths), Validation: []string{},
		Incoming: []readmodel.Edge{}, Dispatches: []readmodel.DispatchRow{}, Results: []readmodel.WorkerResult{},
		Continuations: []readmodel.ContinuationRow{}, Validations: []readmodel.ValidationRun{}, FixAttempts: []readmodel.FixAttempt{},
		Checkpoints: []readmodel.Checkpoint{}, Events: []readmodel.TimelineRow{}}
	for _, k := range x.Deps {
		d.Incoming = append(d.Incoming, readmodel.Edge{FromKey: k, ToID: x.ID.String(), ToKey: x.Key})
	}
	d.Validation = validationCommandStrings(x.ValidationCommands)
	// Dispatches / results / continuations (Phase 12–14 read services).
	if s.d.Workers != nil {
		list, _, err := s.d.Workers.Dispatches(ctx, persistence.DispatchFilter{ProjectID: projectID, WorkUnitID: &unitID, Limit: 100})
		if err != nil {
			return nil, mapRead(err)
		}
		for _, w := range list {
			d.Dispatches = append(d.Dispatches, dispatchRowOf(w))
		}
	}
	var latestResult string
	if s.d.Results != nil {
		list, _, err := s.d.Results.Results(ctx, persistence.WorkerResultFilter{ProjectID: projectID, WorkUnitID: &unitID, Limit: 100})
		if err != nil {
			return nil, mapRead(err)
		}
		for _, r := range list {
			d.Results = append(d.Results, resultOf(r, x.Status))
		}
		if len(d.Results) > 0 {
			latestResult = d.Results[0].Status
		}
	}
	if s.d.Continuations != nil {
		list, _, err := s.d.Continuations.Continuations(ctx, persistence.ContinuationFilter{ProjectID: projectID, WorkUnitID: &unitID, Limit: 100})
		if err != nil {
			return nil, mapRead(err)
		}
		for _, c := range list {
			d.Continuations = append(d.Continuations, continuationOf(c))
		}
	}
	runs, err := s.d.Web.ValidationRuns(ctx, projectID, persistence.ValidationFilter{WorkUnitID: &unitID, Limit: 100})
	if err != nil {
		return nil, mapRead(err)
	}
	var latestRun *persistence.ValidationRunRow
	for i, r := range runs {
		if i == 0 {
			latestRun = &runs[0]
		}
		d.Validations = append(d.Validations, runOf(r))
	}
	var acc *execution.AcceptanceResult
	if latestRun != nil {
		acc = latestRun.Acceptance
	}
	d.Acceptance = acceptanceItems(acc, x.AcceptanceText)
	fixes, err := s.FixAttempts(ctx, projectID, nil, &unitID)
	if err != nil {
		return nil, err
	}
	d.FixAttempts = fixes
	cps, err := s.d.Web.Checkpoints(ctx, projectID, persistence.CheckpointFilter{TargetID: &unitID, Limit: 50})
	if err != nil {
		return nil, mapRead(err)
	}
	for _, c := range cps {
		d.Checkpoints = append(d.Checkpoints, s.checkpointOf(c, ""))
	}
	d.Unit.Stage = readmodel.StageOf(readmodel.UnitFacts{Status: x.Status, WorkerResult: latestResult, ValidationOutcome: x.WorkUnitRow.Validation,
		CheckpointPending:    x.WorkUnitRow.Validation == "PASS" && x.Checkpoint != "CREATED" && x.Checkpoint != "SKIPPED",
		ContinuationInFlight: hasActiveContinuation(d.Continuations)})
	d.Report = readmodel.ReportOf(readmodel.UnitFacts{Status: x.Status, WorkerResult: latestResult})
	d.Stages = UnitFlow(d.Unit.Stage)
	return d, nil
}

func hasActiveContinuation(cs []readmodel.ContinuationRow) bool {
	for _, c := range cs {
		switch c.Status {
		case "COMPLETED", "BLOCKED", "FAILED", "CANCELLED", "RECOVERY_REQUIRED":
		default:
			return true
		}
	}
	return false
}

// validationCommandStrings renders declared validation commands as data
// (program + args), never as shell strings (WBS 17.125).
func validationCommandStrings(raw []byte) []string {
	var specs []struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
	}
	out := []string{}
	if len(raw) == 0 || json.Unmarshal(raw, &specs) != nil {
		return out
	}
	for _, s := range specs {
		out = append(out, strings.TrimSpace(s.Program+" "+strings.Join(s.Args, " ")))
	}
	return out
}

// UnitFlow is the work-unit stage ladder (WBS 17.168): COMPLETE (worker
// report) and DONE (overseer decision) are distinct steps.
func UnitFlow(stage readmodel.UnitStage) readmodel.Flow {
	order := []readmodel.UnitStage{readmodel.StageReady, readmodel.StageExecuting, readmodel.StageIncomplete, readmodel.StageContinuing,
		readmodel.StageComplete, readmodel.StageValidating, readmodel.StageCheckpoint, readmodel.StageDone}
	labels := map[readmodel.UnitStage]string{
		readmodel.StageComplete: "COMPLETE (worker report)",
		readmodel.StageDone:     "DONE (validated, checkpointed)",
	}
	idx := -1
	for i, st := range order {
		if st == stage {
			idx = i
		}
	}
	if stage == readmodel.StagePending {
		idx = -1
	}
	f := readmodel.Flow{Name: "Work unit"}
	for i, st := range order {
		state := "PENDING"
		switch {
		case stage == readmodel.StageFailed || stage == readmodel.StageBlocked:
			state = "PENDING"
		case i < idx:
			state = "DONE"
		case i == idx && stage == readmodel.StageDone:
			state = "DONE"
		case i == idx:
			state = "ACTIVE"
		}
		// Incomplete / continuing are only visited on continuation paths.
		if (st == readmodel.StageIncomplete || st == readmodel.StageContinuing) && state == "DONE" && idx > 3 {
			state = "SKIPPED"
		}
		l := labels[st]
		if l == "" {
			l = string(st)
		}
		f.Steps = append(f.Steps, readmodel.FlowStep{Label: l, State: state})
	}
	if stage == readmodel.StageFailed || stage == readmodel.StageBlocked {
		f.Steps = append(f.Steps, readmodel.FlowStep{Label: string(stage), State: "FAILED"})
	}
	return f
}

// ---------------------------------------------------------------------------
// Worker (P17-55 / P17-56 / P17-57).

// WorkerPanel returns the worker session panel (WBS 17.82); transport
// credentials are never part of it.
func (s *ConsoleReadService) WorkerPanel(ctx context.Context, projectID uuid.UUID) (*readmodel.SessionPanel, error) {
	p := &readmodel.SessionPanel{}
	if s.d.Workers == nil {
		return p, nil
	}
	if _, err := s.d.Settings.GetSettings(ctx, projectID.String()); err != nil {
		return nil, mapRead(err)
	}
	v, err := s.d.Workers.View(ctx, projectID)
	if err != nil {
		if errors.Is(err, ErrWorkerNotConfigured) {
			return p, nil
		}
		return nil, mapRead(err)
	}
	p.Provider = string(v.Provider)
	p.Connection = string(v.Connection)
	p.Context = readmodel.NewContextUsage(int(v.Context.Tokens), int(v.Context.Window), 0)
	p.Context.Known = v.Context.Known
	if st, err := s.d.Settings.GetSettings(ctx, projectID.String()); err == nil {
		p.Model = st.WorkerModel
		p.Context.ResetThreshold = readmodel.NewContextUsage(0, 0, st.ContextThreshold).ResetThreshold
	}
	if v.SessionID != nil {
		p.Present, p.SessionID = true, v.SessionID.String()
		if v.SessionStatus != nil {
			p.Status = string(*v.SessionStatus)
		}
		if v.WorkingDirectory != nil {
			p.CWD = *v.WorkingDirectory
		}
		if sess, err := s.d.Workers.Session(ctx, projectID, *v.SessionID); err == nil && sess != nil {
			p.ProviderSess, p.CreatedAt, p.LastActivity = sess.ProviderSessionID, tptr(sess.CreatedAt), tptr(sess.LastSeenAt)
		}
		if st := s.d.Workers.ProviderSessionStats(ctx, projectID); st != nil {
			p.ProviderStats = &readmodel.ProviderSessionStats{Messages: st.Messages, ToolCalls: st.ToolCalls, ModelCalls: st.ModelCalls,
				InputTokens: st.InputTokens, OutputTokens: st.OutputTokens, CacheReadTokens: st.CacheReadTokens}
		}
	}
	return p, nil
}

// Dispatches returns a dispatch history page (WBS 17.83).
func (s *ConsoleReadService) Dispatches(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]readmodel.DispatchRow, readmodel.Page, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	out := []readmodel.DispatchRow{}
	if s.d.Workers == nil {
		return out, readmodel.Page{Limit: limit}, nil
	}
	list, total, err := s.d.Workers.Dispatches(ctx, persistence.DispatchFilter{ProjectID: projectID, Limit: limit, Offset: offset})
	if err != nil {
		return nil, readmodel.Page{}, mapRead(err)
	}
	for _, d := range list {
		out = append(out, dispatchRowOf(d))
	}
	return out, readmodel.Page{Total: total, Limit: limit, Offset: offset}, nil
}

// Result returns one structured worker result, labeled advisory beside the
// overseer's unit status (WBS 17.84 / 17.85).
func (s *ConsoleReadService) Result(ctx context.Context, projectID, resultID uuid.UUID) (*readmodel.WorkerResult, error) {
	if s.d.Results == nil {
		return nil, ErrNotFound
	}
	r, err := s.d.Results.Result(ctx, projectID, resultID)
	if err != nil {
		return nil, mapRead(err)
	}
	overseer := ""
	if r.Result.Metadata.WorkUnitID != nil {
		if u, err := s.d.Web.UnitDetail(ctx, projectID, *r.Result.Metadata.WorkUnitID); err == nil {
			overseer = u.Status
		}
	}
	out := resultOf(*r, overseer)
	return &out, nil
}

// Continuations lists continuations (and session resets) of the project
// (WBS 17.86 / 17.87).
func (s *ConsoleReadService) ContinuationList(ctx context.Context, projectID uuid.UUID, limit int) ([]readmodel.ContinuationRow, error) {
	out := []readmodel.ContinuationRow{}
	if s.d.Continuations == nil {
		return out, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, _, err := s.d.Continuations.Continuations(ctx, persistence.ContinuationFilter{ProjectID: projectID, Limit: limit})
	if err != nil {
		return nil, mapRead(err)
	}
	for _, c := range list {
		out = append(out, continuationOf(c))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Validation (P17-58).

// ValidationRuns lists runs (all attempts, failed ones included — WBS
// 17.90).
func (s *ConsoleReadService) ValidationRuns(ctx context.Context, projectID uuid.UUID, executionID, unitID *uuid.UUID, limit int) ([]readmodel.ValidationRun, error) {
	runs, err := s.d.Web.ValidationRuns(ctx, projectID, persistence.ValidationFilter{ExecutionID: executionID, WorkUnitID: unitID, Limit: limit})
	if err != nil {
		return nil, mapRead(err)
	}
	out := make([]readmodel.ValidationRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, runOf(r))
	}
	return out, nil
}

// HeadBytes is the output head shown inline (the rest is range-loaded).
const HeadBytes = 4096

// Validation returns one run with its commands (WBS 17.89, 17.144).
func (s *ConsoleReadService) Validation(ctx context.Context, projectID, runID uuid.UUID) (*readmodel.ValidationDetail, error) {
	run, cmds, err := s.d.Web.ValidationRun(ctx, projectID, runID, HeadBytes)
	if err != nil {
		return nil, mapRead(err)
	}
	d := &readmodel.ValidationDetail{Run: runOf(*run), Commands: []readmodel.CommandResult{}}
	for _, c := range cmds {
		args := c.Args
		if args == nil {
			args = []string{}
		}
		d.Commands = append(d.Commands, readmodel.CommandResult{ID: c.ID.String(), Ord: c.Ord, Name: c.Name, Executable: c.Program, Args: args,
			CWD: c.CWD, ExitCode: c.ExitCode, DurationMS: c.DurationMS, TimeoutMS: c.TimeoutMS, TimedOut: c.TimedOut, Outcome: c.Outcome,
			InfraReason: c.InfraReason, StdoutHead: c.StdoutHead, StderrHead: c.StderrHead, StdoutLen: c.StdoutLen, StderrLen: c.StderrLen,
			StdoutNext: len([]rune(c.StdoutHead)), StderrNext: len([]rune(c.StderrHead)),
			Truncated: c.StdoutLen > len([]rune(c.StdoutHead)) || c.StderrLen > len([]rune(c.StderrHead))})
	}
	return d, nil
}

// Output reads a range of one command's output (WBS 17.121 / 17.153).
func (s *ConsoleReadService) Output(ctx context.Context, projectID, resultID uuid.UUID, stream string, offset, length int) (*persistence.OutputRange, error) {
	if stream != "stdout" && stream != "stderr" {
		return nil, &FieldErrors{Fields: map[string]string{"stream": "must be stdout or stderr"}}
	}
	if offset < 0 || length < 0 || length > 64<<10 {
		return nil, &FieldErrors{Fields: map[string]string{"range": "offset ≥ 0 and 0 ≤ length ≤ 65536"}}
	}
	r, err := s.d.Web.ValidationOutputRange(ctx, projectID, resultID, stream, offset, length)
	if err != nil {
		return nil, mapRead(err)
	}
	return r, nil
}

// FixAttempts lists fix attempts with n / max from the captured engine
// settings (WBS 17.92).
func (s *ConsoleReadService) FixAttempts(ctx context.Context, projectID uuid.UUID, executionID, targetID *uuid.UUID) ([]readmodel.FixAttempt, error) {
	rows, err := s.d.Web.FixAttempts(ctx, projectID, executionID, targetID)
	if err != nil {
		return nil, mapRead(err)
	}
	maxBy := map[uuid.UUID]int{}
	out := make([]readmodel.FixAttempt, 0, len(rows))
	for _, r := range rows {
		mx, ok := maxBy[r.ExecutionID]
		if !ok {
			mx = execution.DefaultEngineSettings().MaxFixAttempts
			if c := s.snapshotContent(ctx, r.ExecutionID); c != nil {
				mx = c.EngineSettingsOrDefault().MaxFixAttempts
			}
			maxBy[r.ExecutionID] = mx
		}
		fa := readmodel.FixAttempt{ID: r.ID.String(), Attempt: r.Attempt, Max: mx, TargetKind: r.TargetKind, TargetID: r.TargetID.String(),
			FailedRunID: uid(r.FailedRunID), Role: r.DispatchRole, Status: r.DispatchStatus, DispatchID: uid(r.DispatchID), CreatedAt: r.CreatedAt.UTC()}
		if fa.Status == "" {
			fa.Status = "PENDING_DISPATCH"
		}
		fa.FailedCommand = failingCommands(r.Evidence)
		if len(fa.FailedCommand) > 0 {
			fa.Reason = strings.Join(fa.FailedCommand, ", ") + " failed"
		} else {
			fa.Reason = "validation failed"
		}
		out = append(out, fa)
	}
	return out, nil
}

func failingCommands(ev map[string]any) []string {
	var out []string
	if list, ok := ev["failed_commands"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	if list, ok := ev["records"].([]any); ok {
		for _, v := range list {
			m, _ := v.(map[string]any)
			if m == nil || m["Outcome"] == "PASS" || m["outcome"] == "PASS" {
				continue
			}
			name, _ := m["Command"].(string)
			if name == "" {
				name, _ = m["command"].(string)
			}
			if name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Checkpoints / repository (P17-59).

func (s *ConsoleReadService) checkpointOf(c persistence.CheckpointRow, policy string) readmodel.Checkpoint {
	short := c.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	return readmodel.Checkpoint{ID: c.ID.String(), ExecutionID: c.ExecutionID.String(), Kind: string(c.Kind), Policy: policy, Status: c.Status,
		Commit: c.Commit, ShortCommit: short, Parent: c.Parent, Tree: c.Tree, TargetID: uid(c.TargetID), ValidationRunID: uid(c.ValidationRunID),
		ValidationStatus: c.ValidationStatus, Error: c.Error, Key: c.Key, CreatedAt: c.CreatedAt.UTC(), CompletedAt: c.CompletedAt}
}

func (s *ConsoleReadService) policyFor(ctx context.Context, executionID uuid.UUID, kind execution.RepoCheckpointKind) string {
	if kind == execution.RepoCheckpointHardeningBaseline {
		return string(execution.CheckpointRequired)
	}
	if c := s.snapshotContent(ctx, executionID); c != nil {
		return string(c.EngineSettingsOrDefault().CheckpointPolicy)
	}
	return string(execution.DefaultEngineSettings().CheckpointPolicy)
}

// Checkpoints lists checkpoints (newest first).
func (s *ConsoleReadService) Checkpoints(ctx context.Context, projectID uuid.UUID, executionID *uuid.UUID, limit int) ([]readmodel.Checkpoint, error) {
	rows, err := s.d.Web.Checkpoints(ctx, projectID, persistence.CheckpointFilter{ExecutionID: executionID, Limit: limit})
	if err != nil {
		return nil, mapRead(err)
	}
	out := make([]readmodel.Checkpoint, 0, len(rows))
	policies := map[uuid.UUID]string{}
	for _, c := range rows {
		p, ok := policies[c.ExecutionID]
		if !ok {
			p = s.policyFor(ctx, c.ExecutionID, "")
			policies[c.ExecutionID] = p
		}
		if c.Kind == execution.RepoCheckpointHardeningBaseline {
			p = string(execution.CheckpointRequired)
		}
		out = append(out, s.checkpointOf(c, p))
	}
	return out, nil
}

// Checkpoint returns one checkpoint with its unit / validation relation
// (WBS 17.93, 17.145).
func (s *ConsoleReadService) Checkpoint(ctx context.Context, projectID, id uuid.UUID) (*readmodel.Checkpoint, error) {
	c, err := s.d.Web.Checkpoint(ctx, projectID, id)
	if err != nil {
		return nil, mapRead(err)
	}
	out := s.checkpointOf(*c, s.policyFor(ctx, c.ExecutionID, c.Kind))
	if c.TargetID != nil && c.Kind == execution.RepoCheckpointUnit {
		if u, err := s.d.Web.UnitDetail(ctx, projectID, *c.TargetID); err == nil {
			out.TargetLabel = strings.TrimSpace(u.Key + " " + u.Name)
		}
	}
	return &out, nil
}

// Repository returns the observational repository status (WBS 17.94 /
// 17.95); it never mutates the repository.
func (s *ConsoleReadService) Repository(ctx context.Context, projectID uuid.UUID) (*readmodel.RepositoryStatus, error) {
	st, err := s.d.Settings.GetSettings(ctx, projectID.String())
	if err != nil {
		return nil, mapRead(err)
	}
	r := &readmodel.RepositoryStatus{PreExistingChanges: []string{}, OverseerChanges: []string{}}
	if s.d.Inspector == nil || st.WorkingDirectory == "" {
		r.Error = "no working directory configured"
		return r, nil
	}
	snap, err := s.d.Inspector.Inspect(ctx, st.WorkingDirectory)
	if err != nil || !snap.IsGitRepository {
		r.Error = "repository unavailable"
		return r, nil
	}
	r.Available, r.Dirty = true, snap.Dirty
	if snap.Branch != nil {
		r.Branch = *snap.Branch
	}
	if snap.HeadCommit != nil {
		r.Head = *snap.HeadCommit
	}
	var paths []string
	if cp, ok := s.d.Inspector.(changedPathser); ok {
		paths, _ = cp.ChangedPaths(ctx, st.WorkingDirectory)
	}
	ov, err := s.d.Web.ProjectOverview(ctx, projectID)
	if err == nil && ov.ExecutionID != nil {
		if b, err := s.d.Execs.GetBaseline(ctx, *ov.ExecutionID, persistence.BaselineSnapshot); err == nil && b != nil {
			r.BaselineDirty = b.Snapshot.Dirty
			if b.Snapshot.HeadCommit != nil {
				r.BaselineHead = *b.Snapshot.HeadCommit
			}
			// Attribution is only possible when the baseline was clean: every
			// current change then post-dates the execution start.
			if !b.Snapshot.Dirty {
				r.ChangesTracked = true
				r.OverseerChanges = nonNil(paths)
			} else {
				r.PreExistingChanges = nonNil(paths)
			}
		}
		if cps, err := s.d.Web.Checkpoints(ctx, projectID, persistence.CheckpointFilter{ExecutionID: ov.ExecutionID, Limit: 1}); err == nil && len(cps) > 0 {
			c := s.checkpointOf(cps[0], s.policyFor(ctx, cps[0].ExecutionID, cps[0].Kind))
			r.LatestCheckpoint = &c
		}
	} else if len(paths) > 0 {
		r.PreExistingChanges = paths
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Recovery (P17-60).

// Recovery returns the recovery dashboard + read-only evidence + the
// backend-approved actions (WBS 17.96–17.98).
func (s *ConsoleReadService) Recovery(ctx context.Context, projectID, executionID uuid.UUID) (*readmodel.Recovery, error) {
	actions, rec, err := s.d.Recovery.Available(ctx, projectID, executionID)
	if err != nil {
		return nil, err
	}
	r := &readmodel.Recovery{ExecutionID: rec.ID.String(), ExecutionStatus: string(rec.Status), Reason: rec.RecoveryReason,
		Required: rec.Status == execution.ExecutionRecovery, Evidence: map[string]any{}, Events: []readmodel.TimelineRow{},
		Actions: []readmodel.RecoveryAction{}}
	r.AutomaticStopped = r.Required || rec.Status == execution.ExecutionPaused
	if r.Required {
		p := CurrentPlan(rec)
		r.Condition, r.Classification, r.PlanHash = string(p.Condition), string(p.Classification), p.Hash()
	}
	for k, v := range rec.RecoveryEvidence {
		r.Evidence[k] = v
	}
	for _, a := range actions {
		r.Actions = append(r.Actions, readmodel.RecoveryAction{Kind: string(a.Kind), Label: a.Label, HighImpact: a.HighImpact})
	}
	if rec.CurrentWorkUnitID != nil {
		if u, err := s.d.Web.UnitDetail(ctx, projectID, *rec.CurrentWorkUnitID); err == nil {
			r.AffectedUnit = &readmodel.UnitRef{ID: u.ID.String(), Key: u.Key, Name: u.Name, Status: u.Status, Kind: u.Kind}
		}
	}
	ds, ok := rec.RecoveryEvidence["dispatch_id"].(string)
	if !ok {
		ds, ok = rec.RecoveryEvidence["last_dispatch_id"].(string)
	}
	if ok && s.d.Workers != nil {
		if did, err := uuid.Parse(ds); err == nil {
			if dd, err := s.d.Workers.GetDispatch(ctx, projectID, did); err == nil && dd != nil {
				row := dispatchRowOf(dd.Dispatch)
				r.Dispatch, r.PromptHash = &row, dd.Dispatch.PromptHash
				if wp, err := s.d.Workers.DispatchPrompt(ctx, projectID, did); err == nil {
					r.Prompt = &readmodel.RecoveryPrompt{Content: wp.Content, Hash: wp.Hash, Source: string(wp.Source)}
				}
				if resp := dd.Response; resp != nil {
					r.Response = &readmodel.RecoveryResponse{Content: resp.Content, ContentHash: resp.ContentHash, CompletedAt: resp.CompletedAt}
				}
			}
		}
	}
	if rs, ok := rec.RecoveryEvidence["run_id"].(string); ok {
		if rid, err := uuid.Parse(rs); err == nil {
			if v, err := s.Validation(ctx, projectID, rid); err == nil {
				r.Validation = &v.Run
			}
		}
	}
	// Repository modified since the execution's baseline fingerprint.
	if repo, err := s.Repository(ctx, projectID); err == nil {
		r.Repository = repo
		if b, err := s.d.Execs.GetBaseline(ctx, rec.ID, persistence.BaselineSnapshot); err == nil && b != nil && s.d.Inspector != nil && repo.Available {
			if st, err := s.d.Settings.GetSettings(ctx, projectID.String()); err == nil {
				if snap, err := s.d.Inspector.Inspect(ctx, st.WorkingDirectory); err == nil {
					changed := snap.Fingerprint != b.Snapshot.Fingerprint
					r.RepositoryChanged = &changed
				}
			}
		}
	}
	evs, err := s.d.Web.TimelinePage(ctx, persistence.TimelineFilter{ExecutionID: rec.ID, Limit: 20,
		Types: execution.TypesInCategories([]execution.EventCategory{execution.EventCatRecovery, execution.EventCatWorker, execution.EventCatValidation})})
	if err == nil {
		for _, e := range evs {
			r.Events = append(r.Events, TimelineRowOf(e))
		}
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Timeline (P17-61).

// TimelineQuery selects one timeline page (WBS 17.80 / 17.81 / 17.151).
type TimelineQuery struct {
	Categories []string
	Before     int64
	Limit      int
}

// Timeline returns one keyset page of persisted events.
func (s *ConsoleReadService) Timeline(ctx context.Context, projectID, executionID uuid.UUID, q TimelineQuery) (*readmodel.Timeline, error) {
	if _, err := s.d.Execs.Get(ctx, nil, projectID, executionID); err != nil {
		return nil, mapRead(err)
	}
	var cats []execution.EventCategory
	for _, c := range q.Categories {
		if c == "" {
			continue
		}
		ec, ok := execution.ParseEventCategory(c)
		if !ok {
			return nil, &FieldErrors{Fields: map[string]string{"category": "unknown category " + c}}
		}
		cats = append(cats, ec)
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	if q.Before < 0 {
		return nil, &FieldErrors{Fields: map[string]string{"before": "must not be negative"}}
	}
	f := persistence.TimelineFilter{ExecutionID: executionID, BeforeSeq: q.Before, Limit: q.Limit}
	if len(cats) > 0 {
		f.Types = execution.TypesInCategories(cats)
	}
	evs, err := s.d.Web.TimelinePage(ctx, f)
	if err != nil {
		return nil, mapRead(err)
	}
	t := &readmodel.Timeline{ExecutionID: executionID.String(), Rows: make([]readmodel.TimelineRow, 0, len(evs)), Categories: []string{}}
	for _, c := range cats {
		t.Categories = append(t.Categories, string(c))
	}
	for _, e := range evs {
		t.Rows = append(t.Rows, TimelineRowOf(e))
	}
	if len(evs) == q.Limit && len(evs) > 0 {
		t.NextBefore = evs[len(evs)-1].ExecSeq
	}
	return t, nil
}

// PlanningFlow derives the planning live flow from persisted events
// (WBS 17.167): Planning → Waiting for worker → Plan received → Validating
// plan → Plan activated.
func PlanningFlow(phase string, recent []readmodel.TimelineRow) readmodel.Flow {
	f := readmodel.Flow{Name: "Planning " + phase}
	labels := []string{"Planning " + phase, "Waiting for worker", "Plan received", "Validating plan", "Plan activated"}
	reached := 0
	// recent is newest first; walk oldest → newest.
	for i := len(recent) - 1; i >= 0; i-- {
		e := recent[i]
		switch e.Type {
		case string(execution.EventDispatchIntent), string(execution.EventExecutionWaiting):
			if reached < 2 {
				reached = 2
			}
		case string(execution.EventStepIntent):
			if e.Status == string(execution.ActionPlanValidate) && reached < 4 {
				reached = 4
			} else if e.Status == string(execution.ActionPlanDispatch) && reached < 1 {
				reached = 1
			}
		case string(execution.EventPlanRejected):
			reached = 3
		case string(execution.EventPlanActivated), string(execution.EventPlanAmended):
			reached = 5
		}
		if reached == 2 && e.Type == string(execution.EventStepApplied) {
			reached = 3
		}
	}
	for i, l := range labels {
		st := "PENDING"
		switch {
		case i < reached-1 || reached == 5:
			st = "DONE"
		case i == reached-1:
			st = "ACTIVE"
		}
		f.Steps = append(f.Steps, readmodel.FlowStep{Label: l, State: st})
	}
	return f
}

// PhaseFlow is the phase completion flow (WBS 17.170): 100 % unit progress
// never equals DONE; phase DONE only after phase validation.
func PhaseFlow(p readmodel.PhaseRow, latestPhaseValidation string) readmodel.Flow {
	f := readmodel.Flow{Name: "Phase " + strconv.Itoa(p.Order)}
	units := "PENDING"
	if p.Progress.Total > 0 && p.Progress.Done == p.Progress.Total {
		units = "DONE"
	} else if p.Progress.Done > 0 {
		units = "ACTIVE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: fmt.Sprintf("Work Units %.1f%%", p.Progress.Percent), State: units})
	val := "PENDING"
	switch {
	case p.Status == "DONE":
		val = "DONE"
	case latestPhaseValidation == "RUNNING":
		val = "ACTIVE"
	case latestPhaseValidation == "FAIL":
		val = "FAILED"
	case units == "DONE":
		val = "ACTIVE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Phase Validation " + latestOr(latestPhaseValidation, "PENDING"), State: val})
	done := "PENDING"
	if p.Status == "DONE" {
		done = "DONE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Phase DONE", State: done})
	return f
}

func latestOr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ProjectFlow is the project completion gate order (WBS 17.171): All
// Phases DONE → Final Validation → Final Checkpoint → COMPLETED.
func ProjectFlow(phasesDone, phasesTotal int, finalValidation, finalCheckpoint, projectStatus string) readmodel.Flow {
	f := readmodel.Flow{Name: "Project completion"}
	all := "PENDING"
	if phasesTotal > 0 && phasesDone == phasesTotal {
		all = "DONE"
	} else if phasesDone > 0 {
		all = "ACTIVE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: fmt.Sprintf("All Phases DONE (%d / %d)", phasesDone, phasesTotal), State: all})
	fv := "PENDING"
	switch finalValidation {
	case "PASS":
		fv = "DONE"
	case "FAIL":
		fv = "FAILED"
	case "RUNNING":
		fv = "ACTIVE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Final Validation " + latestOr(finalValidation, "PENDING"), State: fv})
	fc := "PENDING"
	switch finalCheckpoint {
	case "CREATED", "SKIPPED":
		fc = "DONE"
	case "FAILED":
		fc = "FAILED"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Final Checkpoint " + latestOr(finalCheckpoint, "PENDING"), State: fc})
	c := "PENDING"
	if projectStatus == "COMPLETED" {
		c = "DONE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "COMPLETED", State: c})
	return f
}

// Flows returns the live flow panels of a project (WBS 17.167–17.171).
func (s *ConsoleReadService) Flows(ctx context.Context, projectID uuid.UUID) ([]readmodel.Flow, error) {
	ov, err := s.Overview(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var flows []readmodel.Flow
	phases, err := s.Phases(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var execID *uuid.UUID
	if ov.Execution != nil {
		id, _ := uuid.Parse(ov.Execution.ID)
		execID = &id
	}
	if ov.Phase != nil && execID != nil {
		page, err := s.d.Web.TimelinePage(ctx, persistence.TimelineFilter{ExecutionID: *execID, Limit: 50,
			Types: execution.TypesInCategories([]execution.EventCategory{execution.EventCatPlanning, execution.EventCatWorker, execution.EventCatSystem})})
		if err == nil {
			rows := make([]readmodel.TimelineRow, 0, len(page))
			for _, e := range page {
				rows = append(rows, TimelineRowOf(e))
			}
			flows = append(flows, PlanningFlow(strconv.Itoa(ov.Phase.Order), rows))
		}
	}
	if ov.CurrentUnit != nil {
		flows = append(flows, UnitFlow(ov.CurrentUnit.Stage))
	}
	for _, p := range phases {
		if !p.Current {
			continue
		}
		pid, _ := uuid.Parse(p.ID)
		latest := ""
		if runs, err := s.d.Web.ValidationRuns(ctx, projectID, persistence.ValidationFilter{PhaseID: &pid, Scope: "PHASE", Limit: 1}); err == nil && len(runs) > 0 {
			latest = runs[0].Status
		}
		flows = append(flows, PhaseFlow(p, latest))
	}
	finalVal, finalCP := "", ""
	if execID != nil {
		if runs, err := s.d.Web.ValidationRuns(ctx, projectID, persistence.ValidationFilter{ExecutionID: execID, Scope: "FINAL", Limit: 1}); err == nil && len(runs) > 0 {
			finalVal = runs[0].Status
		}
		if cps, err := s.d.Web.Checkpoints(ctx, projectID, persistence.CheckpointFilter{ExecutionID: execID, Limit: 20}); err == nil {
			for _, c := range cps {
				if c.Kind == execution.RepoCheckpointFinal {
					finalCP = c.Status
					break
				}
			}
		}
	}
	flows = append(flows, ProjectFlow(ov.PhasesDone, ov.PhasesTotal, finalVal, finalCP, ov.Status))
	return flows, nil
}

// sortStrings is a small helper used by renderers.
func sortStrings(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

var _ = sortStrings
var _ = workerresult.StatusComplete
