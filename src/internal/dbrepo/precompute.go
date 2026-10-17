package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/precompute"
)

// ---------------------------------------------------------------------------
// Settings.

// PrecomputeSettingsRepository implements persistence.PrecomputeSettingsRepository.
type PrecomputeSettingsRepository struct{ exec executor }

var _ persistence.PrecomputeSettingsRepository = (*PrecomputeSettingsRepository)(nil)

// NewPrecomputeSettingsRepository binds the repository to a pool.
func NewPrecomputeSettingsRepository(pool *pgxpool.Pool) *PrecomputeSettingsRepository {
	return &PrecomputeSettingsRepository{exec: poolAdapter{pool}}
}

// Get returns the stored settings or the defaults.
func (r *PrecomputeSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (precompute.Settings, bool, error) {
	var s precompute.Settings
	var policy string
	var durMS int64
	err := r.exec.QueryRow(ctx, `SELECT enabled, depth, max_candidates, concurrency, compression_enabled, static_fragments,
		during_worker_execution, same_session_compression, plan_next_phase, max_tokens, max_duration_ms, resource_policy, worker_is_local
		FROM project_precompute_settings WHERE project_id = $1`, projectID).
		Scan(&s.Enabled, &s.Depth, &s.MaxCandidates, &s.Concurrency, &s.CompressionEnabled, &s.StaticFragments,
			&s.DuringWorkerExecution, &s.SameSessionCompression, &s.PlanNextPhase, &s.MaxTokens, &durMS, &policy, &s.WorkerIsLocal)
	if err != nil {
		if errors.Is(TranslateDBError("get precompute settings", err), persistence.ErrNotFound) {
			return precompute.DefaultSettings(), false, nil
		}
		return precompute.Settings{}, false, TranslateDBError("get precompute settings", err)
	}
	s.MaxDuration, s.ResourcePolicy = time.Duration(durMS)*time.Millisecond, precompute.ResourcePolicy(policy)
	return s, true, nil
}

// Upsert stores the settings.
func (r *PrecomputeSettingsRepository) Upsert(ctx context.Context, projectID uuid.UUID, s precompute.Settings) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO project_precompute_settings (project_id, enabled, depth, max_candidates, concurrency,
		compression_enabled, static_fragments, during_worker_execution, same_session_compression, plan_next_phase, max_tokens,
		max_duration_ms, resource_policy, worker_is_local, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		ON CONFLICT (project_id) DO UPDATE SET enabled = EXCLUDED.enabled, depth = EXCLUDED.depth, max_candidates = EXCLUDED.max_candidates,
		  concurrency = EXCLUDED.concurrency, compression_enabled = EXCLUDED.compression_enabled, static_fragments = EXCLUDED.static_fragments,
		  during_worker_execution = EXCLUDED.during_worker_execution, same_session_compression = EXCLUDED.same_session_compression,
		  plan_next_phase = EXCLUDED.plan_next_phase, max_tokens = EXCLUDED.max_tokens, max_duration_ms = EXCLUDED.max_duration_ms,
		  resource_policy = EXCLUDED.resource_policy, worker_is_local = EXCLUDED.worker_is_local, updated_at = now()`,
		projectID, s.Enabled, s.Depth, s.MaxCandidates, s.Concurrency, s.CompressionEnabled, s.StaticFragments, s.DuringWorkerExecution,
		s.SameSessionCompression, s.PlanNextPhase, s.MaxTokens, s.MaxDuration.Milliseconds(), string(s.ResourcePolicy), s.WorkerIsLocal); err != nil {
		return TranslateDBError("upsert precompute settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Projections.

// ProjectionRepository implements persistence.ProjectionRepository.
type ProjectionRepository struct{ exec executor }

var _ persistence.ProjectionRepository = (*ProjectionRepository)(nil)

// NewProjectionRepository binds the repository to a pool.
func NewProjectionRepository(pool *pgxpool.Pool) *ProjectionRepository {
	return &ProjectionRepository{exec: poolAdapter{pool}}
}

// Create stores a projection.
func (r *ProjectionRepository) Create(ctx context.Context, p *precompute.ProjectedContext) error {
	path, _ := json.Marshal(nonNilStrings(p.TransitionPath))
	pins, _ := json.Marshal(p.Pins)
	if p.Pins == nil {
		pins = []byte("[]")
	}
	if _, err := r.exec.Exec(ctx, `INSERT INTO precompute_projections (id, project_id, base_snapshot_id, base_semantic_hash, phase_id,
		projected_work_unit_id, projected_state_key, projected_role_key, transition_path, pins, projection_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		p.ID, p.ProjectID, p.BaseSnapshotID, p.BaseSemanticHash, p.PhaseID, p.ProjectedWorkUnitID, p.ProjectedStateKey,
		p.ProjectedRoleKey, path, pins, p.ProjectionHash, p.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create projection", err)
	}
	return nil
}

// Get returns a projection.
func (r *ProjectionRepository) Get(ctx context.Context, id uuid.UUID) (*precompute.ProjectedContext, error) {
	var p precompute.ProjectedContext
	var path, pins []byte
	err := r.exec.QueryRow(ctx, `SELECT id, project_id, base_snapshot_id, base_semantic_hash, phase_id, projected_work_unit_id,
		projected_state_key, projected_role_key, transition_path, pins, projection_hash, created_at FROM precompute_projections WHERE id = $1`, id).
		Scan(&p.ID, &p.ProjectID, &p.BaseSnapshotID, &p.BaseSemanticHash, &p.PhaseID, &p.ProjectedWorkUnitID, &p.ProjectedStateKey,
			&p.ProjectedRoleKey, &path, &pins, &p.ProjectionHash, &p.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("get projection", err)
	}
	_ = json.Unmarshal(path, &p.TransitionPath)
	_ = json.Unmarshal(pins, &p.Pins)
	return &p, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// Jobs + candidates.

// PrecomputeJobRepository implements persistence.PrecomputeJobRepository.
type PrecomputeJobRepository struct{ exec executor }

var _ persistence.PrecomputeJobRepository = (*PrecomputeJobRepository)(nil)

// NewPrecomputeJobRepository binds the repository to a pool.
func NewPrecomputeJobRepository(pool *pgxpool.Pool) *PrecomputeJobRepository {
	return &PrecomputeJobRepository{exec: poolAdapter{pool}}
}

const jobColumns = `id, project_id, trigger, status, owner, base_snapshot_id, state_key, work_unit_id, depth, max_candidates,
	candidate_count, hit_count, built_count, fragment_count, template_count, skipped_count, failed_count, reason,
	created_at, started_at, completed_at, heartbeat_at`

func scanJob(row interface{ Scan(dest ...any) error }) (*precompute.Job, error) {
	var j precompute.Job
	var trig, status string
	if err := row.Scan(&j.ID, &j.ProjectID, &trig, &status, &j.Owner, &j.BaseSnapshotID, &j.StateKey, &j.WorkUnitID, &j.Depth,
		&j.MaxCandidates, &j.CandidateCount, &j.HitCount, &j.BuiltCount, &j.FragmentCount, &j.TemplateCount, &j.SkippedCount,
		&j.FailedCount, &j.Reason, &j.CreatedAt, &j.StartedAt, &j.CompletedAt, &j.HeartbeatAt); err != nil {
		return nil, err
	}
	j.Trigger, j.Status = precompute.Trigger(trig), precompute.JobStatus(status)
	return &j, nil
}

// CreateJob inserts a job.
func (r *PrecomputeJobRepository) CreateJob(ctx context.Context, j *precompute.Job) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO precompute_jobs (`+jobColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		$11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		j.ID, j.ProjectID, string(j.Trigger), string(j.Status), j.Owner, j.BaseSnapshotID, j.StateKey, j.WorkUnitID, j.Depth,
		j.MaxCandidates, j.CandidateCount, j.HitCount, j.BuiltCount, j.FragmentCount, j.TemplateCount, j.SkippedCount, j.FailedCount,
		j.Reason, j.CreatedAt.UTC(), j.StartedAt, j.CompletedAt, j.HeartbeatAt.UTC()); err != nil {
		return TranslateDBError("create precompute job", err)
	}
	return nil
}

// StartJob moves PENDING → RUNNING.
func (r *PrecomputeJobRepository) StartJob(ctx context.Context, id uuid.UUID, owner string, at time.Time) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE precompute_jobs SET status = 'RUNNING', owner = $2, started_at = $3, heartbeat_at = $3
		WHERE id = $1 AND status = 'PENDING'`, id, owner, at.UTC())
	if err != nil {
		return false, TranslateDBError("start precompute job", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinishJob sets a terminal status with counts.
func (r *PrecomputeJobRepository) FinishJob(ctx context.Context, j *precompute.Job) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE precompute_jobs SET status = $2, candidate_count = $3, hit_count = $4, built_count = $5,
		fragment_count = $6, template_count = $7, skipped_count = $8, failed_count = $9, reason = $10, completed_at = $11,
		base_snapshot_id = COALESCE($12, base_snapshot_id), state_key = CASE WHEN $13 = '' THEN state_key ELSE $13 END,
		work_unit_id = COALESCE($14, work_unit_id), heartbeat_at = $11
		WHERE id = $1 AND status IN ('PENDING', 'RUNNING')`,
		j.ID, string(j.Status), j.CandidateCount, j.HitCount, j.BuiltCount, j.FragmentCount, j.TemplateCount, j.SkippedCount,
		j.FailedCount, j.Reason, j.CompletedAt, j.BaseSnapshotID, j.StateKey, j.WorkUnitID)
	if err != nil {
		return false, TranslateDBError("finish precompute job", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Heartbeat refreshes a running job's heartbeat.
func (r *PrecomputeJobRepository) Heartbeat(ctx context.Context, id uuid.UUID, at time.Time) error {
	if _, err := r.exec.Exec(ctx, `UPDATE precompute_jobs SET heartbeat_at = $2 WHERE id = $1 AND status = 'RUNNING'`, id, at.UTC()); err != nil {
		return TranslateDBError("heartbeat precompute job", err)
	}
	return nil
}

// GetJob returns a job.
func (r *PrecomputeJobRepository) GetJob(ctx context.Context, id uuid.UUID) (*precompute.Job, error) {
	j, err := scanJob(r.exec.QueryRow(ctx, `SELECT `+jobColumns+` FROM precompute_jobs WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get precompute job", err)
	}
	return j, nil
}

func (r *PrecomputeJobRepository) listJobs(ctx context.Context, op, q string, args ...any) ([]precompute.Job, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+jobColumns+` FROM precompute_jobs WHERE `+q, args...)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	out := []precompute.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, TranslateDBError(op, err)
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ListJobs pages a project's jobs (newest first) with the total count.
func (r *PrecomputeJobRepository) ListJobs(ctx context.Context, f persistence.JobFilter) ([]precompute.Job, int, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM precompute_jobs WHERE project_id = $1`, f.ProjectID).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count precompute jobs", err)
	}
	jobs, err := r.listJobs(ctx, "list precompute jobs", `project_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, f.ProjectID, limit, f.Offset)
	return jobs, total, err
}

// ListStale returns non-terminal jobs with a stale heartbeat owned by
// another instance.
func (r *PrecomputeJobRepository) ListStale(ctx context.Context, before time.Time, owner string) ([]precompute.Job, error) {
	return r.listJobs(ctx, "list stale precompute jobs", `status IN ('PENDING', 'RUNNING') AND heartbeat_at < $1 AND owner <> $2 ORDER BY created_at`, before.UTC(), owner)
}

// ListActive returns a project's non-terminal jobs.
func (r *PrecomputeJobRepository) ListActive(ctx context.Context, projectID uuid.UUID) ([]precompute.Job, error) {
	return r.listJobs(ctx, "list active precompute jobs", `project_id = $1 AND status IN ('PENDING', 'RUNNING') ORDER BY created_at`, projectID)
}

const candColumns = `id, job_id, project_id, state_key, role_key, phase_id, work_unit_id, paths, depth, next_unit, readiness, priority,
	missing, result, reason, cache_key, cache_entry_id, fragment_ids, template_id, projection_id, created_at`

// UpsertCandidate inserts or updates a candidate record.
func (r *PrecomputeJobRepository) UpsertCandidate(ctx context.Context, c *precompute.CandidateRecord) error {
	paths, _ := json.Marshal(c.Paths)
	if c.Paths == nil {
		paths = []byte("[]")
	}
	missing, _ := json.Marshal(c.Missing)
	if c.Missing == nil {
		missing = []byte("[]")
	}
	frags, _ := json.Marshal(c.FragmentIDs)
	if c.FragmentIDs == nil {
		frags = []byte("[]")
	}
	if _, err := r.exec.Exec(ctx, `INSERT INTO precompute_candidates (`+candColumns+`, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $21)
		ON CONFLICT (id) DO UPDATE SET result = EXCLUDED.result, reason = EXCLUDED.reason, cache_key = EXCLUDED.cache_key,
		  cache_entry_id = EXCLUDED.cache_entry_id, fragment_ids = EXCLUDED.fragment_ids, template_id = EXCLUDED.template_id,
		  projection_id = EXCLUDED.projection_id, readiness = EXCLUDED.readiness, missing = EXCLUDED.missing, updated_at = now()`,
		c.ID, c.JobID, c.ProjectID, c.StateKey, c.RoleKey, c.PhaseID, c.WorkUnitID, paths, c.Depth, c.NextUnit, string(c.Readiness),
		c.Priority, missing, string(c.Result), c.Reason, c.CacheKey, c.CacheEntryID, frags, c.TemplateID, c.ProjectionID, c.CreatedAt.UTC()); err != nil {
		return TranslateDBError("upsert precompute candidate", err)
	}
	return nil
}

func (r *PrecomputeJobRepository) listCands(ctx context.Context, where string, arg any) ([]precompute.CandidateRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+candColumns+` FROM precompute_candidates WHERE `+where, arg)
	if err != nil {
		return nil, TranslateDBError("list precompute candidates", err)
	}
	defer rows.Close()
	out := []precompute.CandidateRecord{}
	for rows.Next() {
		var c precompute.CandidateRecord
		var paths, missing, frags []byte
		var readiness, result string
		if err := rows.Scan(&c.ID, &c.JobID, &c.ProjectID, &c.StateKey, &c.RoleKey, &c.PhaseID, &c.WorkUnitID, &paths, &c.Depth, &c.NextUnit,
			&readiness, &c.Priority, &missing, &result, &c.Reason, &c.CacheKey, &c.CacheEntryID, &frags, &c.TemplateID, &c.ProjectionID, &c.CreatedAt); err != nil {
			return nil, TranslateDBError("scan precompute candidate", err)
		}
		c.Readiness, c.Result = precompute.Readiness(readiness), precompute.CandidateResult(result)
		_ = json.Unmarshal(paths, &c.Paths)
		_ = json.Unmarshal(missing, &c.Missing)
		_ = json.Unmarshal(frags, &c.FragmentIDs)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCandidates returns a job's candidates in priority order.
func (r *PrecomputeJobRepository) ListCandidates(ctx context.Context, jobID uuid.UUID) ([]precompute.CandidateRecord, error) {
	return r.listCands(ctx, `job_id = $1 ORDER BY priority ASC, depth ASC, state_key ASC, created_at ASC`, jobID)
}

// ListProjectCandidates returns every candidate of the project.
func (r *PrecomputeJobRepository) ListProjectCandidates(ctx context.Context, projectID uuid.UUID) ([]precompute.CandidateRecord, error) {
	return r.listCands(ctx, `project_id = $1 ORDER BY created_at ASC`, projectID)
}

// FailPendingCandidates marks a job's PENDING candidates FAILED.
func (r *PrecomputeJobRepository) FailPendingCandidates(ctx context.Context, jobID uuid.UUID, reason string, at time.Time) (int, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE precompute_candidates SET result = 'FAILED', reason = $2, updated_at = $3 WHERE job_id = $1 AND result = 'PENDING'`, jobID, reason, at.UTC())
	if err != nil {
		return 0, TranslateDBError("fail pending candidates", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Fragment manifests.

// FragmentManifestRepository implements persistence.FragmentManifestRepository.
type FragmentManifestRepository struct{ exec executor }

var _ persistence.FragmentManifestRepository = (*FragmentManifestRepository)(nil)

// NewFragmentManifestRepository binds the repository to a pool.
func NewFragmentManifestRepository(pool *pgxpool.Pool) *FragmentManifestRepository {
	return &FragmentManifestRepository{exec: poolAdapter{pool}}
}

// CreateIfAbsent stores the manifest unless it exists.
func (r *FragmentManifestRepository) CreateIfAbsent(ctx context.Context, m *precompute.FragmentManifest) (*precompute.FragmentManifest, bool, error) {
	req, _ := json.Marshal(m.RequiredDynamicInputs)
	tag, err := r.exec.Exec(ctx, `INSERT INTO fragment_manifests (id, project_id, fragment_id, fragment_key, required_inputs, source_snapshot_hash, projection_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (fragment_id, projection_hash) DO NOTHING`,
		m.ID, m.ProjectID, m.FragmentID, m.FragmentKey, req, m.SourceSnapshotHash, m.ProjectionHash, m.CreatedAt.UTC())
	if err != nil {
		return nil, false, TranslateDBError("create fragment manifest", err)
	}
	cp := *m
	return &cp, tag.RowsAffected() == 1, nil
}

// ListByProject returns the project's manifests.
func (r *FragmentManifestRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]precompute.FragmentManifest, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, project_id, fragment_id, fragment_key, required_inputs, source_snapshot_hash, projection_hash, created_at
		FROM fragment_manifests WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, TranslateDBError("list fragment manifests", err)
	}
	defer rows.Close()
	out := []precompute.FragmentManifest{}
	for rows.Next() {
		var m precompute.FragmentManifest
		var req []byte
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.FragmentID, &m.FragmentKey, &req, &m.SourceSnapshotHash, &m.ProjectionHash, &m.CreatedAt); err != nil {
			return nil, TranslateDBError("scan fragment manifest", err)
		}
		_ = json.Unmarshal(req, &m.RequiredDynamicInputs)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Transition latencies.

// TransitionLatencyRepository implements persistence.TransitionLatencyRepository.
type TransitionLatencyRepository struct{ exec executor }

var _ persistence.TransitionLatencyRepository = (*TransitionLatencyRepository)(nil)

// NewTransitionLatencyRepository binds the repository to a pool.
func NewTransitionLatencyRepository(pool *pgxpool.Pool) *TransitionLatencyRepository {
	return &TransitionLatencyRepository{exec: poolAdapter{pool}}
}

// Create stores a measured latency.
func (r *TransitionLatencyRepository) Create(ctx context.Context, l *persistence.TransitionLatency) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO transition_latencies (id, project_id, state_key, work_unit_id, worker_prompt_id, cache_entry_id,
		source, result_received_at, prompt_ready_at, duration_ms, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		l.ID, l.ProjectID, l.StateKey, l.WorkUnitID, l.WorkerPromptID, l.CacheEntryID, l.Source, l.ResultReceivedAt.UTC(),
		l.PromptReadyAt.UTC(), l.Duration.Milliseconds(), l.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create transition latency", err)
	}
	return nil
}

// ListByProject returns the project's latencies in order.
func (r *TransitionLatencyRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]persistence.TransitionLatency, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, project_id, state_key, work_unit_id, worker_prompt_id, cache_entry_id, source,
		result_received_at, prompt_ready_at, duration_ms, created_at FROM transition_latencies WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, TranslateDBError("list transition latencies", err)
	}
	defer rows.Close()
	out := []persistence.TransitionLatency{}
	for rows.Next() {
		var l persistence.TransitionLatency
		var ms int64
		if err := rows.Scan(&l.ID, &l.ProjectID, &l.StateKey, &l.WorkUnitID, &l.WorkerPromptID, &l.CacheEntryID, &l.Source,
			&l.ResultReceivedAt, &l.PromptReadyAt, &ms, &l.CreatedAt); err != nil {
			return nil, TranslateDBError("scan transition latency", err)
		}
		l.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Placeholder templates (Decision 5).

// PrecomputedTemplateRepository implements persistence.PrecomputedTemplateRepository.
type PrecomputedTemplateRepository struct{ exec executor }

var _ persistence.PrecomputedTemplateRepository = (*PrecomputedTemplateRepository)(nil)

// NewPrecomputedTemplateRepository binds the repository to a pool.
func NewPrecomputedTemplateRepository(pool *pgxpool.Pool) *PrecomputedTemplateRepository {
	return &PrecomputedTemplateRepository{exec: poolAdapter{pool}}
}

const tmplColumns = `id, project_id, projection_id, work_unit_id, state_key, normalized_key, placeholder_token, canonical_prompt_id,
	compressed_prompt_id, content_source, status, resolved_entry_id, reason, created_at, resolved_at`

func scanTemplate(row interface{ Scan(dest ...any) error }) (*persistence.PrecomputedTemplate, error) {
	var t persistence.PrecomputedTemplate
	if err := row.Scan(&t.ID, &t.ProjectID, &t.ProjectionID, &t.WorkUnitID, &t.StateKey, &t.NormalizedKey, &t.PlaceholderToken,
		&t.CanonicalPromptID, &t.CompressedPromptID, &t.ContentSource, &t.Status, &t.ResolvedEntryID, &t.Reason, &t.CreatedAt, &t.ResolvedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// Create stores a PENDING template (a concurrent PENDING one wins).
func (r *PrecomputedTemplateRepository) Create(ctx context.Context, t *persistence.PrecomputedTemplate) (*persistence.PrecomputedTemplate, bool, error) {
	tag, err := r.exec.Exec(ctx, `INSERT INTO precomputed_templates (`+tmplColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (project_id, normalized_key) WHERE status = 'PENDING' DO NOTHING`,
		t.ID, t.ProjectID, t.ProjectionID, t.WorkUnitID, t.StateKey, t.NormalizedKey, t.PlaceholderToken, t.CanonicalPromptID,
		t.CompressedPromptID, t.ContentSource, t.Status, t.ResolvedEntryID, t.Reason, t.CreatedAt.UTC(), t.ResolvedAt)
	if err != nil {
		return nil, false, TranslateDBError("create precomputed template", err)
	}
	if tag.RowsAffected() == 1 {
		cp := *t
		return &cp, true, nil
	}
	w, err := r.GetPending(ctx, t.ProjectID, t.NormalizedKey)
	return w, false, err
}

// GetPending returns the PENDING template for a normalized key.
func (r *PrecomputedTemplateRepository) GetPending(ctx context.Context, projectID uuid.UUID, key string) (*persistence.PrecomputedTemplate, error) {
	t, err := scanTemplate(r.exec.QueryRow(ctx, `SELECT `+tmplColumns+` FROM precomputed_templates WHERE project_id = $1 AND normalized_key = $2 AND status = 'PENDING'`, projectID, key))
	if err != nil {
		return nil, TranslateDBError("get pending template", err)
	}
	return t, nil
}

// Get returns a template.
func (r *PrecomputedTemplateRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.PrecomputedTemplate, error) {
	t, err := scanTemplate(r.exec.QueryRow(ctx, `SELECT `+tmplColumns+` FROM precomputed_templates WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get template", err)
	}
	return t, nil
}

// Finish moves a PENDING template to a final status.
func (r *PrecomputedTemplateRepository) Finish(ctx context.Context, id uuid.UUID, status, reason string, entryID *uuid.UUID, at time.Time) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE precomputed_templates SET status = $2, reason = $3, resolved_entry_id = $4, resolved_at = $5
		WHERE id = $1 AND status = 'PENDING'`, id, status, reason, entryID, at.UTC())
	if err != nil {
		return false, TranslateDBError("finish template", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListByProject returns the project's templates in creation order.
func (r *PrecomputedTemplateRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]persistence.PrecomputedTemplate, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+tmplColumns+` FROM precomputed_templates WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, TranslateDBError("list templates", err)
	}
	defer rows.Close()
	out := []persistence.PrecomputedTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, TranslateDBError("scan template", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
