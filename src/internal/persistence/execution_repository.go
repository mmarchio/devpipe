package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

// ExecutionRecord is the persisted execution plus its bootstrap metadata
// (WBS 15.1, 15.9, 15.86, 15.156).
type ExecutionRecord struct {
	execution.Execution

	WBSVersion int
	WBSHash    string
	PlanID     *uuid.UUID
	PlanState  execution.PlanState

	InitialStateKey         string
	FirstActionableStateID  *uuid.UUID
	FirstActionableStateKey string
	InitialRoleKey          string
	InitialWorkUnitID       *uuid.UUID
	InitialTemplateKey      string

	InitialContextSnapshotID *uuid.UUID
	InitialWorkerPromptID    *uuid.UUID
	InitialCacheKey          string
	InitialCanonicalKey      string

	PlanHash string

	Stage         execution.BootstrapStage
	FailedStage   execution.BootstrapStage
	FailureCode   execution.BootstrapErrorCode
	FailureClass  execution.FailureClass
	FailureReason string

	ClaimOwner     string
	ClaimExpiresAt *time.Time

	ReadyAt   *time.Time
	RunningAt *time.Time

	// Reproducibility metadata recorded at bootstrap (WBS 18.206).
	AppVersion    string
	SchemaVersion int
	AppConfigHash string

	// Engine position and control (P16-55).
	CurrentPhaseID         *uuid.UUID
	CurrentWorkUnitID      *uuid.UUID
	RuntimeStateID         *uuid.UUID
	DriverOwner            string
	DriverExpiresAt        *time.Time
	DriverEpoch            int64
	LastScheduledAt        *time.Time
	WaitReason             string
	RecoveryReason         string
	RecoveryEvidence       map[string]any
	PausedAt               *time.Time
	FinalizationGeneration int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ExecutionMutation are the bootstrap artifact fields an AdvanceStage may
// set. Nil pointers leave a column untouched.
type ExecutionMutation struct {
	FirstActionableStateID   *uuid.UUID
	FirstActionableStateKey  *string
	InitialContextSnapshotID *uuid.UUID
	InitialWorkerPromptID    *uuid.UUID
	InitialCacheKey          *string
	InitialCanonicalKey      *string
	ClearFailure             bool
}

// StageTransition is one bootstrap history row (WBS 15.10).
type StageTransition struct {
	ID          uuid.UUID
	ExecutionID uuid.UUID
	Sequence    int
	FromStage   *execution.BootstrapStage
	ToStage     execution.BootstrapStage
	Status      execution.ExecutionStatus
	Reason      string
	CreatedAt   time.Time
}

// ConfigurationSnapshotRecord is a persisted execution configuration
// snapshot (WBS 15.40).
type ConfigurationSnapshotRecord struct {
	ID             uuid.UUID
	ExecutionID    uuid.UUID
	ProjectID      uuid.UUID
	ProjectVersion int
	Canonical      []byte
	Hash           string
	CreatedAt      time.Time
}

// BaselineKind distinguishes the snapshot-time and final baselines.
type BaselineKind string

// Baseline kinds (WBS 15.80/15.127).
const (
	BaselineSnapshot BaselineKind = "SNAPSHOT"
	BaselineFinal    BaselineKind = "FINAL"
)

// BaselineRecord is a persisted repository baseline.
type BaselineRecord struct {
	ID          uuid.UUID
	ExecutionID uuid.UUID
	Kind        BaselineKind
	Snapshot    execution.RepositorySnapshot
	Hash        string
	CapturedAt  time.Time
}

// CheckpointRecord is a persisted bootstrap checkpoint (WBS 15.149).
type CheckpointRecord struct {
	ID         uuid.UUID
	Checkpoint execution.BootstrapCheckpoint
	Hash       string
	CreatedAt  time.Time
}

// ExecutionRepository persists executions and their bootstrap artifacts.
// Methods taking an Executor run inside the caller's transaction (nil = the
// repository's pool).
type ExecutionRepository interface {
	GetSettings(ctx context.Context, projectID uuid.UUID) (execution.Settings, bool, error)
	UpsertSettings(ctx context.Context, projectID uuid.UUID, s execution.Settings) error

	// LockProject serializes bootstrap per project (advisory xact lock).
	LockProject(ctx context.Context, ex Executor, projectID uuid.UUID) error
	// Create allocates the next number and inserts the execution with its
	// initial stage history (VALIDATING → SNAPSHOTTING).
	Create(ctx context.Context, ex Executor, rec *ExecutionRecord) error
	Get(ctx context.Context, ex Executor, projectID, id uuid.UUID) (*ExecutionRecord, error)
	GetByID(ctx context.Context, ex Executor, id uuid.UUID) (*ExecutionRecord, error)
	GetActive(ctx context.Context, ex Executor, projectID uuid.UUID) (*ExecutionRecord, error)
	ListByProject(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]*ExecutionRecord, int, error)

	AdvanceStage(ctx context.Context, ex Executor, id uuid.UUID, expectedVersion int, to execution.BootstrapStage, reason string, mut *ExecutionMutation) (*ExecutionRecord, error)
	UpdateStatus(ctx context.Context, ex Executor, id uuid.UUID, expectedVersion int, to execution.ExecutionStatus, reason string) (*ExecutionRecord, error)
	// SetArtifacts is a versioned CAS of the artifact columns without a
	// stage change (e.g. the context snapshot recorded mid-stage).
	SetArtifacts(ctx context.Context, ex Executor, id uuid.UUID, expectedVersion int, mut *ExecutionMutation) (*ExecutionRecord, error)
	RecordFailure(ctx context.Context, ex Executor, id uuid.UUID, expectedVersion int, failedStage execution.BootstrapStage, code execution.BootstrapErrorCode, class execution.FailureClass, reason string, terminal bool) (*ExecutionRecord, error)
	ListStageHistory(ctx context.Context, executionID uuid.UUID) ([]StageTransition, error)

	Claim(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (bool, error)
	Release(ctx context.Context, id uuid.UUID, owner string) error
	ListRecoverable(ctx context.Context, now time.Time) ([]*ExecutionRecord, error)

	CreateSnapshot(ctx context.Context, ex Executor, rec *ConfigurationSnapshotRecord) error
	GetSnapshot(ctx context.Context, executionID uuid.UUID) (*ConfigurationSnapshotRecord, error)

	// CreateBaseline is idempotent per (execution, kind): an existing row
	// is returned unchanged.
	CreateBaseline(ctx context.Context, rec *BaselineRecord) (*BaselineRecord, error)
	GetBaseline(ctx context.Context, executionID uuid.UUID, kind BaselineKind) (*BaselineRecord, error)

	CreateCheckpoint(ctx context.Context, ex Executor, rec *CheckpointRecord) error
	GetCheckpoint(ctx context.Context, executionID uuid.UUID) (*CheckpointRecord, error)

	AppendEvent(ctx context.Context, ex Executor, e *execution.Event) error
	ListEvents(ctx context.Context, projectID uuid.UUID, executionID *uuid.UUID) ([]execution.Event, error)

	// Artifact hashes for checkpoint verification (WBS 15.150).
	StateFileVersionHashes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
	WorkerPromptHash(ctx context.Context, id uuid.UUID) (string, error)

	// StatesWithProtocol returns which of the states pin a response
	// protocol (Phase 8 composition requirement, WBS 15.38).
	StatesWithProtocol(ctx context.Context, stateIDs []uuid.UUID) (map[uuid.UUID]bool, error)

	// ActivityCounts reads worker/continuation activity for the read model
	// (WBS 15.103/15.147): dispatches, continuations, confirmed resets,
	// no-progress stops.
	ActivityCounts(ctx context.Context, projectID uuid.UUID) (dispatches, continuations, resets, noProgress int, err error)

	// Runtime binding (WBS 15.53–15.56).
	GetRuntimeByExecution(ctx context.Context, ex Executor, executionID uuid.UUID) (*RuntimeStateRecord, error)
	BindRuntime(ctx context.Context, ex Executor, rec *RuntimeStateRecord) (*RuntimeStateRecord, error)
	AppendHistory(ctx context.Context, ex Executor, rec *TransitionHistoryRecord) error
	ListExecutionHistory(ctx context.Context, executionID uuid.UUID) ([]*TransitionHistoryRecord, error)
}
