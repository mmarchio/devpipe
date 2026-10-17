package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateInstructionRecord is the domain-facing view of a stored state
// instruction version (WBS 6.5): immutable historical content plus its
// deterministic content hash. A state instruction is a distinct entity from
// the role system prompt (T6.5.5).
type StateInstructionRecord struct {
	ID          string
	StateID     uuid.UUID
	Version     int
	Content     string
	ContentHash string
	CreatedAt   time.Time
}

// StateInstructionRepository is the domain-facing persistence contract for
// state-instruction version history (WBS 6.5). Appends are append-only:
// historical versions are never overwritten (T6.5.3), the state's
// current-version pointer always tracks the latest, and a hash is generated
// per version (T6.5.4).
type StateInstructionRepository interface {
	// AppendVersion stores content as the next version (MAX(version)+1) with
	// its content hash and advances the state's current-version pointer.
	// A missing state row yields ErrInvalidReference (the version's FK).
	AppendVersion(ctx context.Context, stateID uuid.UUID, content string) (*StateInstructionRecord, error)
	// GetVersion returns the stored version; ErrNotFound when the state has
	// no such version.
	GetVersion(ctx context.Context, stateID uuid.UUID, version int) (*StateInstructionRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, stateID uuid.UUID) ([]*StateInstructionRecord, error)
	// GetCurrentVersion returns the state's current version (0 when the
	// state has no instruction).
	GetCurrentVersion(ctx context.Context, stateID uuid.UUID) (int, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateInstructionRepository
}
