package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RoleInstructionAssignmentRecord is the domain-facing view of a stored
// role → instruction assignment (WBS 5.9).
type RoleInstructionAssignmentRecord struct {
	ID            uuid.UUID
	RoleID        uuid.UUID
	InstructionID uuid.UUID
	Position      int
	CreatedAt     time.Time
}

// RoleInstructionAssignmentRepository is the domain-facing persistence
// contract for ordered role → instruction assignments (WBS 5.9).
type RoleInstructionAssignmentRepository interface {
	// Assign links instruction to role at the next deterministic position
	// (MAX(position)+1, 1 when none). Duplicate (role, instruction) is
	// ErrConflict (T5.9.4).
	Assign(ctx context.Context, roleID, instructionID uuid.UUID) (*RoleInstructionAssignmentRecord, error)
	// Unassign removes the assignment row (WBS 5.9 T5.9.5 — the instruction
	// row is retained by ON DELETE CASCADE on the assignment, not deleted).
	Unassign(ctx context.Context, roleID, instructionID uuid.UUID) error
	// ListByRole returns the role's assignments in position order
	// (T5.9.3).
	ListByRole(ctx context.Context, roleID uuid.UUID) ([]*RoleInstructionAssignmentRecord, error)
	// ListByInstruction returns the assignments referencing an instruction
	// (WBS 5.23 T5.23.5 — a referenced instruction is protected from
	// destructive deletion).
	ListByInstruction(ctx context.Context, instructionID uuid.UUID) ([]*RoleInstructionAssignmentRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) RoleInstructionAssignmentRepository
}
