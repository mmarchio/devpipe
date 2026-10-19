package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// ---------------------------------------------------------------------------
// Sessions (P12-51, WBS 12.18).

// WorkerSessionRepository implements persistence.WorkerSessionRepository.
type WorkerSessionRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.WorkerSessionRepository = (*WorkerSessionRepository)(nil)

// NewWorkerSessionRepository binds the repository to a pool.
func NewWorkerSessionRepository(pool *pgxpool.Pool) *WorkerSessionRepository {
	return &WorkerSessionRepository{exec: poolAdapter{pool}, pool: pool}
}

type usageJSON struct {
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
	ContextTokens *int64 `json:"context_tokens,omitempty"`
	ContextWindow *int64 `json:"context_window,omitempty"`
	Estimated     bool   `json:"estimated,omitempty"`
}

func encodeUsage(u worker.WorkerUsage) []byte {
	b, _ := json.Marshal(usageJSON{u.InputTokens, u.OutputTokens, u.ContextTokens, u.ContextWindow, u.Estimated})
	return b
}

func decodeUsage(b []byte) worker.WorkerUsage {
	var u usageJSON
	_ = json.Unmarshal(b, &u)
	return worker.WorkerUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ContextTokens: u.ContextTokens, ContextWindow: u.ContextWindow, Estimated: u.Estimated}
}

const sessionCols = `id, project_id, provider, provider_session_id, status, session_policy, working_directory, usage, status_reason,
	created_at, last_seen_at, closed_at`

func scanSession(row interface{ Scan(dest ...any) error }) (*persistence.WorkerSessionRecord, error) {
	var s persistence.WorkerSessionRecord
	var provider, status, policy string
	var usage []byte
	if err := row.Scan(&s.ID, &s.ProjectID, &provider, &s.ProviderSessionID, &status, &policy, &s.WorkingDirectory, &usage, &s.StatusReason,
		&s.CreatedAt, &s.LastSeenAt, &s.ClosedAt); err != nil {
		return nil, err
	}
	s.Provider, s.Status, s.Policy, s.Usage = worker.WorkerProvider(provider), worker.SessionStatus(status), worker.SessionPolicy(policy), decodeUsage(usage)
	return &s, nil
}

func insertSession(ctx context.Context, ex executor, s *persistence.WorkerSessionRecord) error {
	_, err := ex.Exec(ctx, `INSERT INTO worker_sessions (`+sessionCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		s.ID, s.ProjectID, string(s.Provider), s.ProviderSessionID, string(s.Status), string(s.Policy), s.WorkingDirectory, encodeUsage(s.Usage),
		s.StatusReason, s.CreatedAt.UTC(), s.LastSeenAt.UTC(), s.ClosedAt)
	return err
}

// Create stores a session.
func (r *WorkerSessionRepository) Create(ctx context.Context, s *persistence.WorkerSessionRecord) error {
	if err := insertSession(ctx, r.exec, s); err != nil {
		return TranslateDBError("create worker session", err)
	}
	return nil
}

// Get returns a project's session.
func (r *WorkerSessionRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	s, err := scanSession(r.exec.QueryRow(ctx, `SELECT `+sessionCols+` FROM worker_sessions WHERE id = $1 AND project_id = $2`, id, projectID))
	if err != nil {
		return nil, TranslateDBError("get worker session", err)
	}
	return s, nil
}

// GetActiveForProject returns the newest ACTIVE session.
func (r *WorkerSessionRepository) GetActiveForProject(ctx context.Context, projectID uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	s, err := scanSession(r.exec.QueryRow(ctx, `SELECT `+sessionCols+` FROM worker_sessions WHERE project_id = $1 AND status = 'ACTIVE'
		ORDER BY created_at DESC, id LIMIT 1`, projectID))
	if err != nil {
		return nil, TranslateDBError("get active worker session", err)
	}
	return s, nil
}

func (r *WorkerSessionRepository) list(ctx context.Context, op, q string, args ...any) ([]persistence.WorkerSessionRecord, error) {
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	var out []persistence.WorkerSessionRecord
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, TranslateDBError(op, err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListForProject returns the session history, oldest first.
func (r *WorkerSessionRepository) ListForProject(ctx context.Context, projectID uuid.UUID) ([]persistence.WorkerSessionRecord, error) {
	return r.list(ctx, "list worker sessions", `SELECT `+sessionCols+` FROM worker_sessions WHERE project_id = $1 ORDER BY created_at, id`, projectID)
}

// ListActive returns every ACTIVE session (restart reconciliation).
func (r *WorkerSessionRepository) ListActive(ctx context.Context) ([]persistence.WorkerSessionRecord, error) {
	return r.list(ctx, "list active worker sessions", `SELECT `+sessionCols+` FROM worker_sessions WHERE status = 'ACTIVE' ORDER BY created_at, id`)
}

func (r *WorkerSessionRepository) update(ctx context.Context, op, q string, args ...any) error {
	tag, err := r.exec.Exec(ctx, q, args...)
	if err != nil {
		return TranslateDBError(op, err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// UpdateLastSeen records a confirmation of the session.
func (r *WorkerSessionRepository) UpdateLastSeen(ctx context.Context, projectID, id uuid.UUID, at time.Time) error {
	return r.update(ctx, "touch worker session", `UPDATE worker_sessions SET last_seen_at = $3 WHERE id = $1 AND project_id = $2`, id, projectID, at.UTC())
}

// UpdateWorkingDirectory records the confirmed directory (ACTIVE only).
func (r *WorkerSessionRepository) UpdateWorkingDirectory(ctx context.Context, projectID, id uuid.UUID, dir *string, at time.Time) error {
	return r.update(ctx, "update worker session cwd", `UPDATE worker_sessions SET working_directory = $3, last_seen_at = $4
		WHERE id = $1 AND project_id = $2 AND status = 'ACTIVE'`, id, projectID, dir, at.UTC())
}

// UpdateUsage records the session's latest usage.
func (r *WorkerSessionRepository) UpdateUsage(ctx context.Context, projectID, id uuid.UUID, u worker.WorkerUsage) error {
	return r.update(ctx, "update worker session usage", `UPDATE worker_sessions SET usage = $3 WHERE id = $1 AND project_id = $2`, id, projectID, encodeUsage(u))
}

func markSession(ctx context.Context, ex executor, projectID, id uuid.UUID, status worker.SessionStatus, reason string, at time.Time) (bool, error) {
	tag, err := ex.Exec(ctx, `UPDATE worker_sessions SET status = $3, status_reason = $4, closed_at = $5
		WHERE id = $1 AND project_id = $2 AND status = 'ACTIVE'`, id, projectID, string(status), reason, at.UTC())
	if err != nil {
		return false, TranslateDBError("mark worker session", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkReset marks an ACTIVE session RESET.
func (r *WorkerSessionRepository) MarkReset(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error) {
	return markSession(ctx, r.exec, projectID, id, worker.SessionReset, reason, at)
}

// MarkLost marks an ACTIVE session LOST.
func (r *WorkerSessionRepository) MarkLost(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error) {
	return markSession(ctx, r.exec, projectID, id, worker.SessionLost, reason, at)
}

// MarkClosed marks an ACTIVE session CLOSED.
func (r *WorkerSessionRepository) MarkClosed(ctx context.Context, projectID, id uuid.UUID, reason string, at time.Time) (bool, error) {
	return markSession(ctx, r.exec, projectID, id, worker.SessionClosed, reason, at)
}

// ReplaceActive marks prev RESET and inserts next ACTIVE in one
// transaction (no moment with two authoritative ACTIVE sessions).
func (r *WorkerSessionRepository) ReplaceActive(ctx context.Context, next *persistence.WorkerSessionRecord, prev *uuid.UUID, at time.Time) error {
	return Run(ctx, r.pool, func(tx persistence.Transaction) error {
		if prev != nil {
			if _, err := markSession(ctx, tx, next.ProjectID, *prev, worker.SessionReset, "replaced by new session", at); err != nil {
				return err
			}
		}
		if err := insertSession(ctx, tx, next); err != nil {
			return TranslateDBError("insert replacement worker session", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Settings (P12-52).

// WorkerSettingsRepository implements persistence.WorkerSettingsRepository.
type WorkerSettingsRepository struct{ exec executor }

var _ persistence.WorkerSettingsRepository = (*WorkerSettingsRepository)(nil)

// NewWorkerSettingsRepository binds the repository to a pool.
func NewWorkerSettingsRepository(pool *pgxpool.Pool) *WorkerSettingsRepository {
	return &WorkerSettingsRepository{exec: poolAdapter{pool}}
}

// Get returns stored settings or the defaults (stored=false).
func (r *WorkerSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (worker.Settings, bool, error) {
	var policy, send, format string
	var lease, wait int64
	err := r.exec.QueryRow(ctx, `SELECT session_policy, concurrent_send, lease_ms, wait_ms, response_format FROM project_worker_settings WHERE project_id = $1`, projectID).
		Scan(&policy, &send, &lease, &wait, &format)
	if err != nil {
		if errors.Is(TranslateDBError("get worker settings", err), persistence.ErrNotFound) {
			return worker.DefaultSettings(), false, nil
		}
		return worker.Settings{}, false, TranslateDBError("get worker settings", err)
	}
	return worker.Settings{SessionPolicy: worker.SessionPolicy(policy), ConcurrentSend: worker.ConcurrentSendPolicy(send),
		Lease: time.Duration(lease) * time.Millisecond, Wait: time.Duration(wait) * time.Millisecond,
		ResponseFormat: worker.ResponseFormat(format)}, true, nil
}

// Upsert stores settings.
func (r *WorkerSettingsRepository) Upsert(ctx context.Context, projectID uuid.UUID, s worker.Settings) error {
	_, err := r.exec.Exec(ctx, `INSERT INTO project_worker_settings (project_id, session_policy, concurrent_send, lease_ms, wait_ms, response_format, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (project_id) DO UPDATE SET session_policy = EXCLUDED.session_policy, concurrent_send = EXCLUDED.concurrent_send,
		  lease_ms = EXCLUDED.lease_ms, wait_ms = EXCLUDED.wait_ms, response_format = EXCLUDED.response_format, updated_at = now()`,
		projectID, string(s.SessionPolicy), string(s.ConcurrentSend), s.Lease.Milliseconds(), s.Wait.Milliseconds(), string(s.ResponseFormat.OrDefault()))
	if err != nil {
		return TranslateDBError("upsert worker settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Dispatches (P12-53).

// WorkerDispatchRepository implements persistence.WorkerDispatchRepository.
type WorkerDispatchRepository struct{ exec executor }

var _ persistence.WorkerDispatchRepository = (*WorkerDispatchRepository)(nil)

// NewWorkerDispatchRepository binds the repository to a pool.
func NewWorkerDispatchRepository(pool *pgxpool.Pool) *WorkerDispatchRepository {
	return &WorkerDispatchRepository{exec: poolAdapter{pool}}
}

const dispatchCols = `id, project_id, session_id, phase_id, work_unit_id, worker_prompt_id, prompt_hash, runtime_state_id, state_key, role_key,
	request_id, operation_id, attempt, status, delivery, retry_class, error_code, error_message, provider_code, retry_after_ms, diagnostic,
	owner, created_at, started_at, completed_at, worker_model, continuation_id`

func scanDispatch(row interface{ Scan(dest ...any) error }) (*worker.WorkerDispatch, error) {
	var d worker.WorkerDispatch
	var status string
	var delivery, retry *string
	var retryAfter *int64
	if err := row.Scan(&d.ID, &d.ProjectID, &d.SessionID, &d.PhaseID, &d.WorkUnitID, &d.WorkerPromptID, &d.PromptHash, &d.RuntimeStateID,
		&d.StateKey, &d.RoleKey, &d.RequestID, &d.OperationID, &d.Attempt, &status, &delivery, &retry, &d.ErrorCode, &d.ErrorMessage,
		&d.ProviderCode, &retryAfter, &d.Diagnostic, &d.Owner, &d.CreatedAt, &d.StartedAt, &d.CompletedAt, &d.WorkerModel, &d.ContinuationID); err != nil {
		return nil, err
	}
	d.Status = worker.DispatchStatus(status)
	if delivery != nil {
		v := worker.DeliveryState(*delivery)
		d.Delivery = &v
	}
	if retry != nil {
		v := worker.RetryClass(*retry)
		d.RetryClass = &v
	}
	if retryAfter != nil {
		v := time.Duration(*retryAfter) * time.Millisecond
		d.RetryAfter = &v
	}
	return &d, nil
}

// Create stores a PENDING dispatch.
func (r *WorkerDispatchRepository) Create(ctx context.Context, d *worker.WorkerDispatch) error {
	_, err := r.exec.Exec(ctx, `INSERT INTO worker_dispatches (id, project_id, session_id, phase_id, work_unit_id, worker_prompt_id, prompt_hash,
		runtime_state_id, state_key, role_key, request_id, operation_id, attempt, status, owner, created_at, updated_at, worker_model, continuation_id,
		correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16, $17, $18, $19)`,
		d.ID, d.ProjectID, d.SessionID, d.PhaseID, d.WorkUnitID, d.WorkerPromptID, d.PromptHash, d.RuntimeStateID, d.StateKey, d.RoleKey,
		d.RequestID, d.OperationID, d.Attempt, string(d.Status), d.Owner, d.CreatedAt.UTC(), d.WorkerModel, d.ContinuationID,
		correlationOf(ctx))
	if err != nil {
		return TranslateDBError("create worker dispatch", err)
	}
	return nil
}

func strPtr[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	s := string(*v)
	return &s
}

func durMS(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	v := d.Milliseconds()
	return &v
}

func transitionDispatch(ctx context.Context, ex executor, id uuid.UUID, from, to worker.DispatchStatus, u persistence.DispatchUpdate) (bool, error) {
	tag, err := ex.Exec(ctx, `UPDATE worker_dispatches SET status = $3,
		delivery = COALESCE($4, delivery), retry_class = COALESCE($5, retry_class),
		error_code = CASE WHEN $6 = '' THEN error_code ELSE $6 END, error_message = CASE WHEN $7 = '' THEN error_message ELSE $7 END,
		provider_code = CASE WHEN $8 = '' THEN provider_code ELSE $8 END, retry_after_ms = COALESCE($9, retry_after_ms),
		diagnostic = CASE WHEN $10 = '' THEN diagnostic ELSE $10 END, owner = CASE WHEN $11 = '' THEN owner ELSE $11 END,
		started_at = COALESCE($12, started_at), completed_at = COALESCE($13, completed_at), updated_at = $14
		WHERE id = $1 AND status = $2`,
		id, string(from), string(to), strPtr(u.Delivery), strPtr(u.RetryClass), u.ErrorCode, u.ErrorMessage, u.ProviderCode, durMS(u.RetryAfter),
		u.Diagnostic, u.Owner, u.StartedAt, u.CompletedAt, u.At.UTC())
	if err != nil {
		return false, TranslateDBError("transition worker dispatch", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Transition moves a dispatch from → to.
func (r *WorkerDispatchRepository) Transition(ctx context.Context, id uuid.UUID, from, to worker.DispatchStatus, u persistence.DispatchUpdate) (bool, error) {
	return transitionDispatch(ctx, r.exec, id, from, to, u)
}

// Get returns a project's dispatch.
func (r *WorkerDispatchRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*worker.WorkerDispatch, error) {
	d, err := scanDispatch(r.exec.QueryRow(ctx, `SELECT `+dispatchCols+` FROM worker_dispatches WHERE id = $1 AND project_id = $2`, id, projectID))
	if err != nil {
		return nil, TranslateDBError("get worker dispatch", err)
	}
	return d, nil
}

func (r *WorkerDispatchRepository) query(ctx context.Context, op, q string, args ...any) ([]worker.WorkerDispatch, error) {
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	var out []worker.WorkerDispatch
	for rows.Next() {
		d, err := scanDispatch(rows)
		if err != nil {
			return nil, TranslateDBError(op, err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// List pages a project's dispatches (newest first) with filters.
func (r *WorkerDispatchRepository) List(ctx context.Context, f persistence.DispatchFilter) ([]worker.WorkerDispatch, int, error) {
	where := " WHERE project_id = $1"
	args := []any{f.ProjectID}
	add := func(col string, v any) {
		args = append(args, v)
		where += " AND " + col + " = $" + itoa(len(args))
	}
	if f.PhaseID != nil {
		add("phase_id", *f.PhaseID)
	}
	if f.WorkUnitID != nil {
		add("work_unit_id", *f.WorkUnitID)
	}
	if f.StateKey != "" {
		add("state_key", f.StateKey)
	}
	if f.RoleKey != "" {
		add("role_key", f.RoleKey)
	}
	if f.Status != "" {
		add("status", string(f.Status))
	}
	if f.SessionID != nil {
		add("session_id", *f.SessionID)
	}
	if f.ContinuationID != nil {
		add("continuation_id", *f.ContinuationID)
	}
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM worker_dispatches`+where, args...).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count worker dispatches", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	out, err := r.query(ctx, "list worker dispatches", `SELECT `+dispatchCols+` FROM worker_dispatches`+where+
		` ORDER BY created_at DESC, id LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	return out, total, err
}

// ListAttempts returns every attempt of one logical operation.
func (r *WorkerDispatchRepository) ListAttempts(ctx context.Context, projectID uuid.UUID, operationID string) ([]worker.WorkerDispatch, error) {
	return r.query(ctx, "list worker dispatch attempts", `SELECT `+dispatchCols+` FROM worker_dispatches
		WHERE project_id = $1 AND operation_id = $2 ORDER BY attempt`, projectID, operationID)
}

// ListInFlight returns non-terminal dispatches.
func (r *WorkerDispatchRepository) ListInFlight(ctx context.Context, projectID *uuid.UUID) ([]worker.WorkerDispatch, error) {
	if projectID != nil {
		return r.query(ctx, "list in-flight dispatches", `SELECT `+dispatchCols+` FROM worker_dispatches
			WHERE status IN ('PENDING', 'SENDING', 'WAITING') AND project_id = $1 ORDER BY created_at`, *projectID)
	}
	return r.query(ctx, "list in-flight dispatches", `SELECT `+dispatchCols+` FROM worker_dispatches
		WHERE status IN ('PENDING', 'SENDING', 'WAITING') ORDER BY created_at`)
}

// ---------------------------------------------------------------------------
// Responses + usage (P12-54).

// WorkerResponseRepository implements persistence.WorkerResponseRepository.
type WorkerResponseRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.WorkerResponseRepository = (*WorkerResponseRepository)(nil)

// NewWorkerResponseRepository binds the repository to a pool.
func NewWorkerResponseRepository(pool *pgxpool.Pool) *WorkerResponseRepository {
	return &WorkerResponseRepository{exec: poolAdapter{pool}, pool: pool}
}

var errNotWaiting = errors.New("dispatch not waiting")

// CompleteWithResponse stores the raw response and completes the dispatch
// atomically.
func (r *WorkerResponseRepository) CompleteWithResponse(ctx context.Context, rec *worker.WorkerResponseRecord, u persistence.DispatchUpdate) (bool, error) {
	md, _ := json.Marshal(rec.ProviderMetadata)
	if rec.ProviderMetadata == nil {
		md = []byte("{}")
	}
	err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
		ok, err := transitionDispatch(ctx, tx, rec.DispatchID, worker.DispatchWaiting, worker.DispatchCompleted, u)
		if err != nil {
			return err
		}
		if !ok {
			return errNotWaiting
		}
		_, err = tx.Exec(ctx, `INSERT INTO worker_responses (id, dispatch_id, project_id, content, content_hash, provider_metadata, input_tokens,
			output_tokens, context_tokens, context_window, usage_estimated, started_at, completed_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			rec.ID, rec.DispatchID, rec.ProjectID, rec.Content, rec.ContentHash, md, rec.Usage.InputTokens, rec.Usage.OutputTokens,
			rec.Usage.ContextTokens, rec.Usage.ContextWindow, rec.Usage.Estimated, rec.StartedAt.UTC(), rec.CompletedAt.UTC(), rec.CreatedAt.UTC())
		if err != nil {
			return TranslateDBError("insert worker response", err)
		}
		return nil
	})
	if errors.Is(err, errNotWaiting) {
		return false, nil
	}
	return err == nil, err
}

// GetByDispatch returns the raw response of a dispatch.
func (r *WorkerResponseRepository) GetByDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*worker.WorkerResponseRecord, error) {
	var rec worker.WorkerResponseRecord
	var md []byte
	err := r.exec.QueryRow(ctx, `SELECT id, dispatch_id, project_id, content, content_hash, provider_metadata, input_tokens, output_tokens,
		context_tokens, context_window, usage_estimated, started_at, completed_at, created_at
		FROM worker_responses WHERE dispatch_id = $1 AND project_id = $2`, dispatchID, projectID).
		Scan(&rec.ID, &rec.DispatchID, &rec.ProjectID, &rec.Content, &rec.ContentHash, &md, &rec.Usage.InputTokens, &rec.Usage.OutputTokens,
			&rec.Usage.ContextTokens, &rec.Usage.ContextWindow, &rec.Usage.Estimated, &rec.StartedAt, &rec.CompletedAt, &rec.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("get worker response", err)
	}
	_ = json.Unmarshal(md, &rec.ProviderMetadata)
	return &rec, nil
}

// InsertObservation appends a usage observation.
func (r *WorkerResponseRepository) InsertObservation(ctx context.Context, o *persistence.WorkerUsageObservation) error {
	err := r.exec.QueryRow(ctx, `INSERT INTO worker_usage_observations (id, project_id, session_id, dispatch_id, input_tokens, output_tokens,
		context_tokens, context_window, estimated, source, observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING seq`,
		o.ID, o.ProjectID, o.SessionID, o.DispatchID, o.Usage.InputTokens, o.Usage.OutputTokens, o.Usage.ContextTokens, o.Usage.ContextWindow,
		o.Source == worker.SourceEstimated, string(o.Source), o.ObservedAt.UTC()).Scan(&o.Seq)
	if err != nil {
		return TranslateDBError("insert worker usage observation", err)
	}
	return nil
}

const obsCols = `id, seq, project_id, session_id, dispatch_id, input_tokens, output_tokens, context_tokens, context_window, estimated, source, observed_at`

func scanObs(row interface{ Scan(dest ...any) error }) (*persistence.WorkerUsageObservation, error) {
	var o persistence.WorkerUsageObservation
	var src string
	if err := row.Scan(&o.ID, &o.Seq, &o.ProjectID, &o.SessionID, &o.DispatchID, &o.Usage.InputTokens, &o.Usage.OutputTokens,
		&o.Usage.ContextTokens, &o.Usage.ContextWindow, &o.Usage.Estimated, &src, &o.ObservedAt); err != nil {
		return nil, err
	}
	o.Source = worker.UsageSource(src)
	return &o, nil
}

// ListObservations returns observations in order.
func (r *WorkerResponseRepository) ListObservations(ctx context.Context, projectID uuid.UUID, sessionID *uuid.UUID) ([]persistence.WorkerUsageObservation, error) {
	q, args := `SELECT `+obsCols+` FROM worker_usage_observations WHERE project_id = $1`, []any{projectID}
	if sessionID != nil {
		q += ` AND session_id = $2`
		args = append(args, *sessionID)
	}
	rows, err := r.exec.Query(ctx, q+` ORDER BY seq`, args...)
	if err != nil {
		return nil, TranslateDBError("list worker usage", err)
	}
	defer rows.Close()
	var out []persistence.WorkerUsageObservation
	for rows.Next() {
		o, err := scanObs(rows)
		if err != nil {
			return nil, TranslateDBError("list worker usage", err)
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// LatestObservation returns a session's newest observation.
func (r *WorkerResponseRepository) LatestObservation(ctx context.Context, projectID, sessionID uuid.UUID) (*persistence.WorkerUsageObservation, error) {
	o, err := scanObs(r.exec.QueryRow(ctx, `SELECT `+obsCols+` FROM worker_usage_observations WHERE project_id = $1 AND session_id = $2
		ORDER BY seq DESC LIMIT 1`, projectID, sessionID))
	if err != nil {
		return nil, TranslateDBError("latest worker usage", err)
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// Leases (P12-55).

// WorkerLeaseRepository implements persistence.WorkerLeaseRepository.
type WorkerLeaseRepository struct{ exec executor }

var _ persistence.WorkerLeaseRepository = (*WorkerLeaseRepository)(nil)

// NewWorkerLeaseRepository binds the repository to a pool.
func NewWorkerLeaseRepository(pool *pgxpool.Pool) *WorkerLeaseRepository {
	return &WorkerLeaseRepository{exec: poolAdapter{pool}}
}

// Acquire inserts the lease or reclaims an expired one.
func (r *WorkerLeaseRepository) Acquire(ctx context.Context, l persistence.WorkerSessionLease) (bool, error) {
	tag, err := r.exec.Exec(ctx, `INSERT INTO worker_session_leases (session_id, project_id, owner, dispatch_id, acquired_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (session_id) DO UPDATE SET owner = EXCLUDED.owner, dispatch_id = EXCLUDED.dispatch_id,
		  acquired_at = EXCLUDED.acquired_at, expires_at = EXCLUDED.expires_at
		WHERE worker_session_leases.expires_at <= EXCLUDED.acquired_at`,
		l.SessionID, l.ProjectID, l.Owner, l.DispatchID, l.AcquiredAt.UTC(), l.ExpiresAt.UTC())
	if err != nil {
		return false, TranslateDBError("acquire worker session lease", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Renew extends an owned lease.
func (r *WorkerLeaseRepository) Renew(ctx context.Context, sessionID uuid.UUID, owner string, expires time.Time) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE worker_session_leases SET expires_at = $3 WHERE session_id = $1 AND owner = $2 AND expires_at < $3`,
		sessionID, owner, expires.UTC())
	if err != nil {
		return false, TranslateDBError("renew worker session lease", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Release drops an owned lease.
func (r *WorkerLeaseRepository) Release(ctx context.Context, sessionID uuid.UUID, owner string) (bool, error) {
	tag, err := r.exec.Exec(ctx, `DELETE FROM worker_session_leases WHERE session_id = $1 AND owner = $2`, sessionID, owner)
	if err != nil {
		return false, TranslateDBError("release worker session lease", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Get returns the lease of a session.
func (r *WorkerLeaseRepository) Get(ctx context.Context, sessionID uuid.UUID) (*persistence.WorkerSessionLease, error) {
	var l persistence.WorkerSessionLease
	err := r.exec.QueryRow(ctx, `SELECT session_id, project_id, owner, dispatch_id, acquired_at, expires_at FROM worker_session_leases WHERE session_id = $1`,
		sessionID).Scan(&l.SessionID, &l.ProjectID, &l.Owner, &l.DispatchID, &l.AcquiredAt, &l.ExpiresAt)
	if err != nil {
		return nil, TranslateDBError("get worker session lease", err)
	}
	return &l, nil
}

// ReleaseExpired deletes expired leases (crash recovery, WBS 12.57.4).
func (r *WorkerLeaseRepository) ReleaseExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := r.exec.Exec(ctx, `DELETE FROM worker_session_leases WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, TranslateDBError("release expired worker leases", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Events (P12-56).

// WorkerEventRepository implements persistence.WorkerEventRepository.
type WorkerEventRepository struct{ exec executor }

var _ persistence.WorkerEventRepository = (*WorkerEventRepository)(nil)

// NewWorkerEventRepository binds the repository to a pool.
func NewWorkerEventRepository(pool *pgxpool.Pool) *WorkerEventRepository {
	return &WorkerEventRepository{exec: poolAdapter{pool}}
}

// Append stores an event.
func (r *WorkerEventRepository) Append(ctx context.Context, e *persistence.WorkerEventRecord) error {
	p := e.Payload
	if p == nil {
		p = map[string]any{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return &persistence.InfrastructureError{Op: "encode worker event", Cause: err}
	}
	if err := r.exec.QueryRow(ctx, `INSERT INTO worker_events (id, project_id, session_id, dispatch_id, type, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING seq`, e.ID, e.ProjectID, e.SessionID, e.DispatchID, string(e.Type), b, e.CreatedAt.UTC()).
		Scan(&e.Seq); err != nil {
		return TranslateDBError("append worker event", err)
	}
	return nil
}

// List returns events in order.
func (r *WorkerEventRepository) List(ctx context.Context, f persistence.WorkerEventFilter) ([]persistence.WorkerEventRecord, error) {
	q, args := `SELECT id, seq, project_id, session_id, dispatch_id, type, payload, created_at FROM worker_events WHERE project_id = $1`, []any{f.ProjectID}
	if f.SessionID != nil {
		args = append(args, *f.SessionID)
		q += ` AND session_id = $` + itoa(len(args))
	}
	if f.DispatchID != nil {
		args = append(args, *f.DispatchID)
		q += ` AND dispatch_id = $` + itoa(len(args))
	}
	rows, err := r.exec.Query(ctx, q+` ORDER BY seq`, args...)
	if err != nil {
		return nil, TranslateDBError("list worker events", err)
	}
	defer rows.Close()
	var out []persistence.WorkerEventRecord
	for rows.Next() {
		var e persistence.WorkerEventRecord
		var typ string
		var b []byte
		if err := rows.Scan(&e.ID, &e.Seq, &e.ProjectID, &e.SessionID, &e.DispatchID, &typ, &b, &e.CreatedAt); err != nil {
			return nil, TranslateDBError("list worker events", err)
		}
		e.Type = worker.EventType(typ)
		_ = json.Unmarshal(b, &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendTool stores a tool event.
func (r *WorkerEventRepository) AppendTool(ctx context.Context, t *persistence.WorkerToolEventRecord) error {
	var dur *int64
	if t.Activity.Duration != nil {
		v := t.Activity.Duration.Milliseconds()
		dur = &v
	}
	_, err := r.exec.Exec(ctx, `INSERT INTO worker_tool_events (id, project_id, dispatch_id, tool_name, state, started_at, finished_at, duration_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t.ID, t.ProjectID, t.DispatchID, t.Activity.Name, string(t.Activity.State),
		t.Activity.StartedAt.UTC(), t.Activity.FinishedAt, dur, t.CreatedAt.UTC())
	if err != nil {
		return TranslateDBError("append worker tool event", err)
	}
	return nil
}

// ListTools returns a dispatch's tool events in order.
func (r *WorkerEventRepository) ListTools(ctx context.Context, projectID, dispatchID uuid.UUID) ([]persistence.WorkerToolEventRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, project_id, dispatch_id, tool_name, state, started_at, finished_at, duration_ms, created_at
		FROM worker_tool_events WHERE project_id = $1 AND dispatch_id = $2 ORDER BY seq`, projectID, dispatchID)
	if err != nil {
		return nil, TranslateDBError("list worker tool events", err)
	}
	defer rows.Close()
	var out []persistence.WorkerToolEventRecord
	for rows.Next() {
		var t persistence.WorkerToolEventRecord
		var state string
		var dur *int64
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.DispatchID, &t.Activity.Name, &state, &t.Activity.StartedAt, &t.Activity.FinishedAt, &dur, &t.CreatedAt); err != nil {
			return nil, TranslateDBError("list worker tool events", err)
		}
		t.Activity.State = worker.ToolState(state)
		if dur != nil {
			d := time.Duration(*dur) * time.Millisecond
			t.Activity.Duration = &d
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
