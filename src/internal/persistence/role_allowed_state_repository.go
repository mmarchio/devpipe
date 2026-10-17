package persistence

import (
	"context"

	"github.com/google/uuid"
)

// RoleAllowedStateRecord is the domain-facing view of a stored role →
// allowed-state association (WBS 5.11).
type RoleAllowedStateRecord struct {
	ID       uuid.UUID
	RoleID   uuid.UUID
	StateKey string
}

// RoleAllowedStateRepository is the domain-facing persistence contract for
// role allowed-state definitions (WBS 5.11).
type RoleAllowedStateRepository interface {
	// AssignState records that the role may be assigned to the given state
	// key. A duplicate (role, state_key) is ErrConflict (T5.11.3).
	AssignState(ctx context.Context, roleID uuid.UUID, stateKey string) (*RoleAllowedStateRecord, error)
	// UnassignState removes the association; ErrNotFound when absent.
	UnassignState(ctx context.Context, roleID uuid.UUID, stateKey string) error
	// ListByRole returns the role's allowed state keys (stable order).
	ListByRole(ctx context.Context, roleID uuid.UUID) ([]*RoleAllowedStateRecord, error)
	// IsEligible reports whether the role is allowed to be assigned to the
	// given state key (T5.11.5).
	IsEligible(ctx context.Context, roleID uuid.UUID, stateKey string) (bool, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) RoleAllowedStateRepository
}
