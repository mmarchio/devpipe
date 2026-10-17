package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const canonicalPromptColumns = `id, project_id, phase_id, work_unit_id, context_snapshot_id, template_id, template_version, role_id, role_version, state_machine_id, state_machine_version, state_key, content, hash, estimated_tokens, created_at, cache_key, projection_id`

// CanonicalPromptRepository implements persistence.CanonicalPromptRepository
// on PostgreSQL (WBS 8.26 persistence half, P8-30). The content and every
// association/version are stored verbatim so a historical prompt is
// reconstructable after every source advances.
type CanonicalPromptRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.CanonicalPromptRepository = (*CanonicalPromptRepository)(nil)

// NewCanonicalPromptRepository binds the repository to an existing pool.
func NewCanonicalPromptRepository(pool *pgxpool.Pool) *CanonicalPromptRepository {
	return &CanonicalPromptRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *CanonicalPromptRepository) WithTx(tx persistence.Transaction) persistence.CanonicalPromptRepository {
	return &CanonicalPromptRepository{exec: txExecutor{tx}}
}

func scanCanonicalPromptRow(row interface{ Scan(dest ...any) error }) (*persistence.CanonicalPromptRecord, error) {
	var rec persistence.CanonicalPromptRecord
	var key *string
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.PhaseID, &rec.WorkUnitID,
		&rec.ContextSnapshotID, &rec.TemplateID, &rec.TemplateVersion,
		&rec.RoleID, &rec.RoleVersion, &rec.StateMachineID, &rec.StateMachineVersion,
		&rec.StateKey, &rec.Content, &rec.Hash, &rec.EstimatedTokens, &rec.CreatedAt, &key, &rec.ProjectionID); err != nil {
		return nil, err
	}
	if key != nil {
		rec.CacheKey = *key
	}
	return &rec, nil
}

// Create inserts a canonical prompt (WBS 8.26); a missing project reference
// yields persistence.ErrInvalidReference, a missing snapshot/template/role/
// machine reference likewise. The supplied timestamps are written explicitly
// (determinism criterion — the DB default must not win).
func (r *CanonicalPromptRepository) Create(ctx context.Context, rec *persistence.CanonicalPromptRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO canonical_prompts (`+canonicalPromptColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, NULLIF($17, ''), $18)`,
		rec.ID, rec.ProjectID, rec.PhaseID, rec.WorkUnitID,
		rec.ContextSnapshotID, rec.TemplateID, rec.TemplateVersion,
		rec.RoleID, rec.RoleVersion, rec.StateMachineID, rec.StateMachineVersion,
		rec.StateKey, rec.Content, rec.Hash, rec.EstimatedTokens, rec.CreatedAt.UTC(), rec.CacheKey, rec.ProjectionID); err != nil {
		return TranslateDBError("create canonical prompt", err)
	}
	return nil
}

// Get returns the canonical prompt by id; an unknown id yields
// persistence.ErrNotFound.
func (r *CanonicalPromptRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.CanonicalPromptRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+canonicalPromptColumns+` FROM canonical_prompts WHERE id = $1`, id)
	rec, err := scanCanonicalPromptRow(row)
	if err != nil {
		return nil, TranslateDBError("get canonical prompt", err)
	}
	return rec, nil
}

// ListByProject returns the project's canonical prompts in creation order; a
// project with none returns an empty slice.
func (r *CanonicalPromptRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.CanonicalPromptRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+canonicalPromptColumns+` FROM canonical_prompts WHERE project_id = $1 ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list canonical prompts", err)
	}
	defer rows.Close()
	var out []*persistence.CanonicalPromptRecord
	for rows.Next() {
		rec, err := scanCanonicalPromptRow(rows)
		if err != nil {
			return nil, TranslateDBError("list canonical prompts", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list canonical prompts", err)
	}
	return out, nil
}

// FindByCacheKey returns the project's newest canonical prompt carrying the
// Layer 1 cache key (WBS 10.54); ErrNotFound when none.
func (r *CanonicalPromptRepository) FindByCacheKey(ctx context.Context, projectID uuid.UUID, cacheKey string) (*persistence.CanonicalPromptRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+canonicalPromptColumns+` FROM canonical_prompts WHERE project_id = $1 AND cache_key = $2 ORDER BY created_at DESC, id DESC LIMIT 1`,
		projectID, cacheKey)
	rec, err := scanCanonicalPromptRow(row)
	if err != nil {
		return nil, TranslateDBError("find canonical prompt by cache key", err)
	}
	return rec, nil
}
