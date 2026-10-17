package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ContextSnapshotFileRecord is the domain-facing view of one state-file
// pin inside an execution-context snapshot (WBS 7.30/7.31): the file at
// its exact immutable version, with the content hash and the exact content
// bytes duplicated (reconstruction is trivially guaranteed), and the scope
// retained.
type ContextSnapshotFileRecord struct {
	ID uuid.UUID

	SnapshotID  uuid.UUID
	StateFileID uuid.UUID

	TypeKey string
	Scope   string

	Version int
	Hash    string

	Content string

	// TokenEstimate is the pin's component of the snapshot's token budget
	// (WBS 7.35); Classification is the pin's WBS 7.36 source class,
	// persisted at snapshot time (read back from the snapshot, never
	// re-derived from live state).
	TokenEstimate  int
	Classification string

	CreatedAt time.Time
}

// ContextSnapshotRecord is the domain-facing view of a persisted
// execution-context snapshot (WBS 7.31): the exact context a later prompt
// will use, captured against one consistent runtime position.
// RoleSnapshotJSON is the serialized role.RoleSnapshot (the service
// boundary keeps the domain role type out of this contract).
type ContextSnapshotRecord struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	RuntimeStateID      uuid.UUID
	RuntimeStateVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int

	StateKey string

	RoleSnapshotJSON []byte

	// The WBS 7.35 aggregate token budget, stored with the snapshot.
	StateFileTokens int
	RoleTokens      int
	EstimatedTotal  int

	CreatedAt time.Time

	Files []*ContextSnapshotFileRecord
}

// ContextSnapshotRepository is the domain-facing persistence contract for
// execution-context snapshots (WBS 7.31). Create persists the snapshot and
// its file pins atomically (the parent row plus all file rows in one write
// — a partial persist is a persistence failure, WBS 7.32.4).
type ContextSnapshotRepository interface {
	// Create inserts the snapshot and its state-file pins; a failure of
	// any row is an infrastructure error (nothing is partially kept).
	Create(ctx context.Context, s *ContextSnapshotRecord) error
	// ListByProject lists the project's snapshots, oldest first.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*ContextSnapshotRecord, error)
	// Get returns the snapshot with the given id and its file pins;
	// ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*ContextSnapshotRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) ContextSnapshotRepository
}
