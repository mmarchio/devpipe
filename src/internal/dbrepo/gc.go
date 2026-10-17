package dbrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Garbage collection (P18-55, WBS 18.99–18.104). Each call handles one
// bounded batch in one statement: candidate rows are locked FOR UPDATE
// SKIP LOCKED, so a row a concurrent transaction is referencing (a cache
// hit updating it, an insert whose foreign key holds a key-share lock on
// it) is skipped, and the reference checks run in the same statement.
// Every call is idempotent: re-running after a crash only finds what is
// still eligible. Audit rows are never touched here.

// GCRepository runs the bounded GC statements.
type GCRepository struct{ pool *pgxpool.Pool }

// NewGCRepository binds the repository.
func NewGCRepository(pool *pgxpool.Pool) *GCRepository { return &GCRepository{pool: pool} }

func (r *GCRepository) exec(ctx context.Context, op, sql string, args ...any) (int, error) {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, TranslateDBError(op, err)
	}
	return int(tag.RowsAffected()), nil
}

// ExpireCacheEntries deletes prompt cache entries unused since cutoff
// (never one being built under a live lease). The prompts an entry
// points at are not touched: execution history keeps them (WBS 18.99).
func (r *GCRepository) ExpireCacheEntries(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.exec(ctx, "gc cache entries", `DELETE FROM prompt_cache_entries WHERE id IN (
		SELECT id FROM prompt_cache_entries
		WHERE last_used_at < $1 AND NOT (status = 'BUILDING' AND lease_expires_at > now())
		ORDER BY last_used_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, cutoff.UTC(), batch)
}

// ExpirePrecomputedTemplates deletes precomputed template index rows
// older than cutoff or whose unit's phase completed (the canonical prompts
// they point at stay).
func (r *GCRepository) ExpirePrecomputedTemplates(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.exec(ctx, "gc precomputed templates", `DELETE FROM precomputed_templates WHERE id IN (
		SELECT t.id FROM precomputed_templates t
		WHERE t.created_at < $1 OR EXISTS (SELECT 1 FROM work_units w JOIN execution_plans ep ON ep.id = w.execution_plan_id
			JOIN phases p ON p.id = ep.phase_id WHERE w.id = t.work_unit_id AND p.status = 'COMPLETED')
		ORDER BY t.created_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, cutoff.UTC(), batch)
}

// ExpireProjections deletes precompute projections older than cutoff (or
// of a completed phase) that no canonical prompt and no template
// references (a referenced projection is part of a prompt's lineage).
func (r *GCRepository) ExpireProjections(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.exec(ctx, "gc precompute projections", `DELETE FROM precompute_projections WHERE id IN (
		SELECT pp.id FROM precompute_projections pp
		WHERE (pp.created_at < $1 OR EXISTS (SELECT 1 FROM phases p WHERE p.id = pp.phase_id AND p.status = 'COMPLETED'))
		AND NOT EXISTS (SELECT 1 FROM canonical_prompts cp WHERE cp.projection_id = pp.id)
		AND NOT EXISTS (SELECT 1 FROM precomputed_templates t WHERE t.projection_id = pp.id)
		ORDER BY pp.created_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		AND NOT EXISTS (SELECT 1 FROM canonical_prompts cp WHERE cp.projection_id = precompute_projections.id)`, cutoff.UTC(), batch)
}

// ExpirePrecomputeJobs deletes finished precompute jobs (and their
// candidates) older than cutoff.
func (r *GCRepository) ExpirePrecomputeJobs(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.exec(ctx, "gc precompute jobs", `DELETE FROM precompute_jobs WHERE id IN (
		SELECT id FROM precompute_jobs WHERE status IN ('COMPLETED', 'PARTIAL', 'FAILED', 'CANCELLED') AND COALESCE(completed_at, created_at) < $1
		ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, cutoff.UTC(), batch)
}

// ExpireValidationLogs empties raw stdout / stderr of command results
// older than cutoff, recording their sha256 and log_expired_at; outcome,
// exit code and timing are kept (the migration-sanctioned exemption,
// WBS 18.101).
func (r *GCRepository) ExpireValidationLogs(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.exec(ctx, "gc validation logs", `UPDATE validation_command_results SET
		stdout_sha256 = encode(sha256(convert_to(stdout, 'UTF8')), 'hex'), stderr_sha256 = encode(sha256(convert_to(stderr, 'UTF8')), 'hex'),
		stdout = '', stderr = '', log_expired_at = now()
		WHERE id IN (SELECT id FROM validation_command_results WHERE log_expired_at IS NULL AND created_at < $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, cutoff.UTC(), batch)
}
