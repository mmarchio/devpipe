package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// MaterializedStateFileRepository persists materialized-state-file
// metadata on PostgreSQL (WBS 7.23, P7-15): one current record per state
// file (state_file_id primary key), updated in place on
// re-materialization so the row count stays 1 (T7.23.4). The record is a
// projection for drift detection — it never overrides the persisted
// version.
type MaterializedStateFileRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.MaterializedStateFileRepository = (*MaterializedStateFileRepository)(nil)

// NewMaterializedStateFileRepository binds the repository to an existing
// pool.
func NewMaterializedStateFileRepository(pool *pgxpool.Pool) *MaterializedStateFileRepository {
	return &MaterializedStateFileRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *MaterializedStateFileRepository) WithTx(tx persistence.Transaction) persistence.MaterializedStateFileRepository {
	return &MaterializedStateFileRepository{exec: txExecutor{tx}}
}

// Upsert persists (or updates in place) the metadata record for the state
// file. The ON CONFLICT (state_file_id) clause makes re-materialization
// an in-place update — the row count per file stays 1.
func (r *MaterializedStateFileRepository) Upsert(ctx context.Context, rec *persistence.MaterializedStateFileRecord) error {
	if rec == nil {
		return &persistence.InfrastructureError{Op: "upsert materialized state file", Cause: errors.New("record required")}
	}
	if _, err := r.exec.Exec(ctx, `
		INSERT INTO materialized_state_files
			(state_file_id, version, content_hash, path, materialized_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (state_file_id) DO UPDATE SET
			version = EXCLUDED.version,
			content_hash = EXCLUDED.content_hash,
			path = EXCLUDED.path,
			materialized_at = EXCLUDED.materialized_at`,
		rec.StateFileID, rec.Version, rec.ContentHash, rec.Path, rec.MaterializedAt,
	); err != nil {
		return TranslateDBError("upsert materialized state file", err)
	}
	return nil
}

// Get returns the current record for the state file; a file that was
// never materialized yields persistence.ErrNotFound.
func (r *MaterializedStateFileRepository) Get(ctx context.Context, stateFileID uuid.UUID) (*persistence.MaterializedStateFileRecord, error) {
	var rec persistence.MaterializedStateFileRecord
	err := r.exec.QueryRow(ctx, `
		SELECT state_file_id, version, content_hash, path, materialized_at
		FROM materialized_state_files
		WHERE state_file_id = $1`, stateFileID).Scan(
		&rec.StateFileID, &rec.Version, &rec.ContentHash, &rec.Path, &rec.MaterializedAt,
	)
	if err != nil {
		return nil, TranslateDBError("get materialized state file", err)
	}
	return &rec, nil
}
