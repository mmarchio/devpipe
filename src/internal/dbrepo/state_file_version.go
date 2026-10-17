package dbrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

const stateFileVersionColumns = `id, state_file_id, version, content, content_hash, token_estimate, created_at`

// StateFileVersionRepository implements append-only state-file content
// versioning on PostgreSQL (WBS 7.5/7.6 persistence half, P7-06), mirroring
// the instruction-version recipe: the next version is MAX(version)+1
// computed inside a transaction serialized by a file-scoped advisory lock,
// wrapped in a bounded retry on the (state_file_id, version) unique
// constraint (T7.5.5), and the current_version pointer advances in the SAME
// transaction.
type StateFileVersionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateFileVersionRepository = (*StateFileVersionRepository)(nil)

// NewStateFileVersionRepository binds the repository to an existing pool.
func NewStateFileVersionRepository(pool *pgxpool.Pool) *StateFileVersionRepository {
	return &StateFileVersionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateFileVersionRepository) WithTx(tx persistence.Transaction) persistence.StateFileVersionRepository {
	return &StateFileVersionRepository{exec: txExecutor{tx}}
}

func scanStateFileVersionRow(row interface{ Scan(dest ...any) error }) (*persistence.StateFileVersionRecord, error) {
	var rec persistence.StateFileVersionRecord
	if err := row.Scan(&rec.ID, &rec.StateFileID, &rec.Version, &rec.Content, &rec.ContentHash, &rec.TokenEstimate, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// UpdateContent appends the next content version guarded by an expected
// current version (WBS 7.8). A stale expectedVersion is rejected with
// persistence.ErrConflict BEFORE any write — no version row, no pointer
// change (T7.8.3/T7.8.4) — and the error is distinguishable from an
// infrastructure failure (T7.8.5). The insert and the
// state_files.current_version advance commit atomically.
func (r *StateFileVersionRepository) UpdateContent(ctx context.Context, stateFileID uuid.UUID, content string, expectedVersion int) (*persistence.StateFileVersionRecord, error) {
	// Fast-path file existence check: a missing file is a plain
	// ErrNotFound, not a conflict.
	if err := r.fileExists(ctx, stateFileID); err != nil {
		return nil, err
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.updateOnce(ctx, stateFileID, content, expectedVersion)
		if err == nil || !errors.Is(err, persistence.ErrConflict) {
			return v, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *StateFileVersionRepository) fileExists(ctx context.Context, stateFileID uuid.UUID) error {
	var exists bool
	if err := r.exec.QueryRow(ctx, `SELECT true FROM state_files WHERE id = $1`, stateFileID).Scan(&exists); err != nil {
		return TranslateDBError("get state-file", err)
	}
	if !exists {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *StateFileVersionRepository) updateOnce(ctx context.Context, stateFileID uuid.UUID, content string, expectedVersion int) (*persistence.StateFileVersionRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin state-file version update", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "state-file-versions:"+stateFileID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var fileExists bool
	var cur int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) > 0, coalesce(max(current_version), 0) FROM state_files WHERE id = $1`, stateFileID).
		Scan(&fileExists, &cur); err != nil {
		return nil, TranslateDBError("current state-file version", err)
	}
	if !fileExists {
		if !joined {
			_ = tx.Rollback(ctx)
		}
		return nil, persistence.ErrNotFound
	}
	// Optimistic guard (WBS 7.8): a stale expected version is rejected
	// BEFORE any write.
	if cur != expectedVersion {
		if !joined {
			_ = tx.Rollback(ctx)
		}
		return nil, persistence.ErrConflict
	}
	next := statefile.NextVersion(existingVersionNumbers(cur))
	if err := statefile.ValidateVersionCandidate(existingVersionNumbers(cur), next); err != nil {
		return nil, TranslateDBError("validate version candidate", err)
	}
	v, err := r.insertVersion(ctx, tx, joined, stateFileID, content, next)
	if err != nil {
		return nil, err
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit state-file version update", err)
		}
	}
	return v, nil
}

// existingVersionNumbers returns 1..n, the version set a file at current
// version n holds (versions are contiguous and append-only).
func existingVersionNumbers(cur int) []int {
	out := make([]int, 0, cur)
	for v := 1; v <= cur; v++ {
		out = append(out, v)
	}
	return out
}

// insertVersion inserts the version row and advances the current pointer on
// the state_files row — one transaction, so the pointer never lags the
// version table.
func (r *StateFileVersionRepository) insertVersion(ctx context.Context, tx persistence.Transaction, joined bool, stateFileID uuid.UUID, content string, version int) (*persistence.StateFileVersionRecord, error) {
	v := &persistence.StateFileVersionRecord{
		ID: uuid.New(), StateFileID: stateFileID, Version: version,
		Content: content, ContentHash: statefile.ContentHash(content),
		TokenEstimate: statefile.TokenEstimate(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO state_file_versions (id, state_file_id, version, content, content_hash, token_estimate)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+stateFileVersionColumns,
		v.ID, stateFileID, version, content, v.ContentHash, v.TokenEstimate)
	if err := row.Scan(&v.ID, &v.StateFileID, &v.Version, &v.Content, &v.ContentHash, &v.TokenEstimate, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert state-file version", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE state_files SET current_version = $2, updated_at = now() WHERE id = $1`,
		stateFileID, version); err != nil {
		return nil, TranslateDBError("update current state-file version", err)
	}
	_ = joined
	return v, nil
}

func (r *StateFileVersionRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
	if te, ok := r.exec.(txExecutor); ok {
		return te.tx, true, nil
	}
	if b, ok := r.exec.(persistence.TxBeginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return nil, false, err
		}
		return tx, false, nil
	}
	return nil, false, &persistence.InfrastructureError{Op: "state-file version append", Cause: errors.New("executor cannot begin transactions")}
}

// GetCurrentVersion returns the file's current version, resolved from
// state_files.current_version; an unknown file yields ErrNotFound and a
// file without content returns (nil, nil).
func (r *StateFileVersionRepository) GetCurrentVersion(ctx context.Context, stateFileID uuid.UUID) (*persistence.StateFileVersionRecord, error) {
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(current_version, 0) FROM state_files WHERE id = $1`, stateFileID).Scan(&cur); err != nil {
		return nil, TranslateDBError("get current state-file version", err)
	}
	if cur == 0 {
		return nil, nil
	}
	return r.GetVersion(ctx, stateFileID, cur)
}

// GetVersion returns the stored version; a missing version yields
// persistence.ErrNotFound.
func (r *StateFileVersionRepository) GetVersion(ctx context.Context, stateFileID uuid.UUID, version int) (*persistence.StateFileVersionRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+stateFileVersionColumns+` FROM state_file_versions WHERE state_file_id = $1 AND version = $2`,
		stateFileID, version)
	v, err := scanStateFileVersionRow(row)
	if err != nil {
		return nil, TranslateDBError("get state-file version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *StateFileVersionRepository) ListVersions(ctx context.Context, stateFileID uuid.UUID) ([]*persistence.StateFileVersionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+stateFileVersionColumns+` FROM state_file_versions WHERE state_file_id = $1 ORDER BY version ASC`, stateFileID)
	if err != nil {
		return nil, TranslateDBError("list state-file versions", err)
	}
	defer rows.Close()
	var out []*persistence.StateFileVersionRecord
	for rows.Next() {
		v, err := scanStateFileVersionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state-file versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state-file versions", err)
	}
	return out, nil
}
