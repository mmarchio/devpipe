package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CanonicalPromptRecord is the domain-facing view of a stored canonical
// prompt (WBS 8.26): the project/phase/work-unit associations, the
// context-snapshot + template (id+version) + role (id+version) +
// state-machine (id+version+state key) provenance, and the exact prompt
// content + hash + estimated-token count. A nil PhaseID/WorkUnitID means the
// prompt was not bound to a phase/work unit. Content is stored exactly.
type CanonicalPromptRecord struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	ContextSnapshotID uuid.UUID

	TemplateID      uuid.UUID
	TemplateVersion int

	RoleID      uuid.UUID
	RoleVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateKey            string

	Content         string
	Hash            string
	EstimatedTokens int

	// CacheKey is the Phase 10 Layer 1 (canonical) cache key — the semantic
	// identity of the composition inputs ("" = composed without one).
	CacheKey string

	// ProjectionID is set when the prompt was composed from a Phase 11
	// projected context (ContextSnapshotID is then the real base snapshot).
	ProjectionID *uuid.UUID

	CreatedAt time.Time
}

// CanonicalPromptRepository is the domain-facing persistence contract for
// canonical prompts (WBS 8.26 persistence half, P8-30). Create is the sole
// writer (append-only; a canonical prompt is never rewritten after creation).
// Get/ListByProject read the stored prompts.
type CanonicalPromptRepository interface {
	// Create inserts a canonical prompt; a missing project reference yields
	// ErrInvalidReference.
	Create(ctx context.Context, rec *CanonicalPromptRecord) error
	// Get returns the canonical prompt by id; an unknown id yields
	// ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (*CanonicalPromptRecord, error)
	// ListByProject returns the project's canonical prompts in creation
	// order; a project with none returns an empty (non-nil) slice.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*CanonicalPromptRecord, error)
	// FindByCacheKey returns the project's newest canonical prompt with the
	// Layer 1 cache key (Phase 10, WBS 10.54); ErrNotFound when none.
	FindByCacheKey(ctx context.Context, projectID uuid.UUID, cacheKey string) (*CanonicalPromptRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) CanonicalPromptRepository
}
