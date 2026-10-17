package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TransitionHistoryRecord is the domain-facing view of one recorded
// workflow transition (WBS 6.28). The history is append-only; seq is the
// per-project monotonic state-entry sequence number (WBS 6.29).
type TransitionHistoryRecord struct {
	ID                  uuid.UUID
	ProjectID           uuid.UUID
	StateMachineID      uuid.UUID
	StateMachineVersion int

	// FromStateID is uuid.Nil for the bootstrap entry (outcome BOOTSTRAP,
	// WBS 15.56).
	FromStateID uuid.UUID
	ToStateID   uuid.UUID

	// ExecutionID binds the entry to an execution (WBS 15.55); nil for
	// pre-Phase-15 history.
	ExecutionID *uuid.UUID

	Outcome string

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	// RuntimeVersion is the runtime state version AFTER the transition was
	// applied (the version this history entry corresponds to).
	RuntimeVersion int

	// Seq is the per-project monotonic entry sequence (T6.29).
	Seq int

	CreatedAt time.Time
}

// TransitionHistoryRepository is the domain-facing persistence contract for
// state-transition history (WBS 6.28/6.29).
type TransitionHistoryRepository interface {
	// Append records one transition. allocateSeq requests the next
	// per-project sequence number: it is computed from the project's
	// persisted counter under a project-scoped advisory lock (with a
	// bounded retry on the unique (project_id, seq) constraint), the
	// counter is advanced, and the row is inserted — the whole allocation
	// is one statement set, so concurrent callers never duplicate a number
	// and it survives restart (T6.29.1–T6.29.4). A caller-supplied Seq > 0
	// is stored as-is (allocateSeq ignored); Seq 0 with allocateSeq false
	// is rejected (no sequence to store).
	Append(ctx context.Context, rec *TransitionHistoryRecord, allocateSeq bool) error
	// ListByProject lists a project's history in seq order (deterministic
	// ordering, T6.28.6 reads).
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*TransitionHistoryRecord, error)
	// ListByStateID lists the history rows that pass through the given
	// state (from- or to-endpoint), across the machine versions of the
	// project (WBS 6.36, T6.36.5 protection check).
	ListByStateID(ctx context.Context, stateID uuid.UUID) ([]*TransitionHistoryRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) TransitionHistoryRepository
}
