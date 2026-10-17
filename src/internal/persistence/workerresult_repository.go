package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/workerresult"
)

// ResultProtocolSettingsRepository persists per-project response-protocol
// settings (WBS 13.96). Get returns defaults with stored=false when absent.
type ResultProtocolSettingsRepository interface {
	Get(ctx context.Context, projectID uuid.UUID) (workerresult.Settings, bool, error)
	Upsert(ctx context.Context, projectID uuid.UUID, s workerresult.Settings) error
}

// StoredWorkerResult is an accepted, persisted worker result.
type StoredWorkerResult struct {
	ID        uuid.UUID
	CreatedAt time.Time
	Result    workerresult.WorkerResult
}

// WorkerResultFilter selects results (WBS 13.66/13.67).
type WorkerResultFilter struct {
	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	StateKey   string
	RoleKey    string
	Status     workerresult.WorkerResultStatus
	SessionID  *uuid.UUID
	Limit      int
	Offset     int
}

// WorkerResultRepository persists accepted results (WBS 13.43/13.44).
type WorkerResultRepository interface {
	// CreateAccepted stores the result with its ordered children in one
	// transaction. If the dispatch already has an accepted result it is
	// returned unchanged with created=false (WBS 13.46).
	CreateAccepted(ctx context.Context, id uuid.UUID, r *workerresult.WorkerResult, at time.Time) (*StoredWorkerResult, bool, error)
	Get(ctx context.Context, projectID, id uuid.UUID) (*StoredWorkerResult, error)
	GetByDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*StoredWorkerResult, error)
	List(ctx context.Context, f WorkerResultFilter) ([]StoredWorkerResult, int, error)
}

// ParseAttemptRecord is one parse attempt (WBS 13.45).
type ParseAttemptRecord struct {
	ID                  uuid.UUID
	DispatchID          uuid.UUID
	ProjectID           uuid.UUID
	RawResponseHash     string
	Outcome             workerresult.AttemptOutcome
	ErrorCode           workerresult.WorkerResultErrorCode
	ValidationErrors    []workerresult.ValidationError
	ResultStatus        workerresult.WorkerResultStatus
	NormalizedHash      string
	ResultID            *uuid.UUID
	DiffersFromAccepted bool
	ParseMode           workerresult.ParseMode
	RepairMode          workerresult.RepairMode
	Repaired            bool
	RepairMethod        *string
	ParserVersion       string
	Reparse             bool
	RoleKey             string
	StateKey            string
	WorkerProvider      string
	WorkerModel         string
	StartedAt           time.Time
	CompletedAt         time.Time
}

// ParseAttemptFilter selects attempts.
type ParseAttemptFilter struct {
	ProjectID  uuid.UUID
	DispatchID *uuid.UUID
	Outcome    workerresult.AttemptOutcome
	Limit      int
	Offset     int
}

// UnparsedDispatch is a COMPLETED dispatch with a stored response and no
// parse attempt (WBS 13.88).
type UnparsedDispatch struct {
	DispatchID uuid.UUID
	ProjectID  uuid.UUID
}

// ParseAttemptRepository persists parse attempts (append-only).
type ParseAttemptRepository interface {
	Create(ctx context.Context, a *ParseAttemptRecord) error
	Get(ctx context.Context, projectID, id uuid.UUID) (*ParseAttemptRecord, error)
	List(ctx context.Context, f ParseAttemptFilter) ([]ParseAttemptRecord, int, error)
	ListUnparsed(ctx context.Context, limit int) ([]UnparsedDispatch, error)
}
