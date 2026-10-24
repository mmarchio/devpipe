package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/audit"
	"overseer/internal/execution"
)

// ---------------------------------------------------------------------------
// Phase 17 persistence: audit log, drift acknowledgements and the bounded
// web read-model queries (P17-27 .. P17-38).

// AuditRepository persists audit events (P17-27).
type AuditRepository interface {
	// AppendAudit inserts one event (ex nil = own statement). ID / Seq
	// are assigned.
	AppendAudit(ctx context.Context, ex Executor, e *audit.Event) error
	// ListAudit returns one keyset page (seq DESC) matching the filter
	// (already validated).
	ListAudit(ctx context.Context, f audit.Filter) ([]audit.Event, error)
	// GetAudit returns one event (ErrNotFound when absent).
	GetAudit(ctx context.Context, id uuid.UUID) (*audit.Event, error)
}

// DriftAcknowledgement is an explicit "Ignore" of one filesystem content
// hash of a drifted state file (P17-38).
type DriftAcknowledgement struct {
	ID             uuid.UUID
	ProjectID      uuid.UUID
	StateFileID    uuid.UUID
	FSHash         string
	AcknowledgedBy string
	Reason         string
	CreatedAt      time.Time
}

// DriftAckRepository persists drift acknowledgements (P17-38).
type DriftAckRepository interface {
	// AcknowledgeDrift records the acknowledgement; an existing row for the
	// same (file, hash) is returned unchanged (idempotent).
	AcknowledgeDrift(ctx context.Context, a *DriftAcknowledgement) (*DriftAcknowledgement, error)
	// GetDriftAck returns the acknowledgement of (file, hash) or
	// ErrNotFound.
	GetDriftAck(ctx context.Context, stateFileID uuid.UUID, fsHash string) (*DriftAcknowledgement, error)
}

// DashboardCounts are the dashboard aggregates (P17-30).
type DashboardCounts struct {
	ProjectsByStatus   map[string]int
	ProjectsTotal      int
	RecoveryRequired   int
	ActiveExecutions   int
	ActiveSessions     int
	ActiveDispatches   int
	RunningValidations int
}

// ProjectPageFilter selects a project list page (P17-31).
type ProjectPageFilter struct {
	Status string
	// Name is a case-insensitive substring.
	Name string
	// Active: nil = any; true = with an active execution; false = without.
	Active *bool
	// Recovery: only projects whose active execution requires recovery.
	Recovery bool
	Limit    int
	Offset   int
}

// ProjectListRow is one project list row (P17-31).
type ProjectListRow struct {
	ID               uuid.UUID
	Name             string
	Status           string
	UpdatedAt        time.Time
	PhaseID          *uuid.UUID
	PhaseOrder       int
	PhaseName        string
	PhaseStatus      string
	Done             int
	Total            int
	StateKey         string
	UnitID           *uuid.UUID
	UnitKey          string
	UnitName         string
	ExecutionStatus  string
	LastActivity     time.Time
	ActiveExecution  bool
	RecoveryRequired bool
}

// ProjectOverviewRow bundles the overview facts (P17-32).
type ProjectOverviewRow struct {
	ProjectID      uuid.UUID
	Name           string
	Description    string
	Status         string
	ProjectVersion int
	UpdatedAt      time.Time

	ExecutionID       *uuid.UUID
	ExecutionNumber   int
	ExecutionStatus   string
	Generation        int
	WaitReason        string
	RecoveryReason    string
	RecoveryEvidence  map[string]any
	ExecutionActive   bool
	ExecutionStarted  *time.Time
	ExecutionComplete *time.Time

	PhaseID       *uuid.UUID
	PhaseOrder    int
	PhaseName     string
	PhaseStatus   string
	WBSVersion    int
	PhasesTotal   int
	PhasesDone    int
	PlanID        *uuid.UUID
	PlanVersion   int
	PlanWBS       int
	PlanStatus    string
	PlanKind      string
	UnitsDone     int
	UnitsTotal    int
	UnitID        *uuid.UUID
	UnitKey       string
	UnitName      string
	UnitStatus    string
	UnitKind      string
	UnitStartedAt *time.Time
	// UnitCompletedAt is set once the unit is DONE.
	UnitCompletedAt *time.Time

	StateKey     string
	StateName    string
	StateKind    string
	StateEntered *time.Time

	ContextTokens *int64
	ContextWindow *int64
	ContextAt     *time.Time

	// Current-task facts (P17-54) and the unit display stage inputs.
	UnitAttempts       int
	SessionID          *uuid.UUID
	UnitWorkerResult   string
	UnitValidation     string
	UnitCheckpoint     string
	ContinuationActive bool
}

// WorkUnitPageFilter selects a work-unit page of one plan (P17-33).
type WorkUnitPageFilter struct {
	PlanID uuid.UUID
	Status string
	Kind   string
	// Readiness "READY" (not DONE, every dependency DONE) or "BLOCKED"
	// (some dependency not DONE).
	Readiness string
	// Validation is the latest validation run status (PASS, FAIL, ...).
	Validation string
	Limit      int
	Offset     int
}

// WorkUnitRow is one work-unit table row (P17-33).
type WorkUnitRow struct {
	ID          uuid.UUID
	Sequence    int
	Key         string
	Kind        string
	Name        string
	Status      string
	Deps        []string
	Blocking    []string
	Attempts    int
	Validation  string
	Checkpoint  string
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// PlanEdge is one persisted dependency edge (P17-34).
type PlanEdge struct {
	FromID  uuid.UUID
	FromKey string
	ToID    uuid.UUID
	ToKey   string
}

// OutputRange is a slice of a validation command's output (P17-35).
type OutputRange struct {
	Stream string
	Offset int
	Text   string
	// Total is the full output length in characters.
	Total int
}

// TimelineFilter selects one timeline page (P17-36).
type TimelineFilter struct {
	ExecutionID uuid.UUID
	// Types restricts the event types (empty = all).
	Types []execution.EventType
	// BeforeSeq returns events with exec_seq < BeforeSeq (0 = newest).
	BeforeSeq int64
	Limit     int
}

// SchedulerCounts are the scheduler status aggregates (P17-37).
type SchedulerCounts struct {
	Runnable           int
	Waiting            int
	ActiveDispatches   int
	RunningValidations int
}

// ValidationFilter selects validation runs (newest first).
type ValidationFilter struct {
	ExecutionID *uuid.UUID
	WorkUnitID  *uuid.UUID
	PhaseID     *uuid.UUID
	Scope       string
	Limit       int
}

// ValidationRunRow is a validation run without command output.
type ValidationRunRow struct {
	ID          uuid.UUID
	ExecutionID uuid.UUID
	Scope       string
	WorkUnitID  *uuid.UUID
	PhaseID     *uuid.UUID
	Label       string
	Attempt     int
	Status      string
	CWD         string
	Acceptance  *execution.AcceptanceResult
	Commands    int
	Failed      int
	StartedAt   time.Time
	FinishedAt  *time.Time
}

// CommandResultRow is one command result with bounded output heads.
type CommandResultRow struct {
	ID          uuid.UUID
	Ord         int
	Name        string
	Program     string
	Args        []string
	CWD         string
	ExitCode    *int
	DurationMS  int64
	TimeoutMS   int64
	TimedOut    bool
	Outcome     string
	InfraReason string
	StdoutHead  string
	StderrHead  string
	StdoutLen   int
	StderrLen   int
}

// CheckpointFilter selects checkpoints (newest first).
type CheckpointFilter struct {
	ExecutionID *uuid.UUID
	TargetID    *uuid.UUID
	Limit       int
}

// CheckpointRow is a checkpoint with its validation run status.
type CheckpointRow struct {
	RepoCheckpointRecord
	ValidationStatus string
}

// FixAttemptRow is a fix attempt with its dispatch status.
type FixAttemptRow struct {
	FixAttemptRecord
	DispatchStatus   string
	DispatchDelivery string
	DispatchRole     string
}

// PhaseRow is one phase with its current plan and progress (P17-51).
type PhaseRow struct {
	ID          uuid.UUID
	Order       int
	Name        string
	Description string
	Status      string
	WBSVersion  int
	WBSHash     string
	PlanID      *uuid.UUID
	PlanVersion int
	PlanWBS     int
	PlanStatus  string
	PlanKind    string
	Done        int
	Total       int
	StartedAt   *time.Time
	CompletedAt *time.Time
	UpdatedAt   time.Time
}

// UnitDetailRow is one work unit's definition (P17-53).
type UnitDetailRow struct {
	WorkUnitRow
	ProjectID          uuid.UUID
	PlanID             uuid.UUID
	PhaseID            uuid.UUID
	PlanVersion        int
	Objective          string
	Instructions       string
	Description        string
	AcceptanceText     []string
	AcceptanceChecks   []byte
	RequiredTests      []string
	Constraints        []string
	ValidationCommands []byte
	Paths              []string
	ClaimedExecution   *uuid.UUID
	DoneRunID          *uuid.UUID
}

// ProjectMetricsRow are the execution / context / planning aggregates of a
// project (P17-63, P17-64). Nil pointers mean "no data".
type ProjectMetricsRow struct {
	UnitsDone              int
	UnitsTotal             int
	AvgUnitSeconds         *float64
	SingleInvocationDone   int
	UnitsDispatched        int
	Continuations          int
	Resets                 int
	ValidationRuns         int
	ValidationFails        int
	FixAttempts            int
	AvgContextAtCompletion *float64
	UnitsByKind            map[string]int
	AvgDependenciesPerUnit *float64
	PlanAmendments         int
}

// PhaseMetricsRow is one phase's aggregates (P17-64).
type PhaseMetricsRow struct {
	PhaseID            uuid.UUID
	Order              int
	Name               string
	Status             string
	DurationSeconds    *float64
	Units              int
	PlanAttempts       int
	Continuations      int
	ValidationAttempts int
	Fixes              int
}

// WebReadRepository serves the bounded web read-model queries.
type WebReadRepository interface {
	// ListProjectEventsAfter lists a project's events with project_seq >
	// afterSeq in order (P17-29).
	ListProjectEventsAfter(ctx context.Context, projectID uuid.UUID, afterSeq int64, limit int) ([]execution.Event, error)
	// MaxProjectSeq returns the latest project_seq (0 when none).
	MaxProjectSeq(ctx context.Context, projectID uuid.UUID) (int64, error)
	DashboardCounts(ctx context.Context) (DashboardCounts, error)
	ProjectListPage(ctx context.Context, f ProjectPageFilter) ([]ProjectListRow, int, error)
	ProjectOverview(ctx context.Context, projectID uuid.UUID) (*ProjectOverviewRow, error)
	WorkUnitPage(ctx context.Context, f WorkUnitPageFilter) ([]WorkUnitRow, int, error)
	PlanKindCounts(ctx context.Context, planID uuid.UUID) (map[string]int, error)
	PlanEdges(ctx context.Context, planID uuid.UUID, limit, offset int) ([]PlanEdge, int, error)
	// ValidationOutputRange reads a slice of one command result's output
	// without loading the rest (P17-35); the result must belong to the
	// project (ErrNotFound otherwise).
	ValidationOutputRange(ctx context.Context, projectID, resultID uuid.UUID, stream string, offset, length int) (*OutputRange, error)
	TimelinePage(ctx context.Context, f TimelineFilter) ([]execution.Event, error)
	SchedulerCounts(ctx context.Context) (SchedulerCounts, error)
	// ValidationRuns lists runs of a project without output.
	ValidationRuns(ctx context.Context, projectID uuid.UUID, f ValidationFilter) ([]ValidationRunRow, error)
	// ValidationRun returns one run with command results (output heads of
	// headLen characters only).
	ValidationRun(ctx context.Context, projectID, runID uuid.UUID, headLen int) (*ValidationRunRow, []CommandResultRow, error)
	Checkpoints(ctx context.Context, projectID uuid.UUID, f CheckpointFilter) ([]CheckpointRow, error)
	Checkpoint(ctx context.Context, projectID, id uuid.UUID) (*CheckpointRow, error)
	FixAttempts(ctx context.Context, projectID uuid.UUID, executionID *uuid.UUID, targetID *uuid.UUID) ([]FixAttemptRow, error)
	// PhaseRows lists a project's phases by persisted phase_order with their
	// current plans and DONE / total counts, in one query.
	PhaseRows(ctx context.Context, projectID uuid.UUID) ([]PhaseRow, error)
	// UnitDetail returns one project-scoped unit (ErrNotFound otherwise).
	UnitDetail(ctx context.Context, projectID, unitID uuid.UUID) (*UnitDetailRow, error)
	ProjectMetrics(ctx context.Context, projectID uuid.UUID) (*ProjectMetricsRow, error)
	PhaseMetrics(ctx context.Context, projectID uuid.UUID) ([]PhaseMetricsRow, error)
}

// RestoreState is the restore gate of a restored database (P18-24).
type RestoreState struct {
	BackupID        uuid.UUID      `json:"backup_id"`
	BackupCreatedAt time.Time      `json:"backup_created_at"`
	SchemaVersion   int            `json:"schema_version"`
	Status          string         `json:"status"`
	RestoredAt      time.Time      `json:"restored_at"`
	ValidatedAt     *time.Time     `json:"validated_at,omitempty"`
	Detail          map[string]any `json:"detail"`
	EnabledAt       *time.Time     `json:"enabled_at,omitempty"`
	EnabledBy       string         `json:"enabled_by,omitempty"`
	EnableReason    string         `json:"enable_reason,omitempty"`
}
