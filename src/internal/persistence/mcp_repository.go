package persistence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Phase 19: the overseer MCP server's records and database state
// authority.

// Task report kinds: which check state reads the report.
const (
	ReportImplement  = "IMPLEMENT"
	ReportTest       = "TEST"
	ReportAcceptance = "ACCEPTANCE"
	ReportPlan       = "PLAN"
	ReportAnalysis   = "ANALYSIS"
)

// Task report statuses.
const (
	ReportComplete   = "COMPLETE"
	ReportIncomplete = "INCOMPLETE"
	ReportBlocked    = "BLOCKED"
	ReportFailed     = "FAILED"
)

// TaskReportRecord is the team's structured result of one dispatch
// (submit_result). Immutable.
type TaskReportRecord struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	ExecutionID uuid.UUID
	DispatchID  uuid.UUID
	WorkUnitID  *uuid.UUID
	RoleKey     string
	StateKey    string
	Kind        string
	Status      string
	// CriteriaMet holds one entry per acceptance check of the work unit,
	// in specification order.
	CriteriaMet []bool
	Summary     string
	Evidence    map[string]any
	CreatedAt   time.Time
}

// MCPTokenRecord binds a capability token (stored as its SHA-256) to one
// command prompt; a tool call acts for the prompt's in-flight dispatch.
type MCPTokenRecord struct {
	TokenHash      string
	WorkerPromptID uuid.UUID
	ProjectID      uuid.UUID
	ExecutionID    uuid.UUID
	WorkUnitID     *uuid.UUID
	RoleKey        string
	StateKey       string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RevokedAt      *time.Time
}

// MCPRequestRecord is an MCP mutation request kept by idempotency key.
type MCPRequestRecord struct {
	ID             uuid.UUID
	DispatchID     uuid.UUID
	Tool           string
	IdempotencyKey string
	PayloadHash    string
	Result         json.RawMessage
	CreatedAt      time.Time
}

// Outbox kinds and statuses.
const (
	OutboxEngineWake    = "ENGINE_WAKE"
	OutboxContextReset  = "CONTEXT_RESET"
	OutboxBlockerNotify = "BLOCKER_NOTIFY"

	OutboxPending = "PENDING"
	OutboxDone    = "DONE"
	OutboxDead    = "DEAD"
)

// OutboxRecord is a follow-up action committed with the workflow change
// that requires it.
type OutboxRecord struct {
	ID            uuid.UUID
	ProjectID     uuid.UUID
	ExecutionID   *uuid.UUID
	Kind          string
	Payload       map[string]any
	DedupeKey     string
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	ClaimedBy     string
	ClaimedUntil  *time.Time
	LastError     string
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

// MCPRepository persists the Phase 19 records. Methods taking an Executor
// run inside the caller's transaction (nil = the repository's pool).
type MCPRepository interface {
	// CreateToken stores a command prompt's token; a second token for the
	// prompt is ErrConflict.
	CreateToken(ctx context.Context, ex Executor, rec *MCPTokenRecord) error
	// PromptHasToken reports whether the command prompt carries a token.
	PromptHasToken(ctx context.Context, ex Executor, promptID uuid.UUID) (bool, error)
	// GetToken returns the token by hash (ErrNotFound when unknown).
	GetToken(ctx context.Context, ex Executor, hash string) (*MCPTokenRecord, error)
	// RevokeExecutionTokens revokes the execution's tokens (already
	// revoked ones are left as is).
	RevokeExecutionTokens(ctx context.Context, ex Executor, executionID uuid.UUID, at time.Time) error
	// InFlightDispatch returns the prompt's dispatch being sent or awaiting
	// its reply (ErrNotFound when none is).
	InFlightDispatch(ctx context.Context, ex Executor, promptID uuid.UUID) (uuid.UUID, error)

	// GetRequest returns a request by idempotency key (ErrNotFound).
	GetRequest(ctx context.Context, ex Executor, dispatchID uuid.UUID, tool, key string) (*MCPRequestRecord, error)
	// CreateRequest stores a request; a concurrent duplicate key is
	// ErrConflict.
	CreateRequest(ctx context.Context, ex Executor, rec *MCPRequestRecord) error

	// CreateTaskReport stores a report; a second report of the kind for
	// the dispatch is ErrConflict.
	CreateTaskReport(ctx context.Context, ex Executor, rec *TaskReportRecord) error
	// GetTaskReport returns the dispatch's report of the kind
	// (ErrNotFound).
	GetTaskReport(ctx context.Context, ex Executor, dispatchID uuid.UUID, kind string) (*TaskReportRecord, error)
	// ListTaskReports returns the execution's reports (only the unit's
	// when unitID is set), oldest first.
	ListTaskReports(ctx context.Context, ex Executor, executionID uuid.UUID, unitID *uuid.UUID) ([]*TaskReportRecord, error)

	// ArchiveUnit records the unit's archive membership once (a unit
	// already archived is left as is); dispatchID is the accepting dispatch
	// (nil when imported at the cutover or by the migration that retired
	// the statefiles).
	ArchiveUnit(ctx context.Context, ex Executor, unitID uuid.UUID, dispatchID *uuid.UUID, at time.Time) error
	// ArchivedUnits returns the archived units of a plan.
	ArchivedUnits(ctx context.Context, ex Executor, planID uuid.UUID) (map[uuid.UUID]bool, error)

	// EnqueueOutbox stores an action; an existing dedupe key is left as
	// is (the action is already queued).
	EnqueueOutbox(ctx context.Context, ex Executor, rec *OutboxRecord) error
	// ClaimOutbox leases up to limit due PENDING actions to owner.
	ClaimOutbox(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]*OutboxRecord, error)
	// FinishOutbox marks a claimed action DONE.
	FinishOutbox(ctx context.Context, id uuid.UUID, owner string, at time.Time) error
	// FailOutbox records a failed attempt: retried at next, or DEAD when
	// dead is set.
	FailOutbox(ctx context.Context, id uuid.UUID, owner, msg string, next time.Time, dead bool, at time.Time) error
	// ListOutbox returns the project's actions, newest first.
	ListOutbox(ctx context.Context, projectID uuid.UUID, limit int) ([]*OutboxRecord, error)
}
