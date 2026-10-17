package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

const promptTemplateColumns = `id, project_id, key, name, description, purpose, enabled, version, created_at, updated_at`

// PromptTemplateRepository implements persistence.PromptTemplateRepository
// on PostgreSQL (WBS 8.1 persistence half, P8-10). Scoped-key uniqueness
// rides on the (COALESCE(project_id, sentinel), key) unique index
// (T8.1.3 DB half).
type PromptTemplateRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.PromptTemplateRepository = (*PromptTemplateRepository)(nil)

// NewPromptTemplateRepository binds the repository to an existing pool.
func NewPromptTemplateRepository(pool *pgxpool.Pool) *PromptTemplateRepository {
	return &PromptTemplateRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *PromptTemplateRepository) WithTx(tx persistence.Transaction) persistence.PromptTemplateRepository {
	return &PromptTemplateRepository{exec: txExecutor{tx}}
}

func scanPromptTemplateRow(row interface{ Scan(dest ...any) error }) (*persistence.PromptTemplateRecord, error) {
	var rec persistence.PromptTemplateRecord
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.Key, &rec.Name, &rec.Description, &rec.Purpose, &rec.Enabled, &rec.Version, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Create inserts a new prompt template (WBS 8.1); a scoped key duplicate
// yields persistence.ErrConflict (T8.1.3 DB half), a missing project
// reference yields persistence.ErrInvalidReference.
func (r *PromptTemplateRepository) Create(ctx context.Context, rec *persistence.PromptTemplateRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO prompt_templates (id, project_id, key, name, description, purpose, enabled, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.ID, rec.ProjectID, rec.Key, rec.Name, rec.Description, rec.Purpose, rec.Enabled, rec.Version, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create prompt template", err)
	}
	return nil
}

// Get returns the prompt template with the given id; an unknown id yields
// persistence.ErrNotFound.
func (r *PromptTemplateRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.PromptTemplateRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+promptTemplateColumns+` FROM prompt_templates WHERE id = $1`, id)
	rec, err := scanPromptTemplateRow(row)
	if err != nil {
		return nil, TranslateDBError("get prompt template", err)
	}
	return rec, nil
}

// GetByKey returns the template for (scope, key); an unknown key in scope
// yields persistence.ErrNotFound.
func (r *PromptTemplateRepository) GetByKey(ctx context.Context, projectID *uuid.UUID, key string) (*persistence.PromptTemplateRecord, error) {
	key = prompt.NormalizeKey(key)
	var row interface{ Scan(dest ...any) error }
	if projectID == nil {
		row = r.exec.QueryRow(ctx, `SELECT `+promptTemplateColumns+` FROM prompt_templates WHERE project_id IS NULL AND key = $1`, key)
	} else {
		row = r.exec.QueryRow(ctx, `SELECT `+promptTemplateColumns+` FROM prompt_templates WHERE project_id = $1 AND key = $2`, *projectID, key)
	}
	rec, err := scanPromptTemplateRow(row)
	if err != nil {
		return nil, TranslateDBError("get prompt template by key", err)
	}
	return rec, nil
}

// ListGlobal returns the global-scope templates (project_id NULL) in key
// order (P8-12 seed lookup).
func (r *PromptTemplateRepository) ListGlobal(ctx context.Context) ([]*persistence.PromptTemplateRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+promptTemplateColumns+` FROM prompt_templates WHERE project_id IS NULL ORDER BY key ASC`)
	if err != nil {
		return nil, TranslateDBError("list global prompt templates", err)
	}
	defer rows.Close()
	var out []*persistence.PromptTemplateRecord
	for rows.Next() {
		rec, err := scanPromptTemplateRow(rows)
		if err != nil {
			return nil, TranslateDBError("list global prompt templates", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list global prompt templates", err)
	}
	return out, nil
}

// Update mutates the template's mutable fields (name, description, purpose,
// enabled) in place. Key and version are immutable: the key is the identity
// and the version is bumped by the versioned content-append surface of a later
// task. An unknown id yields persistence.ErrNotFound.
func (r *PromptTemplateRepository) Update(ctx context.Context, rec *persistence.PromptTemplateRecord) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_templates
		 SET name = $2, description = $3, purpose = $4, enabled = $5, updated_at = now()
		 WHERE id = $1`,
		rec.ID, rec.Name, rec.Description, rec.Purpose, rec.Enabled)
	if err != nil {
		return TranslateDBError("update prompt template", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListForProject returns the global templates plus the project's own
// templates, global first, each in key order (P17-45).
func (r *PromptTemplateRepository) ListForProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.PromptTemplateRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+promptTemplateColumns+` FROM prompt_templates
		WHERE project_id IS NULL OR project_id = $1 ORDER BY project_id NULLS FIRST, key ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list project prompt templates", err)
	}
	defer rows.Close()
	var out []*persistence.PromptTemplateRecord
	for rows.Next() {
		rec, err := scanPromptTemplateRow(rows)
		if err != nil {
			return nil, TranslateDBError("list project prompt templates", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list project prompt templates", err)
	}
	return out, nil
}
