package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/cache"
	"overseer/internal/persistence"
)

// PromptCacheRepository implements persistence.PromptCacheRepository on
// PostgreSQL (WBS 10.13). Lookups use (project_id, cache_key[, layer,
// status]) indexes only — no prompt content is ever read or compared.
type PromptCacheRepository struct{ exec executor }

var _ persistence.PromptCacheRepository = (*PromptCacheRepository)(nil)

// NewPromptCacheRepository binds the repository to a pool.
func NewPromptCacheRepository(pool *pgxpool.Pool) *PromptCacheRepository {
	return &PromptCacheRepository{exec: poolAdapter{pool}}
}

const cacheEntryColumns = `id, project_id, cache_key, layer, canonical_prompt_id, compressed_prompt_id, worker_prompt_id, status, source,
	state_key, phase_id, work_unit_id, metadata, build_owner, lease_expires_at, error_kind, error_message, retry_after,
	invalidation_reason, compression_calls, compression_input_tokens, build_duration_ms, hit_count, created_at, last_used_at`

func scanCacheEntry(row interface{ Scan(dest ...any) error }) (*cache.PromptCacheEntry, error) {
	var e cache.PromptCacheEntry
	var layer, status, source string
	var md []byte
	var durMS int64
	var hits int64
	if err := row.Scan(&e.ID, &e.ProjectID, &e.CacheKey, &layer, &e.CanonicalPromptID, &e.CompressedPromptID, &e.WorkerPromptID,
		&status, &source, &e.StateKey, &e.PhaseID, &e.WorkUnitID, &md, &e.BuildOwner, &e.LeaseExpiresAt, &e.ErrorKind,
		&e.ErrorMessage, &e.RetryAfter, &e.InvalidationReason, &e.CompressionCalls, &e.CompressionInputTokens, &durMS,
		&hits, &e.CreatedAt, &e.LastUsedAt); err != nil {
		return nil, err
	}
	e.Layer, e.Status, e.Source = cache.Layer(layer), cache.Status(status), cache.EntrySource(source)
	e.BuildDuration = time.Duration(durMS) * time.Millisecond
	e.HitCount = uint64(hits)
	if len(md) > 0 {
		_ = json.Unmarshal(md, &e.Metadata)
	}
	return &e, nil
}

func (r *PromptCacheRepository) one(ctx context.Context, op, where string, args ...any) (*cache.PromptCacheEntry, error) {
	e, err := scanCacheEntry(r.exec.QueryRow(ctx, `SELECT `+cacheEntryColumns+` FROM prompt_cache_entries WHERE `+where, args...))
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	return e, nil
}

// GetValid returns the VALID entry for the key.
func (r *PromptCacheRepository) GetValid(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error) {
	return r.one(ctx, "get valid cache entry", `project_id = $1 AND cache_key = $2 AND layer = $3 AND status = 'VALID'`, projectID, key, string(layer))
}

// GetAuthoritative returns the VALID or BUILDING entry for the key.
func (r *PromptCacheRepository) GetAuthoritative(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error) {
	return r.one(ctx, "get authoritative cache entry", `project_id = $1 AND cache_key = $2 AND layer = $3 AND status IN ('VALID', 'BUILDING')`, projectID, key, string(layer))
}

// LatestFailed returns the newest FAILED entry for the key.
func (r *PromptCacheRepository) LatestFailed(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error) {
	return r.one(ctx, "get failed cache entry", `project_id = $1 AND cache_key = $2 AND layer = $3 AND status = 'FAILED' ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, key, string(layer))
}

// Get returns one entry by id.
func (r *PromptCacheRepository) Get(ctx context.Context, id uuid.UUID) (*cache.PromptCacheEntry, error) {
	return r.one(ctx, "get cache entry", `id = $1`, id)
}

// Insert stores a VALID or BUILDING entry; on an authoritative conflict the
// existing winner is returned (WBS 10.30).
func (r *PromptCacheRepository) Insert(ctx context.Context, e *cache.PromptCacheEntry) (*cache.PromptCacheEntry, bool, error) {
	md, err := json.Marshal(e.Metadata)
	if err != nil {
		return nil, false, &persistence.InfrastructureError{Op: "encode cache metadata", Cause: err}
	}
	tag, err := r.exec.Exec(ctx,
		`INSERT INTO prompt_cache_entries (id, project_id, cache_key, layer, canonical_prompt_id, compressed_prompt_id, worker_prompt_id,
		   status, source, state_key, phase_id, work_unit_id, metadata, build_owner, lease_expires_at, compression_calls,
		   compression_input_tokens, build_duration_ms, created_at, last_used_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		 ON CONFLICT (project_id, cache_key, layer) WHERE status IN ('VALID', 'BUILDING') DO NOTHING`,
		e.ID, e.ProjectID, e.CacheKey, string(e.Layer), e.CanonicalPromptID, e.CompressedPromptID, e.WorkerPromptID,
		string(e.Status), string(e.Source), e.StateKey, e.PhaseID, e.WorkUnitID, md, e.BuildOwner, e.LeaseExpiresAt,
		e.CompressionCalls, e.CompressionInputTokens, e.BuildDuration.Milliseconds(), e.CreatedAt.UTC(), e.LastUsedAt.UTC())
	if err != nil {
		return nil, false, TranslateDBError("insert cache entry", err)
	}
	if tag.RowsAffected() == 1 {
		cp := *e
		return &cp, true, nil
	}
	w, err := r.GetAuthoritative(ctx, e.ProjectID, e.CacheKey, e.Layer)
	if errors.Is(err, persistence.ErrNotFound) {
		// The winner finished (or failed) between the conflict and the read;
		// report a retryable conflict.
		return nil, false, persistence.ErrConflict
	}
	return w, false, err
}

// ReclaimLease takes over an expired BUILDING lease.
func (r *PromptCacheRepository) ReclaimLease(ctx context.Context, id uuid.UUID, owner string, expires, now time.Time) (bool, error) {
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET build_owner = $2, lease_expires_at = $3
		 WHERE id = $1 AND status = 'BUILDING' AND (lease_expires_at IS NULL OR lease_expires_at <= $4)`,
		id, owner, expires.UTC(), now.UTC())
	if err != nil {
		return false, TranslateDBError("reclaim cache lease", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinalizeValid completes an owned BUILDING entry.
func (r *PromptCacheRepository) FinalizeValid(ctx context.Context, id uuid.UUID, owner string, now time.Time, b persistence.BuildResult) (bool, error) {
	md, err := json.Marshal(b.Metadata)
	if err != nil {
		return false, &persistence.InfrastructureError{Op: "encode cache metadata", Cause: err}
	}
	src := b.Source
	if src == "" {
		src = cache.SourceOnDemand
	}
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET status = 'VALID', canonical_prompt_id = $3, compressed_prompt_id = $4, worker_prompt_id = $5,
		   metadata = $6, source = $7, compression_calls = $8, compression_input_tokens = $9, build_duration_ms = $10,
		   last_used_at = $11, lease_expires_at = NULL
		 WHERE id = $1 AND status = 'BUILDING' AND build_owner = $2 AND lease_expires_at > $11`,
		id, owner, b.CanonicalPromptID, b.CompressedPromptID, b.WorkerPromptID, md, string(src),
		b.CompressionCalls, b.CompressionInputTokens, b.BuildDuration.Milliseconds(), now.UTC())
	if err != nil {
		return false, TranslateDBError("finalize cache entry", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinalizeFailed marks an owned BUILDING entry FAILED.
func (r *PromptCacheRepository) FinalizeFailed(ctx context.Context, id uuid.UUID, owner string, now time.Time, kind, message string, retryAfter *time.Time, md cache.KeySourceMetadata) (bool, error) {
	raw, err := json.Marshal(md)
	if err != nil {
		return false, &persistence.InfrastructureError{Op: "encode cache metadata", Cause: err}
	}
	var ra *time.Time
	if retryAfter != nil {
		v := retryAfter.UTC()
		ra = &v
	}
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET status = 'FAILED', error_kind = $3, error_message = $4, retry_after = $5, metadata = $6,
		   last_used_at = $7, lease_expires_at = NULL
		 WHERE id = $1 AND status = 'BUILDING' AND build_owner = $2`,
		id, owner, kind, message, ra, raw, now.UTC())
	if err != nil {
		return false, TranslateDBError("fail cache entry", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkInvalid invalidates a VALID entry.
func (r *PromptCacheRepository) MarkInvalid(ctx context.Context, id uuid.UUID, reason string) (bool, error) {
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET status = 'INVALIDATED', invalidation_reason = $2 WHERE id = $1 AND status = 'VALID'`, id, reason)
	if err != nil {
		return false, TranslateDBError("invalidate cache entry", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InvalidateProject invalidates every VALID entry of the project.
func (r *PromptCacheRepository) InvalidateProject(ctx context.Context, projectID uuid.UUID, reason string) (int, error) {
	tag, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET status = 'INVALIDATED', invalidation_reason = $2 WHERE project_id = $1 AND status = 'VALID'`, projectID, reason)
	if err != nil {
		return 0, TranslateDBError("invalidate project cache", err)
	}
	return int(tag.RowsAffected()), nil
}

// Touch records a hit.
func (r *PromptCacheRepository) Touch(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := r.exec.Exec(ctx,
		`UPDATE prompt_cache_entries SET hit_count = hit_count + 1, last_used_at = $2 WHERE id = $1`, id, now.UTC()); err != nil {
		return TranslateDBError("touch cache entry", err)
	}
	return nil
}

// List returns the project's entries matching the filter, newest first.
func (r *PromptCacheRepository) List(ctx context.Context, f persistence.PromptCacheFilter) ([]cache.PromptCacheEntry, error) {
	where := []string{"project_id = $1"}
	args := []any{f.ProjectID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, cond+" = $"+strconv.Itoa(len(args)))
	}
	if f.StateKey != nil {
		add("state_key", *f.StateKey)
	}
	if f.PhaseID != nil {
		add("phase_id", *f.PhaseID)
	}
	if f.WorkUnitID != nil {
		add("work_unit_id", *f.WorkUnitID)
	}
	if f.Status != nil {
		add("status", string(*f.Status))
	}
	if f.Source != nil {
		add("source", string(*f.Source))
	}
	if f.Layer != nil {
		add("layer", string(*f.Layer))
	}
	rows, err := r.exec.Query(ctx, `SELECT `+cacheEntryColumns+` FROM prompt_cache_entries WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, TranslateDBError("list cache entries", err)
	}
	defer rows.Close()
	out := []cache.PromptCacheEntry{}
	for rows.Next() {
		e, err := scanCacheEntry(rows)
		if err != nil {
			return nil, TranslateDBError("list cache entries", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list cache entries", err)
	}
	return out, nil
}

// LatestInSlot returns the newest non-BUILDING entry of the slot.
func (r *PromptCacheRepository) LatestInSlot(ctx context.Context, projectID uuid.UUID, stateKey string, workUnitID *uuid.UUID, layer cache.Layer) (*cache.PromptCacheEntry, error) {
	return r.one(ctx, "latest cache entry in slot",
		`project_id = $1 AND state_key = $2 AND work_unit_id IS NOT DISTINCT FROM $3 AND layer = $4 AND status <> 'BUILDING'
		 ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, stateKey, workUnitID, string(layer))
}

// WorkerPromptRevoked reports an INVALIDATED entry referencing the worker
// prompt.
func (r *PromptCacheRepository) WorkerPromptRevoked(ctx context.Context, projectID, workerPromptID uuid.UUID) (bool, error) {
	var n int
	if err := r.exec.QueryRow(ctx,
		`SELECT count(*) FROM prompt_cache_entries WHERE project_id = $1 AND worker_prompt_id = $2 AND status = 'INVALIDATED'`,
		projectID, workerPromptID).Scan(&n); err != nil {
		return false, TranslateDBError("revoked worker prompt", err)
	}
	return n > 0, nil
}

// DeleteEntries removes cache-entry rows of the project (never artifacts).
func (r *PromptCacheRepository) DeleteEntries(ctx context.Context, projectID uuid.UUID, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := r.exec.Exec(ctx, `DELETE FROM prompt_cache_entries WHERE project_id = $1 AND id = ANY($2) AND status <> 'BUILDING'`, projectID, ids)
	if err != nil {
		return 0, TranslateDBError("delete cache entries", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------

// PromptCacheEventRepository implements persistence.PromptCacheEventRepository.
type PromptCacheEventRepository struct{ exec executor }

var _ persistence.PromptCacheEventRepository = (*PromptCacheEventRepository)(nil)

// NewPromptCacheEventRepository binds the repository to a pool.
func NewPromptCacheEventRepository(pool *pgxpool.Pool) *PromptCacheEventRepository {
	return &PromptCacheEventRepository{exec: poolAdapter{pool}}
}

// Append stores one event.
func (r *PromptCacheEventRepository) Append(ctx context.Context, e cache.Event) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO prompt_cache_events (id, project_id, entry_id, cache_key, layer, event_type, state_key, work_unit_id, reason,
		   avoided_compression_calls, avoided_tokens, avoided_latency_ms, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		e.ID, e.ProjectID, e.EntryID, e.CacheKey, string(e.Layer), string(e.Type), e.StateKey, e.WorkUnitID, e.Reason,
		e.AvoidedCompressionCalls, e.AvoidedTokens, e.AvoidedLatency.Milliseconds(), e.CreatedAt.UTC()); err != nil {
		return TranslateDBError("append cache event", err)
	}
	return nil
}

// ListByProject returns events newest first.
func (r *PromptCacheEventRepository) ListByProject(ctx context.Context, projectID uuid.UUID, limit int) ([]cache.Event, error) {
	q := `SELECT id, project_id, entry_id, cache_key, layer, event_type, state_key, work_unit_id, reason,
		avoided_compression_calls, avoided_tokens, avoided_latency_ms, created_at
		FROM prompt_cache_events WHERE project_id = $1 ORDER BY created_at DESC, id DESC`
	args := []any{projectID}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, TranslateDBError("list cache events", err)
	}
	defer rows.Close()
	out := []cache.Event{}
	for rows.Next() {
		var e cache.Event
		var layer, typ string
		var ms int64
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.EntryID, &e.CacheKey, &layer, &typ, &e.StateKey, &e.WorkUnitID, &e.Reason,
			&e.AvoidedCompressionCalls, &e.AvoidedTokens, &ms, &e.CreatedAt); err != nil {
			return nil, TranslateDBError("list cache events", err)
		}
		e.Layer, e.Type, e.AvoidedLatency = cache.Layer(layer), cache.EventType(typ), time.Duration(ms)*time.Millisecond
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------

// PromptFragmentRepository implements persistence.PromptFragmentRepository.
type PromptFragmentRepository struct{ exec executor }

var _ persistence.PromptFragmentRepository = (*PromptFragmentRepository)(nil)

// NewPromptFragmentRepository binds the repository to a pool.
func NewPromptFragmentRepository(pool *pgxpool.Pool) *PromptFragmentRepository {
	return &PromptFragmentRepository{exec: poolAdapter{pool}}
}

const fragmentColumns = `id, project_id, fragment_key, kind, content, hash, estimated_tokens, sources, created_at`

func scanFragment(row interface{ Scan(dest ...any) error }) (*cache.PromptFragment, error) {
	var f cache.PromptFragment
	var kind string
	var src []byte
	if err := row.Scan(&f.ID, &f.ProjectID, &f.FragmentKey, &kind, &f.Content, &f.Hash, &f.EstimatedTokens, &src, &f.CreatedAt); err != nil {
		return nil, err
	}
	f.Kind = cache.FragmentKind(kind)
	_ = json.Unmarshal(src, &f.Sources)
	return &f, nil
}

// StoreIfAbsent stores the fragment unless its key exists.
func (r *PromptFragmentRepository) StoreIfAbsent(ctx context.Context, f *cache.PromptFragment) (*cache.PromptFragment, bool, error) {
	if err := cache.ValidateFragment(*f); err != nil {
		return nil, false, err
	}
	src, _ := json.Marshal(f.Sources)
	tag, err := r.exec.Exec(ctx,
		`INSERT INTO prompt_fragments (`+fragmentColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (project_id, fragment_key) DO NOTHING`,
		f.ID, f.ProjectID, f.FragmentKey, string(f.Kind), f.Content, f.Hash, f.EstimatedTokens, src, f.CreatedAt.UTC())
	if err != nil {
		return nil, false, TranslateDBError("store fragment", err)
	}
	if tag.RowsAffected() == 1 {
		cp := *f
		return &cp, true, nil
	}
	got, err := r.GetByKey(ctx, f.ProjectID, f.FragmentKey)
	return got, false, err
}

// GetByKey returns a fragment by key.
func (r *PromptFragmentRepository) GetByKey(ctx context.Context, projectID uuid.UUID, key string) (*cache.PromptFragment, error) {
	f, err := scanFragment(r.exec.QueryRow(ctx, `SELECT `+fragmentColumns+` FROM prompt_fragments WHERE project_id = $1 AND fragment_key = $2`, projectID, key))
	if err != nil {
		return nil, TranslateDBError("get fragment", err)
	}
	return f, nil
}

// ListByProject returns the project's fragments in creation order.
func (r *PromptFragmentRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]cache.PromptFragment, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+fragmentColumns+` FROM prompt_fragments WHERE project_id = $1 ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list fragments", err)
	}
	defer rows.Close()
	out := []cache.PromptFragment{}
	for rows.Next() {
		f, err := scanFragment(rows)
		if err != nil {
			return nil, TranslateDBError("list fragments", err)
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// IncrementHit records a fragment reuse.
func (r *PromptFragmentRepository) IncrementHit(ctx context.Context, id uuid.UUID) error {
	if _, err := r.exec.Exec(ctx, `UPDATE prompt_fragments SET hit_count = hit_count + 1 WHERE id = $1`, id); err != nil {
		return TranslateDBError("fragment hit", err)
	}
	return nil
}

// ---------------------------------------------------------------------------

// PromptCacheSettingsRepository implements
// persistence.PromptCacheSettingsRepository.
type PromptCacheSettingsRepository struct{ exec executor }

var _ persistence.PromptCacheSettingsRepository = (*PromptCacheSettingsRepository)(nil)

// NewPromptCacheSettingsRepository binds the repository to a pool.
func NewPromptCacheSettingsRepository(pool *pgxpool.Pool) *PromptCacheSettingsRepository {
	return &PromptCacheSettingsRepository{exec: poolAdapter{pool}}
}

// Get returns the stored settings, or the defaults with stored=false.
func (r *PromptCacheSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (cache.Settings, bool, error) {
	s := cache.DefaultSettings()
	var policy string
	var negMS, leaseMS int64
	err := r.exec.QueryRow(ctx,
		`SELECT enabled, retention_policy, retention_value, negative_window_ms, lease_ms, cache_same_session_continuation, bypass_on_cache_error
		 FROM project_prompt_cache_settings WHERE project_id = $1`, projectID).
		Scan(&s.Enabled, &policy, &s.Retention.Value, &negMS, &leaseMS, &s.CacheSameSessionContinuation, &s.BypassOnCacheError)
	if err != nil {
		if errors.Is(TranslateDBError("get cache settings", err), persistence.ErrNotFound) {
			return cache.DefaultSettings(), false, nil
		}
		return cache.Settings{}, false, TranslateDBError("get cache settings", err)
	}
	s.Retention.Policy = cache.RetentionPolicy(policy)
	s.NegativeWindow, s.Lease = time.Duration(negMS)*time.Millisecond, time.Duration(leaseMS)*time.Millisecond
	return s, true, nil
}

// Upsert stores the settings.
func (r *PromptCacheSettingsRepository) Upsert(ctx context.Context, projectID uuid.UUID, s cache.Settings) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO project_prompt_cache_settings (project_id, enabled, retention_policy, retention_value, negative_window_ms, lease_ms,
		   cache_same_session_continuation, bypass_on_cache_error, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		 ON CONFLICT (project_id) DO UPDATE SET enabled = EXCLUDED.enabled, retention_policy = EXCLUDED.retention_policy,
		   retention_value = EXCLUDED.retention_value, negative_window_ms = EXCLUDED.negative_window_ms, lease_ms = EXCLUDED.lease_ms,
		   cache_same_session_continuation = EXCLUDED.cache_same_session_continuation,
		   bypass_on_cache_error = EXCLUDED.bypass_on_cache_error, updated_at = now()`,
		projectID, s.Enabled, string(s.Retention.Policy), s.Retention.Value, s.NegativeWindow.Milliseconds(), s.Lease.Milliseconds(),
		s.CacheSameSessionContinuation, s.BypassOnCacheError); err != nil {
		return TranslateDBError("upsert cache settings", err)
	}
	return nil
}
