package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateFileRequirementRecord is the domain-facing view of a stored
// state -> required state-file category (WBS 6.6).
type StateFileRequirementRecord struct {
	ID        uuid.UUID
	StateID   uuid.UUID
	Category  string
	Ordinal   int
	CreatedAt time.Time
}

// StateFileRequirementRepository is the domain-facing persistence contract
// for state state-file requirements (WBS 6.6).
type StateFileRequirementRepository interface {
	// Require records that the state requires the given state-file category
	// at the next deterministic ordinal. A duplicate (state, category) is
	// ErrConflict (T6.6.3); a missing state is ErrInvalidReference.
	Require(ctx context.Context, stateID uuid.UUID, category string) (*StateFileRequirementRecord, error)
	// ListByState returns the state's required state-file categories in
	// ordinal order (deterministic, T6.6.4).
	ListByState(ctx context.Context, stateID uuid.UUID) ([]*StateFileRequirementRecord, error)
	// Remove deletes the state's requirement for the given category
	// (WBS 6.36 state-file replacement); a missing row is a no-op.
	Remove(ctx context.Context, stateID uuid.UUID, category string) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateFileRequirementRepository
}
