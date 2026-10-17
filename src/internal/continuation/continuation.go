package continuation

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Continuation is one persistent continuation operation (WBS 14.1): the
// bound project/phase/work unit and runtime state, its source, lifecycle
// status, decision and the exact captured status it continues from.
type Continuation struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    uuid.UUID
	WorkUnitID uuid.UUID

	RuntimeStateID      uuid.UUID
	RuntimeStateVersion int

	SourceDispatchID uuid.UUID
	SourceResultID   *uuid.UUID
	Trigger          Trigger

	Status   Status
	Strategy Strategy
	Decision *Decision

	Attempt int

	Captured         CapturedStatus
	StatusHash       string
	ProgressKey      string
	StatusUnknown    bool
	NoProgressStreak int
	ScopeExpanded    bool

	SourceSnapshotID *uuid.UUID
	SnapshotID       *uuid.UUID

	OldSessionID          *uuid.UUID
	NewSessionID          *uuid.UUID
	SessionID             *uuid.UUID
	SessionConfirmedAt    *time.Time
	CWDConfirmedAt        *time.Time
	SessionCreateFailures int

	WorkerPromptID *uuid.UUID
	DispatchID     *uuid.UUID

	RoleKey  string
	StateKey string

	RecoveryReason   RecoveryReason
	OverrideStrategy Strategy
	RetryOfID        *uuid.UUID
	FailureMessage   string

	Version        int
	LeaseOwner     string
	LeaseExpiresAt *time.Time

	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// FirstAttempt is the attempt number of a work unit's first continuation
// (T14.1.4); later continuations use NextAttempt.
const FirstAttempt = 1

// NextAttempt returns the attempt after prior (0 = none yet). Transport
// recovery inside one continuation never calls it (WBS 14.36).
func NextAttempt(prior int) int {
	if prior < 0 {
		prior = 0
	}
	return prior + 1
}

// Source is the authoritative binding of a source dispatch or result.
type Source struct {
	ID             uuid.UUID
	ProjectID      uuid.UUID
	PhaseID        *uuid.UUID
	WorkUnitID     *uuid.UUID
	RuntimeStateID uuid.UUID // uuid.Nil = not recorded on the source
}

// NewInput are the fields of a new continuation.
type NewInput struct {
	ProjectID           uuid.UUID
	PhaseID             uuid.UUID
	WorkUnitID          uuid.UUID
	RuntimeStateID      uuid.UUID
	RuntimeStateVersion int

	Trigger Trigger
	Attempt int

	SourceDispatch Source
	SourceResult   *Source

	Captured         CapturedStatus
	StatusUnknown    bool
	NoProgressStreak int
	ScopeExpanded    bool

	SourceSnapshotID *uuid.UUID
	RoleKey          string
	StateKey         string
	RetryOfID        *uuid.UUID

	Now time.Time
}

// Construction errors.
var (
	ErrMissingBinding = errors.New("continuation: project, phase, work unit and runtime state are required")
	ErrMissingSource  = errors.New("continuation: source dispatch (and source result for an INCOMPLETE trigger) required")
	ErrCrossBinding   = errors.New("continuation: source is bound to another project, phase, work unit or runtime state")
	ErrInvalidAttempt = errors.New("continuation: attempt must be >= 1")
)

func sameOpt(a *uuid.UUID, b uuid.UUID) bool { return a != nil && *a == b }

func (in NewInput) checkSource(s Source) error {
	if s.ID == uuid.Nil {
		return ErrMissingSource
	}
	if s.ProjectID != in.ProjectID || !sameOpt(s.PhaseID, in.PhaseID) || !sameOpt(s.WorkUnitID, in.WorkUnitID) {
		return ErrCrossBinding
	}
	if s.RuntimeStateID != uuid.Nil && s.RuntimeStateID != in.RuntimeStateID {
		return ErrCrossBinding
	}
	return nil
}

// NewContinuation validates the bindings (WBS 14.1, 14.66-14.68) and
// returns a PENDING continuation at version 0.
func NewContinuation(in NewInput) (*Continuation, error) {
	if in.ProjectID == uuid.Nil || in.PhaseID == uuid.Nil || in.WorkUnitID == uuid.Nil || in.RuntimeStateID == uuid.Nil {
		return nil, ErrMissingBinding
	}
	if _, err := ParseTrigger(string(in.Trigger)); err != nil {
		return nil, err
	}
	if in.Attempt < FirstAttempt {
		return nil, ErrInvalidAttempt
	}
	if err := in.checkSource(in.SourceDispatch); err != nil {
		return nil, err
	}
	var resultID *uuid.UUID
	if in.SourceResult != nil {
		if err := in.checkSource(*in.SourceResult); err != nil {
			return nil, err
		}
		id := in.SourceResult.ID
		resultID = &id
	} else if in.Trigger.RequiresResult() {
		return nil, ErrMissingSource
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cap := in.Captured.Clone()
	return &Continuation{ID: uuid.New(), ProjectID: in.ProjectID, PhaseID: in.PhaseID, WorkUnitID: in.WorkUnitID,
		RuntimeStateID: in.RuntimeStateID, RuntimeStateVersion: in.RuntimeStateVersion,
		SourceDispatchID: in.SourceDispatch.ID, SourceResultID: resultID, Trigger: in.Trigger, Status: StatusPending,
		Attempt: in.Attempt, Captured: cap, StatusHash: cap.Hash(), ProgressKey: cap.ProgressKey(),
		StatusUnknown: in.StatusUnknown, NoProgressStreak: in.NoProgressStreak, ScopeExpanded: in.ScopeExpanded,
		SourceSnapshotID: in.SourceSnapshotID, RoleKey: in.RoleKey, StateKey: in.StateKey, RetryOfID: in.RetryOfID,
		CreatedAt: now, UpdatedAt: now}, nil
}

// ConfirmedSession returns the session the continuation dispatches
// through: the confirmed new session for a fresh strategy, else the
// validated existing session.
func (c *Continuation) ConfirmedSession() *uuid.UUID {
	if c.NewSessionID != nil && c.SessionConfirmedAt != nil {
		return c.NewSessionID
	}
	return c.SessionID
}
