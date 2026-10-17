package dbrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

const promptColumns = `id, role_id, version, content, content_hash, created_at`

// SystemPromptRepository implements append-only system-prompt version history
// on PostgreSQL (WBS 5.5), mirroring the WBS version recipe: advisory-locked
// MAX(version)+1, deterministic concurrency.
type SystemPromptRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.SystemPromptRepository = (*SystemPromptRepository)(nil)

// NewSystemPromptRepository binds the repository to an existing pool.
func NewSystemPromptRepository(pool *pgxpool.Pool) *SystemPromptRepository {
	return &SystemPromptRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *SystemPromptRepository) WithTx(tx persistence.Transaction) persistence.SystemPromptRepository {
	return &SystemPromptRepository{exec: txExecutor{tx}}
}

func scanPromptRow(row interface{ Scan(dest ...any) error }) (*persistence.SystemPromptRecord, error) {
	var v persistence.SystemPromptRecord
	if err := row.Scan(&v.ID, &v.RoleID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// AppendVersion stores content as the next version for the role (WBS 5.5),
// advancing the role's current-version pointer in the same transaction.
func (r *SystemPromptRepository) AppendVersion(ctx context.Context, roleID, content string) (*persistence.SystemPromptRecord, error) {
	if err := role.ValidateSystemPrompt(content); err != nil {
		return nil, err
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendOnce(ctx, roleID, content)
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

func (r *SystemPromptRepository) appendOnce(ctx context.Context, roleID, content string) (*persistence.SystemPromptRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin prompt append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "role-prompts:"+roleID); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxVersion int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM role_system_prompts WHERE role_id = $1`, roleID).Scan(&maxVersion); err != nil {
		return nil, TranslateDBError("max prompt version", err)
	}
	v := &persistence.SystemPromptRecord{
		ID: newUUIDString(), RoleID: roleID, Version: maxVersion + 1,
		Content: content, ContentHash: role.HashSystemPromptContent(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO role_system_prompts (id, role_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+promptColumns,
		v.ID, roleID, v.Version, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.RoleID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert prompt version", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE roles SET current_system_prompt_version = $2 WHERE id = $1`,
		roleID, v.Version); err != nil {
		return nil, TranslateDBError("update current prompt version", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit prompt append", err)
		}
	}
	return v, nil
}

func (r *SystemPromptRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "prompt append", Cause: errors.New("executor cannot begin transactions")}
}

// GetVersion returns the stored version (WBS 5.5); unknown versions yield
// persistence.ErrNotFound.
func (r *SystemPromptRepository) GetVersion(ctx context.Context, roleID string, version int) (*persistence.SystemPromptRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+promptColumns+` FROM role_system_prompts WHERE role_id = $1 AND version = $2`,
		roleID, version)
	v, err := scanPromptRow(row)
	if err != nil {
		return nil, TranslateDBError("get prompt version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *SystemPromptRepository) ListVersions(ctx context.Context, roleID string) ([]*persistence.SystemPromptRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+promptColumns+` FROM role_system_prompts WHERE role_id = $1 ORDER BY version ASC`, roleID)
	if err != nil {
		return nil, TranslateDBError("list prompt versions", err)
	}
	defer rows.Close()
	var out []*persistence.SystemPromptRecord
	for rows.Next() {
		v, err := scanPromptRow(rows)
		if err != nil {
			return nil, TranslateDBError("list prompt versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list prompt versions", err)
	}
	return out, nil
}

// GetCurrentVersion returns the role's current prompt version (0 when the
// role has no system prompt).
func (r *SystemPromptRepository) GetCurrentVersion(ctx context.Context, roleID string) (int, error) {
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM role_system_prompts WHERE role_id = $1`, roleID).Scan(&cur); err != nil {
		return 0, TranslateDBError("get current prompt version", err)
	}
	return cur, nil
}
