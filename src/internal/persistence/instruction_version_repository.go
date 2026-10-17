package persistence

import (
	"context"
	"time"
)

// InstructionVersionRecord is the domain-facing view of a stored instruction
// version (WBS 5.8): immutable historical content plus its hash.
type InstructionVersionRecord struct {
	ID            string
	InstructionID string
	Version       int
	Content       string
	ContentHash   string
	CreatedAt     time.Time
}

// InstructionVersionRepository is the domain-facing persistence contract for
// instruction version history (WBS 5.8). Append-only, like WBS/prompts.
type InstructionVersionRepository interface {
	// AppendVersion stores content as the next version with its hash and
	// advances the instruction's current-version pointer.
	AppendVersion(ctx context.Context, instructionID, content string) (*InstructionVersionRecord, error)
	// UpdateWithExpected stores a new version only when the instruction's
	// current version equals expectedVersion (WBS 5.8 T5.8.5): a stale
	// expected version is ErrConflict.
	UpdateWithExpected(ctx context.Context, instructionID, content string, expectedVersion int) (*InstructionVersionRecord, error)
	// GetVersion returns the stored version; ErrNotFound when missing.
	GetVersion(ctx context.Context, instructionID string, version int) (*InstructionVersionRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, instructionID string) ([]*InstructionVersionRecord, error)
	// GetCurrentVersion returns the current version (0 when none).
	GetCurrentVersion(ctx context.Context, instructionID string) (int, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) InstructionVersionRepository
}
