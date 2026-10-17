package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
)

// WorkerSessionRecord is the overseer's knowledge of one external worker
// session (WBS 12.17). WorkingDirectory nil = UNKNOWN.
type WorkerSessionRecord struct {
	ID                uuid.UUID
	ProjectID         uuid.UUID
	Provider          worker.WorkerProvider
	ProviderSessionID string
	Status            worker.SessionStatus
	Policy            worker.SessionPolicy
	WorkingDirectory  *string
	Usage             worker.WorkerUsage
	StatusReason      string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	ClosedAt          *time.Time
}

// WorkerSessionRepository persists worker sessions (WBS 12.18). Every
// read is project-scoped: another project's session is ErrNotFound.
type WorkerSessionRepository interface {
	Create(ctx context.Context, s *WorkerSessionRecord) error
	Get(ctx context.Context, projectID, id uuid.UUID) (*WorkerSessionRecord, error)
	GetActiveForProject(ctx context.Context, projectID uuid.UUID) (*WorkerSessionRecord, error)
	ListForProject(ctx context.Context, projectID uuid.UUID) ([]WorkerSessionRecord, error)
	ListActive(ctx context.Context) ([]WorkerSessionRecord, error)
	UpdateLastSeen(ctx context.Context, projectID, id uuid.UUID, at time.Time) error
	// UpdateWorkingDirectory records the last CONFIRMED directory of an
	// ACTIVE session.
	UpdateWorkingDirectory(ctx context.Context, projectID, id uuid.UUID, dir *string, at time.Time) error
	UpdateUsage(ctx context.Context, projectID, id uuid.UUID, u worker.WorkerUsage) error
	MarkReset(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error)
	MarkLost(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error)
	MarkClosed(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error)
	// ReplaceActive atomically marks prev (when ACTIVE) RESET and inserts
	// next as ACTIVE (WBS 12.21/12.64). On any failure nothing changes.
	ReplaceActive(ctx context.Context, next *WorkerSessionRecord, prev *uuid.UUID, at time.Time) error
}

// WorkerSettingsRepository persists per-project worker settings.
type WorkerSettingsRepository interface {
	Get(ctx context.Context, projectID uuid.UUID) (worker.Settings, bool, error)
	Upsert(ctx context.Context, projectID uuid.UUID, s worker.Settings) error
}

// DispatchUpdate carries the fields a status transition sets.
type DispatchUpdate struct {
	Delivery     *worker.DeliveryState
	RetryClass   *worker.RetryClass
	ErrorCode    string
	ErrorMessage string
	ProviderCode string
	RetryAfter   *time.Duration
	Diagnostic   string
	Owner        string
	StartedAt    *time.Time
	CompletedAt  *time.Time
	At           time.Time
}

// DispatchFilter selects dispatches (WBS 12.83).
type DispatchFilter struct {
	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	StateKey   string
	RoleKey    string
	Status     worker.DispatchStatus
	SessionID  *uuid.UUID
	// ContinuationID selects a continuation's dispatches (WBS 14.35).
	ContinuationID *uuid.UUID
	Limit          int
	Offset         int
}

// WorkerDispatchRepository persists dispatch attempts (WBS 12.30).
type WorkerDispatchRepository interface {
	// Create stores a PENDING dispatch; a duplicate idempotency identity
	// is ErrConflict (WBS 12.33).
	Create(ctx context.Context, d *worker.WorkerDispatch) error
	// Transition moves from → to (compare-and-set on the current
	// status); false when the dispatch was not in from.
	Transition(ctx context.Context, id uuid.UUID, from, to worker.DispatchStatus, u DispatchUpdate) (bool, error)
	Get(ctx context.Context, projectID, id uuid.UUID) (*worker.WorkerDispatch, error)
	List(ctx context.Context, f DispatchFilter) ([]worker.WorkerDispatch, int, error)
	ListAttempts(ctx context.Context, projectID uuid.UUID, operationID string) ([]worker.WorkerDispatch, error)
	// ListInFlight returns PENDING/SENDING/WAITING dispatches (all
	// projects when projectID is nil).
	ListInFlight(ctx context.Context, projectID *uuid.UUID) ([]worker.WorkerDispatch, error)
}

// WorkerUsageObservation is one persisted telemetry observation (WBS
// 12.50).
type WorkerUsageObservation struct {
	ID         uuid.UUID
	Seq        int64
	ProjectID  uuid.UUID
	SessionID  uuid.UUID
	DispatchID *uuid.UUID
	Usage      worker.WorkerUsage
	Source     worker.UsageSource
	ObservedAt time.Time
}

// WorkerResponseRepository persists raw responses and usage observations.
type WorkerResponseRepository interface {
	// CompleteWithResponse inserts the raw response and moves the
	// dispatch WAITING → COMPLETED in one transaction; false (nothing
	// stored) when the dispatch was not WAITING.
	CompleteWithResponse(ctx context.Context, r *worker.WorkerResponseRecord, u DispatchUpdate) (bool, error)
	GetByDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*worker.WorkerResponseRecord, error)
	InsertObservation(ctx context.Context, o *WorkerUsageObservation) error
	ListObservations(ctx context.Context, projectID uuid.UUID, sessionID *uuid.UUID) ([]WorkerUsageObservation, error)
	LatestObservation(ctx context.Context, projectID uuid.UUID, sessionID uuid.UUID) (*WorkerUsageObservation, error)
}

// WorkerSessionLease is a session dispatch lease (WBS 12.57).
type WorkerSessionLease struct {
	SessionID  uuid.UUID
	ProjectID  uuid.UUID
	Owner      string
	DispatchID *uuid.UUID
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// WorkerLeaseRepository coordinates session ownership across instances.
type WorkerLeaseRepository interface {
	// Acquire takes the lease when free or expired.
	Acquire(ctx context.Context, l WorkerSessionLease) (bool, error)
	Renew(ctx context.Context, sessionID uuid.UUID, owner string, expires time.Time) (bool, error)
	Release(ctx context.Context, sessionID uuid.UUID, owner string) (bool, error)
	Get(ctx context.Context, sessionID uuid.UUID) (*WorkerSessionLease, error)
	ReleaseExpired(ctx context.Context, now time.Time) (int, error)
}

// WorkerEventRecord is one audit event (WBS 12.53).
type WorkerEventRecord struct {
	ID         uuid.UUID
	Seq        int64
	ProjectID  uuid.UUID
	SessionID  *uuid.UUID
	DispatchID *uuid.UUID
	Type       worker.EventType
	Payload    map[string]any
	CreatedAt  time.Time
}

// WorkerToolEventRecord is one tool activity record (WBS 12.47).
type WorkerToolEventRecord struct {
	ID         uuid.UUID
	ProjectID  uuid.UUID
	DispatchID uuid.UUID
	Activity   worker.ToolActivity
	CreatedAt  time.Time
}

// WorkerEventFilter selects events.
type WorkerEventFilter struct {
	ProjectID  uuid.UUID
	SessionID  *uuid.UUID
	DispatchID *uuid.UUID
}

// WorkerEventRepository persists audit and tool events (append-only).
type WorkerEventRepository interface {
	Append(ctx context.Context, e *WorkerEventRecord) error
	List(ctx context.Context, f WorkerEventFilter) ([]WorkerEventRecord, error)
	AppendTool(ctx context.Context, t *WorkerToolEventRecord) error
	ListTools(ctx context.Context, projectID, dispatchID uuid.UUID) ([]WorkerToolEventRecord, error)
}
