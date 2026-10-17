package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

const stateInstructionVersionColumns = `id, state_id, version, content, content_hash, created_at`

// StateInstructionRepository implements append-only state-instruction version
// history on PostgreSQL (WBS 6.5), mirroring the role system-prompt /
// instruction version recipe: advisory-locked MAX(version)+1.
type StateInstructionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateInstructionRepository = (*StateInstructionRepository)(nil)

// NewStateInstructionRepository binds the repository to an existing pool.
func NewStateInstructionRepository(pool *pgxpool.Pool) *StateInstructionRepository {
	return &StateInstructionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateInstructionRepository) WithTx(tx persistence.Transaction) persistence.StateInstructionRepository {
	return &StateInstructionRepository{exec: txExecutor{tx}}
}

func scanStateInstructionRow(row interface{ Scan(dest ...any) error }) (*persistence.StateInstructionRecord, error) {
	var v persistence.StateInstructionRecord
	if err := row.Scan(&v.ID, &v.StateID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// AppendVersion stores content as the next version for the state (WBS 6.5),
// advancing the state's current-version pointer in the same transaction.
func (r *StateInstructionRepository) AppendVersion(ctx context.Context, stateID uuid.UUID, content string) (*persistence.StateInstructionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendOnce(ctx, stateID, content)
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

func (r *StateInstructionRepository) appendOnce(ctx context.Context, stateID uuid.UUID, content string) (*persistence.StateInstructionRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin state instruction append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "state-instructions:"+stateID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxVersion int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM state_instruction_versions WHERE state_id = $1`, stateID).Scan(&maxVersion); err != nil {
		return nil, TranslateDBError("max state instruction version", err)
	}
	v := &persistence.StateInstructionRecord{
		ID: newUUIDString(), StateID: stateID, Version: maxVersion + 1,
		Content: content, ContentHash: role.HashSystemPromptContent(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO state_instruction_versions (id, state_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+stateInstructionVersionColumns,
		v.ID, stateID, v.Version, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.StateID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert state instruction version", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE state_instructions SET current_version = $2, updated_at = now() WHERE state_id = $1`,
		stateID, v.Version); err != nil {
		return nil, TranslateDBError("update current state instruction version", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit state instruction append", err)
		}
	}
	return v, nil
}

func (r *StateInstructionRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "state instruction append", Cause: errors.New("executor cannot begin transactions")}
}

// GetVersion returns the stored version (WBS 6.5); unknown versions yield
// persistence.ErrNotFound.
func (r *StateInstructionRepository) GetVersion(ctx context.Context, stateID uuid.UUID, version int) (*persistence.StateInstructionRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+stateInstructionVersionColumns+` FROM state_instruction_versions WHERE state_id = $1 AND version = $2`,
		stateID, version)
	v, err := scanStateInstructionRow(row)
	if err != nil {
		return nil, TranslateDBError("get state instruction version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *StateInstructionRepository) ListVersions(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateInstructionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+stateInstructionVersionColumns+` FROM state_instruction_versions WHERE state_id = $1 ORDER BY version ASC`, stateID)
	if err != nil {
		return nil, TranslateDBError("list state instruction versions", err)
	}
	defer rows.Close()
	var out []*persistence.StateInstructionRecord
	for rows.Next() {
		v, err := scanStateInstructionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state instruction versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state instruction versions", err)
	}
	return out, nil
}

// GetCurrentVersion returns the state's current instruction version (0 when
// the state has no instruction).
func (r *StateInstructionRepository) GetCurrentVersion(ctx context.Context, stateID uuid.UUID) (int, error) {
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM state_instruction_versions WHERE state_id = $1`, stateID).Scan(&cur); err != nil {
		return 0, TranslateDBError("get current state instruction version", err)
	}
	return cur, nil
}
