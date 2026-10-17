package dbrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

const wbsVersionColumns = `id, phase_id, version, content, content_hash, created_at`

// WBSVersionRepository implements append-only WBS version history on
// PostgreSQL (WBS 4.10/4.11).
type WBSVersionRepository struct {
	exec executor
}

// NewWBSVersionRepository binds the repository to an existing pool.
func NewWBSVersionRepository(pool *pgxpool.Pool) *WBSVersionRepository {
	return &WBSVersionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *WBSVersionRepository) WithTx(tx persistence.Transaction) persistence.WBSVersionRepository {
	return &WBSVersionRepository{exec: txExecutor{tx}}
}

func scanWBSVersionRow(row interface{ Scan(dest ...any) error }) (*persistence.WBSVersionRecord, error) {
	var v persistence.WBSVersionRecord
	if err := row.Scan(&v.ID, &v.PhaseID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// AppendVersion stores content as the next version for the phase:
// MAX(version)+1 (1 when the phase has no versions), hashed via
// wbs.HashContent, with the phase_documents.current_version pointer advanced
// in the same transaction (WBS 4.10). The computation runs under a
// phase-scoped advisory lock with a bounded retry on the UNIQUE
// (phase_id, version) constraint, so concurrent appends always receive
// distinct versions (T4.10.7).
func (r *WBSVersionRepository) AppendVersion(ctx context.Context, phaseID, content string) (*persistence.WBSVersionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendOnce(ctx, phaseID, content)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, persistence.ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *WBSVersionRepository) appendOnce(ctx context.Context, phaseID, content string) (*persistence.WBSVersionRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin wbs version append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx) // no-op after commit
		}
	}()

	// Serialize appends per phase without blocking other phases.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "wbs-versions:"+phaseID); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxVersion int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM wbs_versions WHERE phase_id = $1`, phaseID).Scan(&maxVersion); err != nil {
		return nil, TranslateDBError("max wbs version", err)
	}
	v := &persistence.WBSVersionRecord{
		ID: newUUIDString(), PhaseID: phaseID, Version: maxVersion + 1,
		Content: content, ContentHash: wbs.HashContent(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO wbs_versions (id, phase_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+wbsVersionColumns,
		v.ID, phaseID, v.Version, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.PhaseID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert wbs version", err)
	}
	// The phase_documents row is the current-version pointer; it exists only
	// once a WBS document has been created for the phase (WBS 3.5), so the
	// update is a no-op until then. The current version is always derivable
	// from wbs_versions itself (GetCurrentVersion).
	if _, err := tx.Exec(ctx,
		`UPDATE phase_documents SET current_version = $2 WHERE phase_id = $1 AND doc_type = 'WBS'`,
		phaseID, v.Version); err != nil {
		return nil, TranslateDBError("update current wbs version", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit wbs version append", err)
		}
	}
	return v, nil
}

// beginTx returns the serializing transaction for the append (same recipe as
// phase order allocation, WBS 2.7). When the repository is already bound to
// a transaction (WithTx), the append joins that transaction so the whole
// outer unit stays atomic (WBS 2.21) and the caller owns the commit.
func (r *WBSVersionRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "wbs version append", Cause: errors.New("executor cannot begin transactions")}
}

// GetVersion returns the stored version (T4.10.4); unknown versions yield
// persistence.ErrNotFound.
func (r *WBSVersionRepository) GetVersion(ctx context.Context, phaseID string, version int) (*persistence.WBSVersionRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+wbsVersionColumns+` FROM wbs_versions WHERE phase_id = $1 AND version = $2`,
		phaseID, version)
	v, err := scanWBSVersionRow(row)
	if err != nil {
		return nil, TranslateDBError("get wbs version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending version order
// (T4.10.6).
func (r *WBSVersionRepository) ListVersions(ctx context.Context, phaseID string) ([]*persistence.WBSVersionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+wbsVersionColumns+` FROM wbs_versions WHERE phase_id = $1 ORDER BY version ASC`, phaseID)
	if err != nil {
		return nil, TranslateDBError("list wbs versions", err)
	}
	defer rows.Close()
	var out []*persistence.WBSVersionRecord
	for rows.Next() {
		v, err := scanWBSVersionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list wbs versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list wbs versions", err)
	}
	return out, nil
}

// GetCurrentVersion returns the phase's current WBS version (T4.10.5) — the
// highest stored version, 0 when the phase has no WBS versions.
func (r *WBSVersionRepository) GetCurrentVersion(ctx context.Context, phaseID string) (int, error) {
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM wbs_versions WHERE phase_id = $1`, phaseID).Scan(&cur); err != nil {
		return 0, TranslateDBError("get current wbs version", err)
	}
	return cur, nil
}
