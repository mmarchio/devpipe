package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AppConfigSnapshotRecord is one startup configuration snapshot
// (WBS 18.3): redacted canonical configuration, its hash and the
// per-section hashes used for drift reporting.
type AppConfigSnapshotRecord struct {
	ID            uuid.UUID
	CreatedAt     time.Time
	InstanceID    string
	Profile       string
	AppVersion    string
	SchemaVersion int
	ConfigHash    string
	Sections      map[string]string
	Config        []byte
}

// AppConfigSnapshotRepository persists startup snapshots (append-only).
type AppConfigSnapshotRepository interface {
	Insert(ctx context.Context, r *AppConfigSnapshotRecord) error
	// Latest returns the most recent snapshot (ErrNotFound when none).
	Latest(ctx context.Context) (*AppConfigSnapshotRecord, error)
	// ByHash returns a snapshot with the given config hash.
	ByHash(ctx context.Context, hash string) (*AppConfigSnapshotRecord, error)
}

// ScanExecution is an active (or recovery-required) execution as the
// startup recovery scan sees it (WBS 18.6).
type ScanExecution struct {
	ID, ProjectID   uuid.UUID
	Status          string
	DriverOwner     string
	DriverExpiresAt *time.Time
	RecoveryReason  string
	Evidence        map[string]any
}

// ScanDispatch is an unfinished or ambiguous worker dispatch.
type ScanDispatch struct {
	ID, ProjectID uuid.UUID
	ExecutionID   *uuid.UUID
	Status        string
	Delivery      string
	StateKey      string
	CreatedAt     time.Time
}

// ScanItem is any other unfinished durable operation.
type ScanItem struct {
	ID, ProjectID uuid.UUID
	ExecutionID   *uuid.UUID
	Kind          string // RUNNING_VALIDATION / PENDING_CHECKPOINT / INTENT_STEP / ACTIVE_CONTINUATION / PRECOMPUTE_JOB
	Detail        map[string]any
}

// RecoveryScanData is everything unfinished in the database.
type RecoveryScanData struct {
	Executions []ScanExecution
	Dispatches []ScanDispatch
	Items      []ScanItem
}

// RecoveryScanRepository reads unfinished durable operations in one
// read-only snapshot.
type RecoveryScanRepository interface {
	Scan(ctx context.Context) (*RecoveryScanData, error)
}

// RecoveryPlanRecord is a persisted recovery plan (WBS 18.9).
type RecoveryPlanRecord struct {
	ID             uuid.UUID
	ExecutionID    uuid.UUID
	ProjectID      uuid.UUID
	PlanHash       string
	Condition      string
	Classification string
	Plan           []byte
	Source         string
	CreatedAt      time.Time
}

// RecoveryActionRecord is one recovery action execution (WBS 18.11 /
// 18.182).
type RecoveryActionRecord struct {
	ID             uuid.UUID
	ExecutionID    uuid.UUID
	ProjectID      uuid.UUID
	IdempotencyKey string
	Action         string
	PlanHash       string
	EvidenceHash   string
	ActorID        string
	ActorName      string
	Reason         string
	Confirmed      bool
	CorrelationID  string
	Status         string
	Result         map[string]any
	Error          string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

// DispatchResolutionRecord is an operator resolution of an unknown
// delivery (WBS 18.23, PLAN Decision 8).
type DispatchResolutionRecord struct {
	ID          uuid.UUID
	DispatchID  uuid.UUID
	ExecutionID uuid.UUID
	ProjectID   uuid.UUID
	Resolution  string
	ActionID    uuid.UUID
	ActorID     string
	Reason      string
	Evidence    map[string]any
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

// RecoveryStore persists recovery plans, actions and resolutions.
type RecoveryStore interface {
	// RecordPlan stores a plan (idempotent on execution + hash).
	RecordPlan(ctx context.Context, p *RecoveryPlanRecord) error
	ListPlans(ctx context.Context, executionID uuid.UUID) ([]RecoveryPlanRecord, error)
	// BeginAction inserts a RUNNING action; when the idempotency key is
	// already used it returns the existing record and ErrConflict.
	BeginAction(ctx context.Context, a *RecoveryActionRecord) (*RecoveryActionRecord, error)
	FinishAction(ctx context.Context, id uuid.UUID, status string, result map[string]any, errText string) error
	GetActionByKey(ctx context.Context, key string) (*RecoveryActionRecord, error)
	ListActions(ctx context.Context, executionID uuid.UUID) ([]RecoveryActionRecord, error)
	InsertResolution(ctx context.Context, r *DispatchResolutionRecord) error
	GetResolution(ctx context.Context, dispatchID uuid.UUID) (*DispatchResolutionRecord, error)
	ConsumeResolution(ctx context.Context, ex Executor, dispatchID uuid.UUID) (bool, error)
	// ReleaseResolution un-consumes a resolution whose resend was
	// provably not sent.
	ReleaseResolution(ctx context.Context, dispatchID uuid.UUID) error
}
