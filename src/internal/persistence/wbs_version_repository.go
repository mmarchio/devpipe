package persistence

import (
	"context"
	"time"
)

// WBSVersionRecord is the domain-facing view of a stored WBS version
// (WBS 4.10/4.11): immutable historical content plus its deterministic
// content hash.
type WBSVersionRecord struct {
	ID          string
	PhaseID     string
	Version     int
	Content     string
	ContentHash string
	CreatedAt   time.Time
}

// WBSVersionRepository is the domain-facing persistence contract for WBS
// version history (WBS 4.10). Appends are append-only: historical versions
// are never overwritten, and the phase's current-version pointer always
// tracks the latest version.
type WBSVersionRepository interface {
	// AppendVersion stores content as the next version (MAX(version)+1) with
	// its content hash and advances the phase's current-version pointer.
	AppendVersion(ctx context.Context, phaseID, content string) (*WBSVersionRecord, error)
	// GetVersion returns the stored version; ErrNotFound when the phase has
	// no such version.
	GetVersion(ctx context.Context, phaseID string, version int) (*WBSVersionRecord, error)
	// ListVersions returns all stored versions in ascending version order.
	ListVersions(ctx context.Context, phaseID string) ([]*WBSVersionRecord, error)
	// GetCurrentVersion returns the phase's current version (0 when the
	// phase has no WBS document).
	GetCurrentVersion(ctx context.Context, phaseID string) (int, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) WBSVersionRepository
}
