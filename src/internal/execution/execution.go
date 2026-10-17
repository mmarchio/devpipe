package execution

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Execution is one autonomous run of a project's configuration (WBS 15.1).
// It references the project by id only (WBS 15.3: no project identity is
// duplicated) and binds permanently to one state-machine version.
type Execution struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	Number int
	Status ExecutionStatus

	// Generation is the prompt/session lineage generation (WBS 15.145).
	Generation int
	// CurrentSessionID is nil until a later phase creates a worker
	// session (WBS 15.146: no fake session id).
	CurrentSessionID *uuid.UUID

	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateMachineHash    string

	InitialPhaseID uuid.UUID

	ConfigurationSnapshotID uuid.UUID

	StartedAt   time.Time
	CompletedAt *time.Time
	FailedAt    *time.Time
	CancelledAt *time.Time

	Version int
}

// NewExecutionInput carries the caller-supplied fields.
type NewExecutionInput struct {
	ProjectID           uuid.UUID
	Number              int
	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateMachineHash    string
	InitialPhaseID      uuid.UUID
	Now                 time.Time
}

// Construction errors (T15.1.2–T15.1.5).
var (
	ErrProjectRequired      = errors.New("execution: project is required")
	ErrStateMachineRequired = errors.New("execution: state-machine binding is required")
	ErrInitialPhaseRequired = errors.New("execution: initial phase is required")
	ErrInvalidNumber        = errors.New("execution: number must be >= 1")
)

// NewExecution constructs an INITIALIZING execution (WBS 15.1): Version 1,
// Generation 1, no session (WBS 15.145/15.146/15.148).
func NewExecution(in NewExecutionInput) (*Execution, error) {
	switch {
	case in.ProjectID == uuid.Nil:
		return nil, ErrProjectRequired
	case in.StateMachineID == uuid.Nil || in.StateMachineVersion < 1 || strings.TrimSpace(in.StateMachineHash) == "":
		return nil, ErrStateMachineRequired
	case in.InitialPhaseID == uuid.Nil:
		return nil, ErrInitialPhaseRequired
	case in.Number < 1:
		return nil, ErrInvalidNumber
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	return &Execution{
		ID:                  uuid.New(),
		ProjectID:           in.ProjectID,
		Number:              in.Number,
		Status:              ExecutionInitializing,
		Generation:          1,
		StateMachineID:      in.StateMachineID,
		StateMachineVersion: in.StateMachineVersion,
		StateMachineHash:    in.StateMachineHash,
		InitialPhaseID:      in.InitialPhaseID,
		StartedAt:           now.UTC(),
		Version:             1,
	}, nil
}

// RecoveryRequired is the recovery marker (WBS 15.148): derived from the
// status, false for every normal bootstrap.
func (e *Execution) RecoveryRequired() bool { return e.Status == ExecutionRecovery }
