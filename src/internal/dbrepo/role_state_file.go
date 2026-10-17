package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const stateFileColumns = `id, role_id, state_file_key, position, created_at`

// RoleStateFileRepository implements role state-file requirements on
// PostgreSQL (WBS 5.12).
type RoleStateFileRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.RoleStateFileRepository = (*RoleStateFileRepository)(nil)

// NewRoleStateFileRepository binds the repository to an existing pool.
func NewRoleStateFileRepository(pool *pgxpool.Pool) *RoleStateFileRepository {
	return &RoleStateFileRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *RoleStateFileRepository) WithTx(tx persistence.Transaction) persistence.RoleStateFileRepository {
	return &RoleStateFileRepository{exec: txExecutor{tx}}
}

func scanStateFileRow(row interface{ Scan(dest ...any) error }) (*persistence.RoleStateFileRecord, error) {
	var rec persistence.RoleStateFileRecord
	if err := row.Scan(&rec.ID, &rec.RoleID, &rec.StateFileKey, &rec.Position, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Require records a state-file type at the next deterministic position
// (MAX(position)+1, 1 when none) under a role-scoped advisory lock so
// concurrent requires get distinct positions (T5.12.4). A duplicate
// (role, state_file_key) yields ErrConflict (T5.12.3).
func (r *RoleStateFileRepository) Require(ctx context.Context, roleID uuid.UUID, stateFileKey string) (*persistence.RoleStateFileRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rec, err := r.requireOnce(ctx, roleID, stateFileKey)
		if err == nil {
			return rec, nil
		}
		if !errors.Is(err, persistence.ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *RoleStateFileRepository) requireOnce(ctx context.Context, roleID uuid.UUID, stateFileKey string) (*persistence.RoleStateFileRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin state-file", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "role-state-files:"+roleID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxPos int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(position), 0) FROM role_state_files WHERE role_id = $1`, roleID).Scan(&maxPos); err != nil {
		return nil, TranslateDBError("max state-file position", err)
	}
	rec := &persistence.RoleStateFileRecord{
		ID: uuid.New(), RoleID: roleID, StateFileKey: stateFileKey, Position: maxPos + 1,
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO role_state_files (id, role_id, state_file_key, position)
		 VALUES ($1, $2, $3, $4) RETURNING `+stateFileColumns,
		rec.ID, roleID, stateFileKey, rec.Position)
	if err := row.Scan(&rec.ID, &rec.RoleID, &rec.StateFileKey, &rec.Position, &rec.CreatedAt); err != nil {
		return nil, TranslateDBError("insert state-file", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit state-file", err)
		}
	}
	return rec, nil
}

func (r *RoleStateFileRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "state-file", Cause: errors.New("executor cannot begin transactions")}
}

// Remove deletes the role's state-file requirement row only — the state-file
// type itself is not owned by the role, so other roles are unaffected
// (T5.12.5).
func (r *RoleStateFileRepository) Remove(ctx context.Context, roleID uuid.UUID, stateFileKey string) error {
	tag, err := r.exec.Exec(ctx,
		`DELETE FROM role_state_files WHERE role_id = $1 AND state_file_key = $2`, roleID, stateFileKey)
	if err != nil {
		return TranslateDBError("remove state-file", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByRole returns the role's state-file requirements in position order
// (deterministic, T5.12.4).
func (r *RoleStateFileRepository) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*persistence.RoleStateFileRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+stateFileColumns+` FROM role_state_files WHERE role_id = $1 ORDER BY position ASC`, roleID)
	if err != nil {
		return nil, TranslateDBError("list state-files", err)
	}
	defer rows.Close()
	var out []*persistence.RoleStateFileRecord
	for rows.Next() {
		rec, err := scanStateFileRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state-files", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state-files", err)
	}
	return out, nil
}
