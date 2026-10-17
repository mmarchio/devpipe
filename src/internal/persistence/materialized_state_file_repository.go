package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MaterializedStateFileRecord is the domain-facing view of a materialized
// state file's metadata (WBS 7.23). It records what was last materialized
// for a state file (the version written, the content hash of the written
// bytes, the path, and when) so drift detection (P7-16) can compare the
// persisted version against what is on disk. It is a PROJECTION record:
// it never overrides the persisted version — PostgreSQL stays the
// authoritative source.
type MaterializedStateFileRecord struct {
	StateFileID uuid.UUID

	Version int
	// ContentHash is the deterministic SHA-256 of the written bytes
	// (statefile.ContentHash).
	ContentHash string
	// Path is the absolute path the file was materialized to.
	Path string
	// MaterializedAt is the UTC time of the last materialization.
	MaterializedAt time.Time
}

// MaterializedStateFileRepository is the domain-facing persistence contract
// for materialized-state-file metadata (WBS 7.23). Uniqueness is per
// (state_file_id) — one current record per file, updated in place on
// re-materialization (T7.23.4: no duplicates).
type MaterializedStateFileRepository interface {
	// Upsert persists (or updates, in place) the metadata record for the
	// state file. Re-materialization updates the SAME row, never
	// appending a new one.
	Upsert(ctx context.Context, rec *MaterializedStateFileRecord) error
	// Get returns the current record for the state file; a file that was
	// never materialized yields ErrNotFound.
	Get(ctx context.Context, stateFileID uuid.UUID) (*MaterializedStateFileRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) MaterializedStateFileRepository
}
