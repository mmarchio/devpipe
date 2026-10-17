package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateRoleAssignmentRecord is the domain-facing view of a stored
// state-to-role assignment (WBS 6.4). A state may carry an ordered list of
// participating roles; Ordinal is the position in that list.
type StateRoleAssignmentRecord struct {
	ID      uuid.UUID
	StateID uuid.UUID
	RoleID  uuid.UUID

	Ordinal int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateRoleAssignmentRepository is the domain-facing persistence contract
// for state-to-role assignments (WBS 6.4). Role existence is DB-level
// (T6.4.2); a cross-project role is rejected at the DB level (T6.4.6); a
// duplicate (state, role) yields ErrConflict. The disabled-role (T6.4.3)
// and allowed-states (T6.4.4) checks are service-level configuration
// concerns — the repository persists the assignment either way.
type StateRoleAssignmentRepository interface {
	// Create inserts a new assignment; a duplicate (state_id, role_id)
	// yields ErrConflict, a missing role yields ErrInvalidReference, and
	// a cross-project role yields a translated infrastructure error.
	Create(ctx context.Context, a *StateRoleAssignmentRecord) error
	// Get returns the assignment with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*StateRoleAssignmentRecord, error)
	// ListByState returns the state's assigned roles in ordinal order
	// (the ordered participating-role list, T6.4.5).
	ListByState(ctx context.Context, stateID uuid.UUID) ([]*StateRoleAssignmentRecord, error)
	// Remove deletes the state-role assignment for the given role
	// (WBS 6.36); a missing row is a no-op.
	Remove(ctx context.Context, stateID, roleID uuid.UUID) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateRoleAssignmentRepository
}
