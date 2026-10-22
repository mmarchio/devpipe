package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/wbs"
)

// PositionUpdate is a versioned change of an execution's engine position
// (P16-66). Zero values leave a column untouched unless its Set flag is
// true.
type PositionUpdate struct {
	// Status "" = unchanged; a change is validated against the status
	// table and audited.
	Status execution.ExecutionStatus
	Reason string

	SetPhase bool
	PhaseID  *uuid.UUID
	SetUnit  bool
	UnitID   *uuid.UUID

	RuntimeStateID *uuid.UUID

	SetWait    bool
	WaitReason string

	// SetRecovery records (or, with an empty reason, clears) the recovery
	// reason and evidence.
	SetRecovery      bool
	RecoveryReason   string
	RecoveryEvidence map[string]any

	// SessionID records the current worker session.
	SessionID *uuid.UUID
}

// EventRef identifies one execution event: its position, time and payload.
type EventRef struct {
	Seq     int64
	At      time.Time
	Payload map[string]any
}

// StepRecord is one durable engine step (WBS 16.8, 16.83).
type StepRecord struct {
	ID                uuid.UUID
	ExecutionID       uuid.UUID
	ProjectID         uuid.UUID
	Seq               int64
	Action            execution.StepAction
	StateKey          string
	RuntimeVersion    int
	Attempt           int
	IdempotencyKey    string
	Status            execution.StepStatus
	PhaseID           *uuid.UUID
	WorkUnitID        *uuid.UUID
	DispatchID        *uuid.UUID
	ContextSnapshotID *uuid.UUID
	WorkerPromptID    *uuid.UUID
	ValidationRunID   *uuid.UUID
	CheckpointID      *uuid.UUID
	PlanID            *uuid.UUID
	ContinuationID    *uuid.UUID
	Outcome           string
	Detail            map[string]any
	Error             string
	IntentAt          time.Time
	FinishedAt        *time.Time
}

// StepRefs are the artifact references a step records (nil = unchanged).
type StepRefs struct {
	DispatchID        *uuid.UUID
	ContextSnapshotID *uuid.UUID
	WorkerPromptID    *uuid.UUID
	ValidationRunID   *uuid.UUID
	CheckpointID      *uuid.UUID
	PlanID            *uuid.UUID
	ContinuationID    *uuid.UUID
	Detail            map[string]any
}

// PlanCandidateRecord is a persisted planner candidate (WBS 16.15–16.17).
type PlanCandidateRecord struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	ExecutionID uuid.UUID
	PhaseID     uuid.UUID
	DispatchID  *uuid.UUID
	WBSVersion  int
	WBSHash     string
	Mode        string
	// Payload is the raw candidate JSON (nil = missing / malformed).
	Payload []byte
	// Markdown is the planner's task file the payload was parsed from
	// (nil = none read).
	Markdown  *string
	Status    string
	Report    wbs.PlanProblems
	PlanID    *uuid.UUID
	CreatedAt time.Time
	DecidedAt *time.Time
}

// Plan candidate statuses.
const (
	CandidatePending  = "PENDING"
	CandidateAccepted = "ACCEPTED"
	CandidateRejected = "REJECTED"
)

// ValidationRunRecord is one deterministic validation run (WBS 16.44).
type ValidationRunRecord struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	ExecutionID uuid.UUID
	Scope       execution.ValidationScope
	WorkUnitID  *uuid.UUID
	PhaseID     *uuid.UUID
	TargetID    *uuid.UUID
	Label       string
	Attempt     int
	// Status is RUNNING, a ValidationOutcome, or ABANDONED.
	Status     string
	CWD        string
	Acceptance *execution.AcceptanceResult
	Records    []execution.ValidationRecord
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Validation run statuses besides the outcomes.
const (
	RunRunning   = "RUNNING"
	RunAbandoned = "ABANDONED"
)

// FixAttemptRecord is one bounded automatic repair (WBS 16.50).
type FixAttemptRecord struct {
	ID          uuid.UUID
	ExecutionID uuid.UUID
	TargetKind  string
	TargetID    uuid.UUID
	FailedRunID *uuid.UUID
	Attempt     int
	DispatchID  *uuid.UUID
	Evidence    map[string]any
	CreatedAt   time.Time
}

// Fix attempt target kinds.
const (
	FixTargetUnit      = "UNIT"
	FixTargetHardening = "HARDENING"
)

// RepoCheckpointRecord is an overseer-owned git checkpoint (WBS 16.54).
type RepoCheckpointRecord struct {
	ID              uuid.UUID
	ProjectID       uuid.UUID
	ExecutionID     uuid.UUID
	Kind            execution.RepoCheckpointKind
	TargetID        *uuid.UUID
	Key             string
	Status          string
	Commit          string
	Parent          string
	Tree            string
	ValidationRunID *uuid.UUID
	Error           string
	CreatedAt       time.Time
	CompletedAt     *time.Time
}

// Checkpoint statuses.
const (
	CheckpointPending = "PENDING"
	CheckpointCreated = "CREATED"
	CheckpointFailed  = "FAILED"
	CheckpointSkipped = "SKIPPED"
)

// HardeningPassRecord is a persisted hardening pass (WBS 16.88).
type HardeningPassRecord struct {
	hardening.Pass
	BaselineFingerprint string
	RepoFingerprint     string
	RecoveryReason      string
	UpdatedAt           time.Time
}

// HardeningGroupRecord is one analysed duplicate group of a round.
type HardeningGroupRecord struct {
	ID               uuid.UUID
	PassID           uuid.UUID
	Round            int
	Key              string
	Fingerprint      string
	Members          []hardening.FunctionInfo
	Classification   hardening.Classification
	Reason           string
	Destination      *hardening.Destination
	Status           hardening.GroupStatus
	Matrix           []hardening.MatrixRow
	Tests            []hardening.TestInfo
	Lineage          *hardening.Lineage
	CheckpointID     *uuid.UUID
	TestsBefore      int
	TestsAfter       int
	RefsMigrated     int
	FunctionsRemoved int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// HardeningOperationRecord is one planned consolidation operation.
type HardeningOperationRecord struct {
	ID              uuid.UUID
	GroupID         uuid.UUID
	Seq             int
	Kind            hardening.OpKind
	Package         string
	Status          hardening.OpStatus
	Evidence        map[string]any
	RepoFingerprint string
	ValidationRunID *uuid.UUID
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// CompletionRecord is the immutable completion record (WBS 16.156–16.158).
type CompletionRecord struct {
	ExecutionID          uuid.UUID
	ProjectID            uuid.UUID
	FinalPhaseID         *uuid.UUID
	HardeningPassID      *uuid.UUID
	FinalValidationRunID *uuid.UUID
	FinalCheckpointID    *uuid.UUID
	CompletedAt          time.Time
}

// SchedulableExecution is a scheduler candidate row.
type SchedulableExecution struct {
	execution.SchedulingCandidate
	Status execution.ExecutionStatus
}

// EngineRepository persists Phase 16 engine state (P16-64 … P16-75).
// Methods taking an Executor run inside the caller's transaction (nil =
// the repository's pool).
type EngineRepository interface {
	// Settings (P16-64).
	GetEngineSettings(ctx context.Context, projectID uuid.UUID) (execution.EngineSettings, hardening.Settings, bool, error)
	UpsertEngineSettings(ctx context.Context, projectID uuid.UUID, s execution.EngineSettings, h hardening.Settings) error

	// Driver lease (P16-65): Acquire returns the fencing epoch.
	AcquireDriver(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (int64, bool, error)
	RenewDriver(ctx context.Context, id uuid.UUID, owner string, epoch int64, now time.Time, ttl time.Duration) (bool, error)
	ReleaseDriver(ctx context.Context, id uuid.UUID, owner string, epoch int64) error
	// CheckDriver locks the execution row and verifies the lease (fencing);
	// a lost lease is ErrConflict.
	CheckDriver(ctx context.Context, ex Executor, id uuid.UUID, owner string, epoch int64, now time.Time) error

	// Position (P16-66).
	UpdatePosition(ctx context.Context, ex Executor, id uuid.UUID, expectedVersion int, upd PositionUpdate) (*ExecutionRecord, error)
	ListSchedulable(ctx context.Context, now time.Time, limit int) ([]SchedulableExecution, error)
	TouchScheduled(ctx context.Context, id uuid.UUID, now time.Time) error
	ListEngineActive(ctx context.Context) ([]*ExecutionRecord, error)

	// Steps (P16-67).
	AppendStepIntent(ctx context.Context, ex Executor, rec *StepRecord) (*StepRecord, bool, error)
	FinishStep(ctx context.Context, ex Executor, id uuid.UUID, status execution.StepStatus, outcome, errMsg string, refs StepRefs) error
	SetStepRefs(ctx context.Context, ex Executor, id uuid.UUID, refs StepRefs) error
	GetStep(ctx context.Context, id uuid.UUID) (*StepRecord, error)
	LatestStep(ctx context.Context, ex Executor, executionID uuid.UUID) (*StepRecord, error)
	ListPendingSteps(ctx context.Context, executionID uuid.UUID) ([]*StepRecord, error)
	ListSteps(ctx context.Context, executionID uuid.UUID, limit, offset int) ([]*StepRecord, int, error)
	FindStepByKey(ctx context.Context, ex Executor, key string) (*StepRecord, error)

	// Events (P16-68).
	ListTimeline(ctx context.Context, executionID uuid.UUID) ([]execution.Event, error)

	// Work units (P16-69 / P16-70).
	ClaimUnit(ctx context.Context, ex Executor, unitID, executionID uuid.UUID) error
	SetUnitStatus(ctx context.Context, ex Executor, unitID uuid.UUID, from []wbs.WorkUnitStatus, to wbs.WorkUnitStatus) error
	MarkUnitDone(ctx context.Context, ex Executor, unitID, executionID, runID uuid.UUID) error
	ListPlanUnits(ctx context.Context, ex Executor, planID uuid.UUID) ([]*wbs.WorkUnit, error)
	UnitDependencies(ctx context.Context, ex Executor, planID uuid.UUID) (map[uuid.UUID][]uuid.UUID, error)

	// Plans (P16-71).
	CreateCandidate(ctx context.Context, ex Executor, rec *PlanCandidateRecord) (*PlanCandidateRecord, error)
	GetCandidateByDispatch(ctx context.Context, ex Executor, dispatchID uuid.UUID) (*PlanCandidateRecord, error)
	LatestCandidate(ctx context.Context, ex Executor, executionID, phaseID uuid.UUID) (*PlanCandidateRecord, error)
	DecideCandidate(ctx context.Context, ex Executor, id uuid.UUID, status string, report wbs.PlanProblems, planID *uuid.UUID) error
	CountRejectedCandidates(ctx context.Context, ex Executor, executionID, phaseID uuid.UUID, since *time.Time) (int, error)
	// LastLimitReset returns when a recovery caused by the attempt limit
	// was last resolved in the execution (nil when never): the limit's
	// counter counts from there.
	LastLimitReset(ctx context.Context, ex Executor, executionID uuid.UUID, limit string) (*time.Time, error)
	// ReplanUnit replaces a unit's specification with its replanned one
	// and drops its manual-criterion approvals (the criteria changed).
	ReplanUnit(ctx context.Context, ex Executor, unitID uuid.UUID, spec *wbs.WorkUnitSpecification) error
	// LastEvent returns the execution's newest event of type typ — when
	// workUnitID is set, the newest whose payload names that unit — or
	// nil.
	LastEvent(ctx context.Context, ex Executor, executionID uuid.UUID, typ execution.EventType, workUnitID *uuid.UUID) (*EventRef, error)
	// ClearUnitApprovals drops a unit's manual-criterion approvals (a
	// requeued task is reviewed again).
	ClearUnitApprovals(ctx context.Context, ex Executor, unitID uuid.UUID) error
	// LastRecoveryResolved returns when the execution's latest recovery
	// with the given reason was resolved (nil when never).
	LastRecoveryResolved(ctx context.Context, ex Executor, executionID uuid.UUID, reason string) (*time.Time, error)
	// CreatePlanWithUnits inserts the plan, its units and dependencies
	// (keys → ids) in the caller's transaction.
	CreatePlanWithUnits(ctx context.Context, ex Executor, plan *wbs.ExecutionPlan, units []wbs.PlannedUnit) ([]*wbs.WorkUnit, error)
	SupersedePlans(ctx context.Context, ex Executor, phaseID uuid.UUID, except uuid.UUID) error
	ListPhasePlans(ctx context.Context, ex Executor, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)

	// Validation (P16-72).
	StartValidationRun(ctx context.Context, ex Executor, rec *ValidationRunRecord) error
	FinishValidationRun(ctx context.Context, ex Executor, id uuid.UUID, status string, records []execution.ValidationRecord, acceptance *execution.AcceptanceResult) error
	GetValidationRun(ctx context.Context, id uuid.UUID) (*ValidationRunRecord, error)
	LatestValidationRun(ctx context.Context, ex Executor, executionID uuid.UUID, scope execution.ValidationScope, target uuid.UUID) (*ValidationRunRecord, error)
	ListRunningValidations(ctx context.Context, executionID uuid.UUID) ([]*ValidationRunRecord, error)
	ListValidationRuns(ctx context.Context, executionID uuid.UUID) ([]*ValidationRunRecord, error)
	CountValidationRuns(ctx context.Context, executionID uuid.UUID, scope execution.ValidationScope, target uuid.UUID) (int, error)
	CreateFixAttempt(ctx context.Context, ex Executor, rec *FixAttemptRecord) error
	SetFixDispatch(ctx context.Context, ex Executor, id, dispatchID uuid.UUID) error
	ListFixAttempts(ctx context.Context, executionID uuid.UUID, target *uuid.UUID) ([]*FixAttemptRecord, error)
	ApproveCriterion(ctx context.Context, executionID, unitID uuid.UUID, index int, by, note string) error
	Approvals(ctx context.Context, ex Executor, unitID uuid.UUID) (map[int]bool, error)
	// RecordStatefileHash stores the MD5 of a verified statefile and
	// returns the hash it replaces ("" when the file was not verified
	// before in the execution).
	RecordStatefileHash(ctx context.Context, executionID uuid.UUID, path, md5 string) (string, error)

	// Checkpoints (P16-73).
	CreateCheckpointIntent(ctx context.Context, rec *RepoCheckpointRecord) (*RepoCheckpointRecord, error)
	CompleteCheckpoint(ctx context.Context, id uuid.UUID, status, commit, parent, tree, errMsg string) (*RepoCheckpointRecord, error)
	GetRepoCheckpoint(ctx context.Context, id uuid.UUID) (*RepoCheckpointRecord, error)
	GetCheckpointByKey(ctx context.Context, key string) (*RepoCheckpointRecord, error)
	ListRepoCheckpoints(ctx context.Context, executionID uuid.UUID) ([]*RepoCheckpointRecord, error)

	// Hardening (P16-74).
	CreatePass(ctx context.Context, rec *HardeningPassRecord) (*HardeningPassRecord, bool, error)
	GetPass(ctx context.Context, ex Executor, executionID uuid.UUID, generation int) (*HardeningPassRecord, error)
	GetPassByID(ctx context.Context, id uuid.UUID) (*HardeningPassRecord, error)
	UpdatePass(ctx context.Context, ex Executor, rec *HardeningPassRecord) (*HardeningPassRecord, error)
	ListPasses(ctx context.Context, executionID uuid.UUID) ([]*HardeningPassRecord, error)
	UpsertGroup(ctx context.Context, rec *HardeningGroupRecord) (*HardeningGroupRecord, error)
	UpdateGroup(ctx context.Context, rec *HardeningGroupRecord) error
	ListGroups(ctx context.Context, passID uuid.UUID, round int) ([]*HardeningGroupRecord, error)
	GetGroup(ctx context.Context, id uuid.UUID) (*HardeningGroupRecord, error)
	CreateOperations(ctx context.Context, groupID uuid.UUID, ops []hardening.Operation) error
	ListOperations(ctx context.Context, groupID uuid.UUID) ([]*HardeningOperationRecord, error)
	UpdateOperation(ctx context.Context, rec *HardeningOperationRecord) error
	UpsertReport(ctx context.Context, passID uuid.UUID, r hardening.Report) error
	GetReport(ctx context.Context, passID uuid.UUID) (*hardening.Report, error)
	UpsertRetained(ctx context.Context, projectID uuid.UUID, fingerprint, classification, reason string) error
	ListRetained(ctx context.Context, projectID uuid.UUID) (map[string]string, error)

	// Completion (P16-75).
	CreateCompletion(ctx context.Context, ex Executor, rec *CompletionRecord) (*CompletionRecord, bool, error)
	GetCompletion(ctx context.Context, executionID uuid.UUID) (*CompletionRecord, error)
}
