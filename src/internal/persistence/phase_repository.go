package persistence

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/phase"
)

// PhaseRepository is the domain-facing persistence contract for phases
// (WBS 3.5, extended for WBS 3.16). It references only domain types.
type PhaseRepository interface {
	// Append creates a phase with a server-assigned order: the value after
	// the project's current maximum (1 when the project has no phases).
	// Order is never client-controlled (WBS 3.16).
	Append(ctx context.Context, projectID uuid.UUID, name, description string) (*phase.Phase, error)
	// Get returns the phase with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*phase.Phase, error)
	// Update persists name/description/status changes; ErrNotFound when
	// the phase does not exist.
	Update(ctx context.Context, p *phase.Phase) error
	// Delete removes the phase; ErrNotFound when it does not exist.
	Delete(ctx context.Context, id uuid.UUID) error
	// ListByProject returns all phases of the project in ascending order.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*phase.Phase, error)
	// GetNextIncomplete returns the lowest-order phase that has not reached
	// DONE, or nil when every phase is complete.
	GetNextIncomplete(ctx context.Context, projectID uuid.UUID) (*phase.Phase, error)
	// MarkCompleted records the completion transition (WBS 4.22): status
	// DONE and the completion timestamp, which is recorded once and
	// preserved on later calls.
	MarkCompleted(ctx context.Context, id uuid.UUID) error
	// MarkFailed records the failure transition (WBS 4.23): status FAILED,
	// the failure reason, and the failure timestamp, which is recorded
	// once and preserved on later calls.
	MarkFailed(ctx context.Context, id uuid.UUID, reason string) error
	// WithTx returns a copy of the repository bound to the given transaction.
	WithTx(tx Transaction) PhaseRepository
}
