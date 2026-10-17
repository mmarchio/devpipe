package dbrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

const instructionVersionColumns = `id, instruction_id, version, content, content_hash, created_at`

// InstructionVersionRepository implements append-only instruction version
// history on PostgreSQL (WBS 5.8), mirroring the WBS/prompt recipe.
type InstructionVersionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.InstructionVersionRepository = (*InstructionVersionRepository)(nil)

// NewInstructionVersionRepository binds the repository to an existing pool.
func NewInstructionVersionRepository(pool *pgxpool.Pool) *InstructionVersionRepository {
	return &InstructionVersionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *InstructionVersionRepository) WithTx(tx persistence.Transaction) persistence.InstructionVersionRepository {
	return &InstructionVersionRepository{exec: txExecutor{tx}}
}

func scanInstructionVersionRow(row interface{ Scan(dest ...any) error }) (*persistence.InstructionVersionRecord, error) {
	var v persistence.InstructionVersionRecord
	if err := row.Scan(&v.ID, &v.InstructionID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// AppendVersion stores content as the next version (WBS 5.8), advancing the
// instruction's current-version pointer in the same transaction.
func (r *InstructionVersionRepository) AppendVersion(ctx context.Context, instructionID, content string) (*persistence.InstructionVersionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendOnce(ctx, instructionID, content)
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

// UpdateWithExpected appends a new version only when the instruction's
// current version equals expectedVersion (T5.8.5); a stale expected version
// is ErrConflict with no version written.
func (r *InstructionVersionRepository) UpdateWithExpected(ctx context.Context, instructionID, content string, expectedVersion int) (*persistence.InstructionVersionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.updateOnce(ctx, instructionID, content, expectedVersion)
		if err == nil || !errors.Is(err, persistence.ErrConflict) {
			return v, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *InstructionVersionRepository) updateOnce(ctx context.Context, instructionID, content string, expectedVersion int) (*persistence.InstructionVersionRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin instruction version update", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "instruction-versions:"+instructionID); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var cur int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM instruction_versions WHERE instruction_id = $1`, instructionID).Scan(&cur); err != nil {
		return nil, TranslateDBError("current instruction version", err)
	}
	if cur != expectedVersion {
		_ = tx.Rollback(ctx)
		if joined {
			return nil, persistence.ErrConflict
		}
		return nil, persistence.ErrConflict
	}
	v, err := r.insertVersion(ctx, tx, joined, instructionID, content, cur+1)
	if err != nil {
		return nil, err
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit instruction version update", err)
		}
	}
	return v, nil
}

func (r *InstructionVersionRepository) appendOnce(ctx context.Context, instructionID, content string) (*persistence.InstructionVersionRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin instruction version append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "instruction-versions:"+instructionID); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var cur int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM instruction_versions WHERE instruction_id = $1`, instructionID).Scan(&cur); err != nil {
		return nil, TranslateDBError("max instruction version", err)
	}
	v, err := r.insertVersion(ctx, tx, joined, instructionID, content, cur+1)
	if err != nil {
		return nil, err
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit instruction version append", err)
		}
	}
	return v, nil
}

// insertVersion inserts the version row and advances the current pointer on
// the instructions row (no-op if the instructions row is absent).
func (r *InstructionVersionRepository) insertVersion(ctx context.Context, tx persistence.Transaction, joined bool, instructionID, content string, version int) (*persistence.InstructionVersionRecord, error) {
	v := &persistence.InstructionVersionRecord{
		ID: newUUIDString(), InstructionID: instructionID, Version: version,
		Content: content, ContentHash: role.HashSystemPromptContent(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO instruction_versions (id, instruction_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+instructionVersionColumns,
		v.ID, instructionID, version, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.InstructionID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert instruction version", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE instructions SET current_version = $2, updated_at = now() WHERE id = $1`,
		instructionID, version); err != nil {
		return nil, TranslateDBError("update current instruction version", err)
	}
	_ = joined
	return v, nil
}

func (r *InstructionVersionRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "instruction version append", Cause: errors.New("executor cannot begin transactions")}
}

// GetVersion returns the stored version; unknown versions yield
// persistence.ErrNotFound.
func (r *InstructionVersionRepository) GetVersion(ctx context.Context, instructionID string, version int) (*persistence.InstructionVersionRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+instructionVersionColumns+` FROM instruction_versions WHERE instruction_id = $1 AND version = $2`,
		instructionID, version)
	v, err := scanInstructionVersionRow(row)
	if err != nil {
		return nil, TranslateDBError("get instruction version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *InstructionVersionRepository) ListVersions(ctx context.Context, instructionID string) ([]*persistence.InstructionVersionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+instructionVersionColumns+` FROM instruction_versions WHERE instruction_id = $1 ORDER BY version ASC`, instructionID)
	if err != nil {
		return nil, TranslateDBError("list instruction versions", err)
	}
	defer rows.Close()
	var out []*persistence.InstructionVersionRecord
	for rows.Next() {
		v, err := scanInstructionVersionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list instruction versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list instruction versions", err)
	}
	return out, nil
}

// GetCurrentVersion returns the current version (0 when the instruction has
// no versions).
func (r *InstructionVersionRepository) GetCurrentVersion(ctx context.Context, instructionID string) (int, error) {
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM instruction_versions WHERE instruction_id = $1`, instructionID).Scan(&cur); err != nil {
		return 0, TranslateDBError("get current instruction version", err)
	}
	return cur, nil
}
