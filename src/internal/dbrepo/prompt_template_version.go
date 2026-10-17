package dbrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

const promptTemplateVersionColumns = `id, template_id, version, content, content_hash, created_at`

// PromptTemplateVersionRepository implements append-only prompt-template
// content versioning on PostgreSQL (WBS 8.4 persistence half, P8-11),
// mirroring the state-file-version recipe: the next version is
// MAX(version)+1 computed inside a transaction serialized by a
// template-scoped advisory lock, wrapped in a bounded retry on the
// (template_id, version) unique constraint (T8.4.5), and the content hash is
// the P8-02 domain function (T8.4.6).
type PromptTemplateVersionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.PromptTemplateVersionRepository = (*PromptTemplateVersionRepository)(nil)

// NewPromptTemplateVersionRepository binds the repository to an existing pool.
func NewPromptTemplateVersionRepository(pool *pgxpool.Pool) *PromptTemplateVersionRepository {
	return &PromptTemplateVersionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *PromptTemplateVersionRepository) WithTx(tx persistence.Transaction) persistence.PromptTemplateVersionRepository {
	return &PromptTemplateVersionRepository{exec: txExecutor{tx}}
}

func scanPromptTemplateVersionRow(row interface{ Scan(dest ...any) error }) (*persistence.PromptTemplateVersionRecord, error) {
	var rec persistence.PromptTemplateVersionRecord
	if err := row.Scan(&rec.ID, &rec.TemplateID, &rec.Version, &rec.Content, &rec.ContentHash, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// AppendVersion stores content as the next version (WBS 8.4). A missing
// template yields persistence.ErrNotFound BEFORE any write.
func (r *PromptTemplateVersionRepository) AppendVersion(ctx context.Context, templateID uuid.UUID, content string) (*persistence.PromptTemplateVersionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendOnce(ctx, templateID, content)
		if err == nil || !errors.Is(err, persistence.ErrConflict) {
			return v, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *PromptTemplateVersionRepository) templateExists(ctx context.Context, templateID uuid.UUID) error {
	var exists bool
	if err := r.exec.QueryRow(ctx, `SELECT true FROM prompt_templates WHERE id = $1`, templateID).Scan(&exists); err != nil {
		return TranslateDBError("get prompt template", err)
	}
	if !exists {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *PromptTemplateVersionRepository) appendOnce(ctx context.Context, templateID uuid.UUID, content string) (*persistence.PromptTemplateVersionRecord, error) {
	// Fast-path template existence check: a missing template is a plain
	// ErrNotFound, not a conflict.
	if err := r.templateExists(ctx, templateID); err != nil {
		return nil, err
	}
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin prompt template version append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "prompt-template-versions:"+templateID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var cur int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM prompt_template_versions WHERE template_id = $1`, templateID).
		Scan(&cur); err != nil {
		return nil, TranslateDBError("max prompt template version", err)
	}
	next := cur + 1
	v, err := r.insertVersion(ctx, tx, templateID, content, next)
	if err != nil {
		return nil, err
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit prompt template version append", err)
		}
	}
	return v, nil
}

func (r *PromptTemplateVersionRepository) insertVersion(ctx context.Context, tx persistence.Transaction, templateID uuid.UUID, content string, version int) (*persistence.PromptTemplateVersionRecord, error) {
	v := &persistence.PromptTemplateVersionRecord{
		ID: uuid.New(), TemplateID: templateID, Version: version,
		Content: content, ContentHash: prompt.ContentHash(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO prompt_template_versions (id, template_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+promptTemplateVersionColumns,
		v.ID, templateID, version, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.TemplateID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert prompt template version", err)
	}
	return v, nil
}

func (r *PromptTemplateVersionRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "prompt template version append", Cause: errors.New("executor cannot begin transactions")}
}

// GetCurrentVersion returns the template's current (highest) version,
// resolved from the stored history; an unknown template yields ErrNotFound
// and a template with no versions returns (nil, nil).
func (r *PromptTemplateVersionRepository) GetCurrentVersion(ctx context.Context, templateID uuid.UUID) (*persistence.PromptTemplateVersionRecord, error) {
	if err := r.templateExists(ctx, templateID); err != nil {
		return nil, err
	}
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM prompt_template_versions WHERE template_id = $1`, templateID).Scan(&cur); err != nil {
		return nil, TranslateDBError("get current prompt template version", err)
	}
	if cur == 0 {
		return nil, nil
	}
	return r.GetVersion(ctx, templateID, cur)
}

// GetVersion returns the stored version; a missing version yields
// persistence.ErrNotFound.
func (r *PromptTemplateVersionRepository) GetVersion(ctx context.Context, templateID uuid.UUID, version int) (*persistence.PromptTemplateVersionRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+promptTemplateVersionColumns+` FROM prompt_template_versions WHERE template_id = $1 AND version = $2`,
		templateID, version)
	v, err := scanPromptTemplateVersionRow(row)
	if err != nil {
		return nil, TranslateDBError("get prompt template version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *PromptTemplateVersionRepository) ListVersions(ctx context.Context, templateID uuid.UUID) ([]*persistence.PromptTemplateVersionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+promptTemplateVersionColumns+` FROM prompt_template_versions WHERE template_id = $1 ORDER BY version ASC`, templateID)
	if err != nil {
		return nil, TranslateDBError("list prompt template versions", err)
	}
	defer rows.Close()
	var out []*persistence.PromptTemplateVersionRecord
	for rows.Next() {
		v, err := scanPromptTemplateVersionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list prompt template versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list prompt template versions", err)
	}
	return out, nil
}
