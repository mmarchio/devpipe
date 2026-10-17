package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/workerresult"
)

// Continuation persistence errors.
var (
	// ErrStaleVersion: a versioned continuation update lost the race
	// (WBS 14.54).
	ErrStaleVersion = errors.New("persistence: stale continuation version")
	// ErrAlreadyConsumed: the WorkerResult was already consumed by the
	// consumer (WBS 14.101).
	ErrAlreadyConsumed = errors.New("persistence: worker result already consumed")
)

// ConsumerContinuation is the consumer identity of Phase 14.
const ConsumerContinuation = "continuation"

// ResultConsumption records that a WorkerResult was consumed (WBS 14.102).
type ResultConsumption struct {
	ResultID       uuid.UUID
	Consumer       string
	ProjectID      uuid.UUID
	ContinuationID *uuid.UUID
	ConsumedAt     time.Time
}

// ContinuationMutation are the fields an Advance / Update may change.
// Nil pointers leave a column untouched.
type ContinuationMutation struct {
	Decision *continuation.Decision

	SessionID          *uuid.UUID
	OldSessionID       *uuid.UUID
	NewSessionID       *uuid.UUID
	SessionConfirmedAt *time.Time
	// ClearSession drops the confirmed new session and CWD confirmation
	// (a fresh-session restart inside the same continuation).
	ClearSession          bool
	CWDConfirmedAt        *time.Time
	ClearCWD              bool
	SessionCreateFailures *int

	SnapshotID     *uuid.UUID
	WorkerPromptID *uuid.UUID
	DispatchID     *uuid.UUID
	ClearDispatch  bool

	RecoveryReason   *continuation.RecoveryReason
	OverrideStrategy *continuation.Strategy
	ClearOverride    bool
	FailureMessage   *string
	NoProgressStreak *int
	ScopeExpanded    *bool
}

// ContinuationFilter selects continuations (WBS 14.81).
type ContinuationFilter struct {
	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	Status     continuation.Status
	Strategy   continuation.Strategy
	Limit      int
	Offset     int
}

// ContinuationTransition is one history row (WBS 14.53).
type ContinuationTransition struct {
	ID             uuid.UUID
	ContinuationID uuid.UUID
	Sequence       int
	From           *continuation.Status
	To             continuation.Status
	Reason         string
	CreatedAt      time.Time
}

// WorkUnitCounts are per-work-unit counters (WBS 14.36-14.39).
type WorkUnitCounts struct {
	Continuations         int
	MaxAttempt            int
	SuccessfulResets      int
	SessionCreateFailures int
	// Latest is the newest continuation of the unit (nil = none).
	Latest *continuation.Continuation
}

// UnconsumedResult is an accepted result Phase 14 has not consumed yet.
type UnconsumedResult struct {
	ResultID       uuid.UUID
	DispatchID     uuid.UUID
	ProjectID      uuid.UUID
	Status         workerresult.WorkerResultStatus
	ContinuationID *uuid.UUID
}

// ContinuationRepository persists continuations, their transition
// history, leases and result consumption (WBS 14.90).
type ContinuationRepository interface {
	// Create inserts a PENDING continuation and its first history row;
	// with consume, the result consumption is recorded in the same
	// transaction (a duplicate consumption is ErrAlreadyConsumed and
	// nothing is written).
	Create(ctx context.Context, c *continuation.Continuation, consume *ResultConsumption) error
	Get(ctx context.Context, projectID, id uuid.UUID) (*continuation.Continuation, error)
	GetCurrentForWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) (*continuation.Continuation, error)
	CurrentForProject(ctx context.Context, projectID uuid.UUID) (*continuation.Continuation, error)
	List(ctx context.Context, f ContinuationFilter) ([]continuation.Continuation, int, error)
	ListByWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) ([]continuation.Continuation, error)
	ListNonterminal(ctx context.Context, projectID uuid.UUID) ([]continuation.Continuation, error)

	// Advance applies one validated lifecycle transition with a version
	// check, the mutation and a history row atomically (WBS 14.53-14.55).
	Advance(ctx context.Context, id uuid.UUID, expectedVersion int, to continuation.Status, reason string, m ContinuationMutation, at time.Time) (*continuation.Continuation, error)
	// Update changes fields without a status change (version checked).
	Update(ctx context.Context, id uuid.UUID, expectedVersion int, m ContinuationMutation, at time.Time) (*continuation.Continuation, error)
	// ConsumeAndAdvance records the consumption, advances the
	// continuation and optionally creates the next one, atomically.
	ConsumeAndAdvance(ctx context.Context, consume ResultConsumption, id uuid.UUID, expectedVersion int, to continuation.Status, reason string,
		m ContinuationMutation, at time.Time, next *continuation.Continuation) (*continuation.Continuation, error)
	// Consume records only a consumption.
	Consume(ctx context.Context, consume ResultConsumption) error

	Claim(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (bool, error)
	Release(ctx context.Context, id uuid.UUID, owner string) error
	ListClaimable(ctx context.Context, now time.Time, limit int) ([]continuation.Continuation, error)

	Transitions(ctx context.Context, projectID, id uuid.UUID) ([]ContinuationTransition, error)
	CountsForWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) (WorkUnitCounts, error)
	IsConsumed(ctx context.Context, resultID uuid.UUID, consumer string) (bool, error)
	ListUnconsumed(ctx context.Context, limit int) ([]UnconsumedResult, error)
	ListAll(ctx context.Context, projectID uuid.UUID) ([]continuation.Continuation, error)
}

// ContinuationSettingsRepository persists project continuation settings.
type ContinuationSettingsRepository interface {
	Get(ctx context.Context, projectID uuid.UUID) (continuation.Settings, bool, error)
	Upsert(ctx context.Context, projectID uuid.UUID, s continuation.Settings) error
}
