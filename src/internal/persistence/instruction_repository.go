package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// InstructionRecord is the domain-facing view of a stored instruction
// document (WBS 5.7). A nil ProjectID is the GLOBAL scope.
type InstructionRecord struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string
	Content     string

	Version int
	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// InstructionRepository is the domain-facing persistence contract for
// instruction documents (WBS 5.7). Key uniqueness is scoped like roles.
type InstructionRepository interface {
	// Create inserts a new instruction; a scoped key duplicate yields
	// ErrConflict.
	Create(ctx context.Context, i *InstructionRecord) error
	// Get returns the instruction with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*InstructionRecord, error)
	// ListByScope lists all instructions in one scope (nil = global),
	// ordered by key.
	ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*InstructionRecord, error)
	// Delete removes the instruction (WBS 5.23); the application constrains
	// destructive deletion of referenced instructions before calling.
	Delete(ctx context.Context, id uuid.UUID) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) InstructionRepository
}
