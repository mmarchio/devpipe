package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateFileVersionRecord is the domain-facing view of a stored state-file
// content version (WBS 7.5/7.6). ContentHash is the deterministic SHA-256
// over the exact content bytes; TokenEstimate is approximate metadata
// (bytes/4 heuristic), not an authoritative tokenizer (WBS 7.28).
type StateFileVersionRecord struct {
	ID          uuid.UUID
	StateFileID uuid.UUID

	Version int
	Content string

	ContentHash   string
	TokenEstimate int

	CreatedAt time.Time
}

// StateFileVersionRepository is the domain-facing persistence contract for
// state-file content versions (WBS 7.5/7.6/7.7/7.8). UpdateContent is the
// optimistic-concurrency surface: it appends the next version only when the
// file's current version equals expectedVersion; a stale expected version is
// ErrConflict with no version written.
type StateFileVersionRepository interface {
	// UpdateContent appends the next version for the file, guarded by
	// expectedVersion (WBS 7.8). A stale expectedVersion yields
	// ErrConflict BEFORE any write; a missing file yields ErrNotFound.
	// The version row insert and the state_files.current_version
	// advance are one transaction.
	UpdateContent(ctx context.Context, stateFileID uuid.UUID, content string, expectedVersion int) (*StateFileVersionRecord, error)
	// GetCurrentVersion returns the file's current version, resolved
	// from state_files.current_version. An unknown file yields
	// ErrNotFound; a file without content (current_version 0) returns
	// (nil, nil).
	GetCurrentVersion(ctx context.Context, stateFileID uuid.UUID) (*StateFileVersionRecord, error)
	// GetVersion returns the stored version; a missing version yields
	// ErrNotFound.
	GetVersion(ctx context.Context, stateFileID uuid.UUID, version int) (*StateFileVersionRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, stateFileID uuid.UUID) ([]*StateFileVersionRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateFileVersionRepository
}
