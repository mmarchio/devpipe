package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RuntimeStateRecord is the domain-facing view of a project's current
// logical workflow position (WBS 6.23): the bound machine family +
// version, the current state, and optional phase / work-unit
// associations. Version protects the row under optimistic concurrency
// (WBS 6.24); it is bumped ONLY by the versioned update op.
type RuntimeStateRecord struct {
	ID                  uuid.UUID
	ProjectID           uuid.UUID
	StateMachineID      uuid.UUID
	StateMachineVersion int

	CurrentStateID uuid.UUID

	// ExecutionID binds the runtime to an execution (WBS 15.55); nil
	// before bootstrap.
	ExecutionID *uuid.UUID

	// PhaseID and WorkUnitID are nullable: a runtime state may exist
	// before a phase / work unit is bound (WBS 6.23).
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	EnteredAt time.Time
	Version   int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RuntimeStateRepository is the domain-facing persistence contract for
// runtime state (WBS 6.23/6.24). One row per project (unique project_id):
// writes are upserts. UpdateWithVersion applies the optimistic-concurrency
// guard (WBS 6.24): the row is only written when its stored version equals
// expectedVersion; a stale version is ErrConflict and a missing row is
// ErrNotFound.
type RuntimeStateRepository interface {
	// Create inserts the project's runtime state row; a second create for
	// the same project yields ErrConflict (the unique project constraint).
	Create(ctx context.Context, rs *RuntimeStateRecord) error
	// Get returns the project's runtime state; ErrNotFound when the project
	// has none yet.
	Get(ctx context.Context, projectID uuid.UUID) (*RuntimeStateRecord, error)
	// UpdateWithVersion persists a full runtime state guarded by an
	// optimistic-concurrency version (WBS 6.24): it writes only when the
	// stored version equals expectedVersion and increments the version
	// atomically. A missing row is ErrNotFound; a stale version is
	// ErrConflict (the row is probed to distinguish the two).
	UpdateWithVersion(ctx context.Context, rs *RuntimeStateRecord, expectedVersion int) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) RuntimeStateRepository
}
