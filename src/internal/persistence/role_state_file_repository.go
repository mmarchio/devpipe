package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RoleStateFileRecord is the domain-facing view of a stored role → state-file
// requirement (WBS 5.12).
type RoleStateFileRecord struct {
	ID           uuid.UUID
	RoleID       uuid.UUID
	StateFileKey string
	Position     int
	CreatedAt    time.Time
}

// RoleStateFileRepository is the domain-facing persistence contract for role
// state-file requirements (WBS 5.12).
type RoleStateFileRepository interface {
	// Require records that the role requires the given state-file type at the
	// next deterministic position. A duplicate (role, state_file_key) is
	// ErrConflict (T5.12.3).
	Require(ctx context.Context, roleID uuid.UUID, stateFileKey string) (*RoleStateFileRecord, error)
	// Remove isolates the requirement (deletes only this role's row);
	// ErrNotFound when absent (T5.12.5).
	Remove(ctx context.Context, roleID uuid.UUID, stateFileKey string) error
	// ListByRole returns the role's required state-file keys in position
	// order (deterministic, T5.12.4).
	ListByRole(ctx context.Context, roleID uuid.UUID) ([]*RoleStateFileRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) RoleStateFileRepository
}
