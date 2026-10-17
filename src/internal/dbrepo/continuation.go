package dbrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/workerresult"
)

// ---------------------------------------------------------------------------
// Settings (P14-33).

// ContinuationSettingsRepository implements persistence.ContinuationSettingsRepository.
type ContinuationSettingsRepository struct{ exec executor }

var _ persistence.ContinuationSettingsRepository = (*ContinuationSettingsRepository)(nil)

// NewContinuationSettingsRepository binds the repository to a pool.
func NewContinuationSettingsRepository(pool *pgxpool.Pool) *ContinuationSettingsRepository {
	return &ContinuationSettingsRepository{exec: poolAdapter{pool}}
}

// Get returns stored settings or defaults (false).
func (r *ContinuationSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (continuation.Settings, bool, error) {
	var s continuation.Settings
	var policy string
	var lease int
	err := r.exec.QueryRow(ctx, `SELECT unknown_context_policy, estimated_may_reset, max_continuations_per_work_unit,
		max_session_resets_per_work_unit, max_no_progress_continuations, session_create_retries, lease_seconds
		FROM project_continuation_settings WHERE project_id = $1`, projectID).
		Scan(&policy, &s.EstimatedMayReset, &s.MaxContinuationsPerWorkUnit, &s.MaxSessionResetsPerWorkUnit,
			&s.MaxNoProgressContinuations, &s.SessionCreateRetries, &lease)
	if err != nil {
		if errors.Is(TranslateDBError("get continuation settings", err), persistence.ErrNotFound) {
			return continuation.DefaultSettings(), false, nil
		}
		return continuation.Settings{}, false, TranslateDBError("get continuation settings", err)
	}
	s.UnknownContextPolicy, s.LeaseDuration = continuation.Strategy(policy), time.Duration(lease)*time.Second
	return s, true, nil
}

// Upsert stores settings.
func (r *ContinuationSettingsRepository) Upsert(ctx context.Context, projectID uuid.UUID, s continuation.Settings) error {
	_, err := r.exec.Exec(ctx, `INSERT INTO project_continuation_settings (project_id, unknown_context_policy, estimated_may_reset,
		max_continuations_per_work_unit, max_session_resets_per_work_unit, max_no_progress_continuations, session_create_retries, lease_seconds, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (project_id) DO UPDATE SET unknown_context_policy = EXCLUDED.unknown_context_policy,
		  estimated_may_reset = EXCLUDED.estimated_may_reset, max_continuations_per_work_unit = EXCLUDED.max_continuations_per_work_unit,
		  max_session_resets_per_work_unit = EXCLUDED.max_session_resets_per_work_unit,
		  max_no_progress_continuations = EXCLUDED.max_no_progress_continuations, session_create_retries = EXCLUDED.session_create_retries,
		  lease_seconds = EXCLUDED.lease_seconds, updated_at = now()`,
		projectID, string(s.UnknownContextPolicy), s.EstimatedMayReset, s.MaxContinuationsPerWorkUnit, s.MaxSessionResetsPerWorkUnit,
		s.MaxNoProgressContinuations, s.SessionCreateRetries, int(s.LeaseDuration/time.Second))
	if err != nil {
		return TranslateDBError("upsert continuation settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Continuations (P14-34 .. P14-38).

// ContinuationRepository implements persistence.ContinuationRepository.
type ContinuationRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.ContinuationRepository = (*ContinuationRepository)(nil)

// NewContinuationRepository binds the repository to a pool.
func NewContinuationRepository(pool *pgxpool.Pool) *ContinuationRepository {
	return &ContinuationRepository{exec: poolAdapter{pool}, pool: pool}
}

const terminalSQL = `('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED')`

var contColNames = []string{"id", "project_id", "phase_id", "work_unit_id", "runtime_state_id", "runtime_state_version",
	"source_dispatch_id", "source_result_id", "trigger", "status", "strategy", "decision", "attempt", "captured_status", "status_hash",
	"progress_key", "status_unknown", "no_progress_streak", "scope_expanded", "source_snapshot_id", "snapshot_id", "role_key", "state_key",
	"session_id", "old_session_id", "new_session_id", "session_confirmed_at", "cwd_confirmed_at", "session_create_failures",
	"worker_prompt_id", "dispatch_id", "recovery_reason", "override_strategy", "retry_of_id", "failure_message", "version",
	"lease_owner", "lease_expires_at", "created_at", "updated_at", "completed_at"}

func contCols(prefix string) string {
	out := make([]string, len(contColNames))
	for i, c := range contColNames {
		out[i] = prefix + c
	}
	return strings.Join(out, ", ")
}

func scanContinuation(row interface{ Scan(dest ...any) error }) (*continuation.Continuation, error) {
	var c continuation.Continuation
	var trigger, status string
	var strategy, recovery, override, leaseOwner *string
	var decision, captured []byte
	if err := row.Scan(&c.ID, &c.ProjectID, &c.PhaseID, &c.WorkUnitID, &c.RuntimeStateID, &c.RuntimeStateVersion, &c.SourceDispatchID,
		&c.SourceResultID, &trigger, &status, &strategy, &decision, &c.Attempt, &captured, &c.StatusHash, &c.ProgressKey, &c.StatusUnknown,
		&c.NoProgressStreak, &c.ScopeExpanded, &c.SourceSnapshotID, &c.SnapshotID, &c.RoleKey, &c.StateKey, &c.SessionID, &c.OldSessionID,
		&c.NewSessionID, &c.SessionConfirmedAt, &c.CWDConfirmedAt, &c.SessionCreateFailures, &c.WorkerPromptID, &c.DispatchID, &recovery,
		&override, &c.RetryOfID, &c.FailureMessage, &c.Version, &leaseOwner, &c.LeaseExpiresAt, &c.CreatedAt, &c.UpdatedAt, &c.CompletedAt); err != nil {
		return nil, err
	}
	c.Trigger, c.Status = continuation.Trigger(trigger), continuation.Status(status)
	if strategy != nil {
		c.Strategy = continuation.Strategy(*strategy)
	}
	if recovery != nil {
		c.RecoveryReason = continuation.RecoveryReason(*recovery)
	}
	if override != nil {
		c.OverrideStrategy = continuation.Strategy(*override)
	}
	if leaseOwner != nil {
		c.LeaseOwner = *leaseOwner
	}
	if len(decision) > 0 {
		d, err := continuation.DecodeDecision(decision)
		if err != nil {
			return nil, err
		}
		c.Decision = &d
	}
	cs, err := continuation.DecodeCaptured(captured)
	if err != nil {
		return nil, err
	}
	c.Captured = cs
	return &c, nil
}

func optStr[T ~string](v T) *string {
	if v == "" {
		return nil
	}
	s := string(v)
	return &s
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func insertContinuation(ctx context.Context, ex executor, c *continuation.Continuation) error {
	var strategy, reason, decision *string
	if c.Decision != nil {
		strategy, reason = optStr(c.Decision.Strategy), optStr(c.Decision.Reason)
		d := string(continuation.EncodeDecision(*c.Decision))
		decision = &d
	}
	_, err := ex.Exec(ctx, `INSERT INTO continuations (id, project_id, phase_id, work_unit_id, runtime_state_id, runtime_state_version,
		source_dispatch_id, source_result_id, trigger, status, strategy, decision_reason, decision, attempt, captured_status, status_hash,
		progress_key, status_unknown, no_progress_streak, scope_expanded, source_snapshot_id, snapshot_id, role_key, state_key,
		retry_of_id, override_strategy, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, 0, $27, $27)`,
		c.ID, c.ProjectID, c.PhaseID, c.WorkUnitID, c.RuntimeStateID, c.RuntimeStateVersion, c.SourceDispatchID, c.SourceResultID,
		string(c.Trigger), string(c.Status), strategy, reason, decision, c.Attempt, string(c.Captured.Canonical()), c.StatusHash, c.ProgressKey,
		c.StatusUnknown, c.NoProgressStreak, c.ScopeExpanded, c.SourceSnapshotID, c.SnapshotID, c.RoleKey, c.StateKey, c.RetryOfID,
		optStr(c.OverrideStrategy), c.CreatedAt.UTC())
	if err != nil {
		return TranslateDBError("insert continuation", err)
	}
	return insertTransition(ctx, ex, c.ID, nil, c.Status, string(c.Trigger), c.CreatedAt)
}

func insertTransition(ctx context.Context, ex executor, id uuid.UUID, from *continuation.Status, to continuation.Status, reason string, at time.Time) error {
	var f *string
	if from != nil {
		s := string(*from)
		f = &s
	}
	_, err := ex.Exec(ctx, `INSERT INTO continuation_transitions (id, continuation_id, sequence, from_status, to_status, reason, created_at)
		SELECT $1, $2, COALESCE(MAX(sequence), 0) + 1, $3, $4, $5, $6 FROM continuation_transitions WHERE continuation_id = $2`,
		uuid.New(), id, f, string(to), reason, at.UTC())
	if err != nil {
		return TranslateDBError("insert continuation transition", err)
	}
	return nil
}

func insertConsumption(ctx context.Context, ex executor, c persistence.ResultConsumption) error {
	tag, err := ex.Exec(ctx, `INSERT INTO worker_result_consumptions (result_id, consumer, project_id, continuation_id, consumed_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (result_id, consumer) DO NOTHING`,
		c.ResultID, c.Consumer, c.ProjectID, c.ContinuationID, c.ConsumedAt.UTC())
	if err != nil {
		return TranslateDBError("insert result consumption", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrAlreadyConsumed
	}
	return nil
}

// Create inserts a continuation (+ consumption) atomically.
func (r *ContinuationRepository) Create(ctx context.Context, c *continuation.Continuation, consume *persistence.ResultConsumption) error {
	return Run(ctx, r.pool, func(tx persistence.Transaction) error {
		if err := insertContinuation(ctx, tx, c); err != nil {
			return err
		}
		if consume != nil {
			return insertConsumption(ctx, tx, *consume)
		}
		return nil
	})
}

// Consume records only a consumption.
func (r *ContinuationRepository) Consume(ctx context.Context, consume persistence.ResultConsumption) error {
	return insertConsumption(ctx, r.exec, consume)
}

func (r *ContinuationRepository) one(ctx context.Context, ex executor, where string, args ...any) (*continuation.Continuation, error) {
	c, err := scanContinuation(ex.QueryRow(ctx, `SELECT `+contCols("")+` FROM continuations WHERE `+where, args...))
	if err != nil {
		return nil, TranslateDBError("get continuation", err)
	}
	return c, nil
}

func (r *ContinuationRepository) many(ctx context.Context, sql string, args ...any) ([]continuation.Continuation, error) {
	rows, err := r.exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("list continuations", err)
	}
	defer rows.Close()
	var out []continuation.Continuation
	for rows.Next() {
		c, err := scanContinuation(rows)
		if err != nil {
			return nil, TranslateDBError("scan continuation", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list continuations", err)
	}
	return out, nil
}

// Get returns a project-scoped continuation.
func (r *ContinuationRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*continuation.Continuation, error) {
	return r.one(ctx, r.exec, `id = $1 AND project_id = $2`, id, projectID)
}

// GetCurrentForWorkUnit returns the unit's nonterminal continuation.
func (r *ContinuationRepository) GetCurrentForWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) (*continuation.Continuation, error) {
	return r.one(ctx, r.exec, `project_id = $1 AND work_unit_id = $2 AND status NOT IN `+terminalSQL, projectID, workUnitID)
}

// CurrentForProject returns the newest nonterminal continuation.
func (r *ContinuationRepository) CurrentForProject(ctx context.Context, projectID uuid.UUID) (*continuation.Continuation, error) {
	return r.one(ctx, r.exec, `project_id = $1 AND status NOT IN `+terminalSQL+` ORDER BY created_at DESC LIMIT 1`, projectID)
}

// List filters continuations, newest first.
func (r *ContinuationRepository) List(ctx context.Context, f persistence.ContinuationFilter) ([]continuation.Continuation, int, error) {
	where := []string{"project_id = $1"}
	args := []any{f.ProjectID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.PhaseID != nil {
		add("phase_id = $%d", *f.PhaseID)
	}
	if f.WorkUnitID != nil {
		add("work_unit_id = $%d", *f.WorkUnitID)
	}
	if f.Status != "" {
		add("status = $%d", string(f.Status))
	}
	if f.Strategy != "" {
		add("strategy = $%d", string(f.Strategy))
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM continuations WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count continuations", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	out, err := r.many(ctx, `SELECT `+contCols("")+` FROM continuations WHERE `+w+fmt.Sprintf(` ORDER BY created_at DESC, attempt DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	return out, total, err
}

// ListByWorkUnit returns the unit's continuations in attempt order.
func (r *ContinuationRepository) ListByWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) ([]continuation.Continuation, error) {
	return r.many(ctx, `SELECT `+contCols("")+` FROM continuations WHERE project_id = $1 AND work_unit_id = $2 ORDER BY attempt`, projectID, workUnitID)
}

// ListNonterminal returns the project's nonterminal continuations.
func (r *ContinuationRepository) ListNonterminal(ctx context.Context, projectID uuid.UUID) ([]continuation.Continuation, error) {
	return r.many(ctx, `SELECT `+contCols("")+` FROM continuations WHERE project_id = $1 AND status NOT IN `+terminalSQL+` ORDER BY created_at`, projectID)
}

// ListAll returns every continuation of the project (metrics).
func (r *ContinuationRepository) ListAll(ctx context.Context, projectID uuid.UUID) ([]continuation.Continuation, error) {
	return r.many(ctx, `SELECT `+contCols("")+` FROM continuations WHERE project_id = $1 ORDER BY created_at`, projectID)
}

func applyMutation(to *continuation.Status, m persistence.ContinuationMutation, at time.Time) ([]string, []any) {
	sets := []string{"version = version + 1", "updated_at = $2"}
	args := []any{nil, at.UTC()}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if to != nil {
		add("status", string(*to))
		if to.IsTerminal() {
			add("completed_at", at.UTC())
		}
	}
	if d := m.Decision; d != nil {
		add("strategy", string(d.Strategy))
		add("decision_reason", string(d.Reason))
		add("decision", string(continuation.EncodeDecision(*d)))
	}
	if m.ClearSession {
		sets = append(sets, "new_session_id = NULL", "session_confirmed_at = NULL", "cwd_confirmed_at = NULL")
	}
	if m.ClearCWD {
		sets = append(sets, "cwd_confirmed_at = NULL")
	}
	if m.SessionID != nil {
		add("session_id", *m.SessionID)
	}
	if m.OldSessionID != nil {
		add("old_session_id", *m.OldSessionID)
	}
	if m.NewSessionID != nil {
		add("new_session_id", *m.NewSessionID)
	}
	if m.SessionConfirmedAt != nil {
		add("session_confirmed_at", m.SessionConfirmedAt.UTC())
	}
	if m.CWDConfirmedAt != nil {
		add("cwd_confirmed_at", m.CWDConfirmedAt.UTC())
	}
	if m.SessionCreateFailures != nil {
		add("session_create_failures", *m.SessionCreateFailures)
	}
	if m.SnapshotID != nil {
		add("snapshot_id", *m.SnapshotID)
	}
	if m.WorkerPromptID != nil {
		add("worker_prompt_id", *m.WorkerPromptID)
	}
	if m.ClearDispatch {
		sets = append(sets, "dispatch_id = NULL", "worker_prompt_id = NULL")
	}
	if m.DispatchID != nil {
		add("dispatch_id", *m.DispatchID)
	}
	if m.RecoveryReason != nil {
		add("recovery_reason", string(*m.RecoveryReason))
	}
	if m.ClearOverride {
		sets = append(sets, "override_strategy = NULL")
	}
	if m.OverrideStrategy != nil {
		add("override_strategy", string(*m.OverrideStrategy))
	}
	if m.FailureMessage != nil {
		add("failure_message", *m.FailureMessage)
	}
	if m.NoProgressStreak != nil {
		add("no_progress_streak", *m.NoProgressStreak)
	}
	if m.ScopeExpanded != nil {
		add("scope_expanded", *m.ScopeExpanded)
	}
	return sets, args
}

func (r *ContinuationRepository) advanceTx(ctx context.Context, tx persistence.Transaction, id uuid.UUID, expected int, to *continuation.Status,
	reason string, m persistence.ContinuationMutation, at time.Time) (*continuation.Continuation, error) {
	var status string
	var version int
	if err := tx.QueryRow(ctx, `SELECT status, version FROM continuations WHERE id = $1 FOR UPDATE`, id).Scan(&status, &version); err != nil {
		return nil, TranslateDBError("lock continuation", err)
	}
	if version != expected {
		return nil, persistence.ErrStaleVersion
	}
	from := continuation.Status(status)
	if to != nil {
		if err := continuation.ValidateTransition(from, *to); err != nil {
			return nil, err
		}
	} else if from.IsTerminal() {
		return nil, fmt.Errorf("%w: %s is final", continuation.ErrIllegalTransition, from)
	}
	sets, args := applyMutation(to, m, at)
	args[0] = id
	if _, err := tx.Exec(ctx, `UPDATE continuations SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...); err != nil {
		return nil, TranslateDBError("update continuation", err)
	}
	if to != nil {
		if err := insertTransition(ctx, tx, id, &from, *to, reason, at); err != nil {
			return nil, err
		}
	}
	return r.one(ctx, tx, `id = $1`, id)
}

// Advance applies one versioned lifecycle transition atomically.
func (r *ContinuationRepository) Advance(ctx context.Context, id uuid.UUID, expected int, to continuation.Status, reason string,
	m persistence.ContinuationMutation, at time.Time) (*continuation.Continuation, error) {
	var out *continuation.Continuation
	err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
		c, err := r.advanceTx(ctx, tx, id, expected, &to, reason, m, at)
		out = c
		return err
	})
	return out, err
}

// Update changes fields without a status change.
func (r *ContinuationRepository) Update(ctx context.Context, id uuid.UUID, expected int, m persistence.ContinuationMutation, at time.Time) (*continuation.Continuation, error) {
	var out *continuation.Continuation
	err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
		c, err := r.advanceTx(ctx, tx, id, expected, nil, "", m, at)
		out = c
		return err
	})
	return out, err
}

// ConsumeAndAdvance records the consumption, advances and optionally
// creates the next continuation in one transaction.
func (r *ContinuationRepository) ConsumeAndAdvance(ctx context.Context, consume persistence.ResultConsumption, id uuid.UUID, expected int,
	to continuation.Status, reason string, m persistence.ContinuationMutation, at time.Time, next *continuation.Continuation) (*continuation.Continuation, error) {
	var out *continuation.Continuation
	err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
		if err := insertConsumption(ctx, tx, consume); err != nil {
			return err
		}
		c, err := r.advanceTx(ctx, tx, id, expected, &to, reason, m, at)
		if err != nil {
			return err
		}
		out = c
		if next != nil {
			return insertContinuation(ctx, tx, next)
		}
		return nil
	})
	return out, err
}

// Claim takes the lease when free, expired or already ours.
func (r *ContinuationRepository) Claim(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE continuations SET lease_owner = $2, lease_expires_at = $3
		WHERE id = $1 AND status NOT IN `+terminalSQL+` AND (lease_owner IS NULL OR lease_owner = $2 OR lease_expires_at IS NULL OR lease_expires_at < $4)`,
		id, owner, now.Add(ttl).UTC(), now.UTC())
	if err != nil {
		return false, TranslateDBError("claim continuation", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Release drops our lease.
func (r *ContinuationRepository) Release(ctx context.Context, id uuid.UUID, owner string) error {
	_, err := r.exec.Exec(ctx, `UPDATE continuations SET lease_owner = NULL, lease_expires_at = NULL WHERE id = $1 AND lease_owner = $2`, id, owner)
	if err != nil {
		return TranslateDBError("release continuation", err)
	}
	return nil
}

// ListClaimable returns nonterminal continuations of RUNNING projects
// whose lease is free or expired.
func (r *ContinuationRepository) ListClaimable(ctx context.Context, now time.Time, limit int) ([]continuation.Continuation, error) {
	if limit <= 0 {
		limit = 50
	}
	return r.many(ctx, `SELECT `+contCols("c.")+` FROM continuations c JOIN projects p ON p.id = c.project_id
		WHERE c.status NOT IN `+terminalSQL+` AND p.status = 'RUNNING'
		  AND (c.lease_owner IS NULL OR c.lease_expires_at IS NULL OR c.lease_expires_at < $1)
		ORDER BY c.updated_at LIMIT $2`, now.UTC(), limit)
}

// Transitions returns the history in sequence order.
func (r *ContinuationRepository) Transitions(ctx context.Context, projectID, id uuid.UUID) ([]persistence.ContinuationTransition, error) {
	rows, err := r.exec.Query(ctx, `SELECT t.id, t.continuation_id, t.sequence, t.from_status, t.to_status, t.reason, t.created_at
		FROM continuation_transitions t JOIN continuations c ON c.id = t.continuation_id
		WHERE c.id = $1 AND c.project_id = $2 ORDER BY t.sequence`, id, projectID)
	if err != nil {
		return nil, TranslateDBError("list continuation transitions", err)
	}
	defer rows.Close()
	var out []persistence.ContinuationTransition
	for rows.Next() {
		var t persistence.ContinuationTransition
		var from *string
		var to string
		if err := rows.Scan(&t.ID, &t.ContinuationID, &t.Sequence, &from, &to, &t.Reason, &t.CreatedAt); err != nil {
			return nil, TranslateDBError("scan continuation transition", err)
		}
		if from != nil {
			f := continuation.Status(*from)
			t.From = &f
		}
		t.To = continuation.Status(to)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountsForWorkUnit returns the unit's counters; successful resets are
// confirmed session creations (CREATING_SESSION → RESTORING_CONTEXT).
func (r *ContinuationRepository) CountsForWorkUnit(ctx context.Context, projectID, workUnitID uuid.UUID) (persistence.WorkUnitCounts, error) {
	var c persistence.WorkUnitCounts
	if err := r.exec.QueryRow(ctx, `SELECT count(*), COALESCE(max(attempt), 0), COALESCE(sum(session_create_failures), 0)
		FROM continuations WHERE project_id = $1 AND work_unit_id = $2`, projectID, workUnitID).
		Scan(&c.Continuations, &c.MaxAttempt, &c.SessionCreateFailures); err != nil {
		return c, TranslateDBError("count continuations", err)
	}
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM continuation_transitions t JOIN continuations c ON c.id = t.continuation_id
		WHERE c.project_id = $1 AND c.work_unit_id = $2 AND t.to_status = 'RESTORING_CONTEXT'`, projectID, workUnitID).Scan(&c.SuccessfulResets); err != nil {
		return c, TranslateDBError("count session resets", err)
	}
	if c.Continuations > 0 {
		l, err := r.one(ctx, r.exec, `project_id = $1 AND work_unit_id = $2 ORDER BY attempt DESC LIMIT 1`, projectID, workUnitID)
		if err != nil {
			return c, err
		}
		c.Latest = l
	}
	return c, nil
}

// IsConsumed reports a recorded consumption.
func (r *ContinuationRepository) IsConsumed(ctx context.Context, resultID uuid.UUID, consumer string) (bool, error) {
	var ok bool
	if err := r.exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM worker_result_consumptions WHERE result_id = $1 AND consumer = $2)`,
		resultID, consumer).Scan(&ok); err != nil {
		return false, TranslateDBError("check consumption", err)
	}
	return ok, nil
}

// ListUnconsumed returns accepted results Phase 14 must consume: INCOMPLETE
// results bound to a work unit and every result of a continuation dispatch.
func (r *ContinuationRepository) ListUnconsumed(ctx context.Context, limit int) ([]persistence.UnconsumedResult, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.exec.Query(ctx, `SELECT r.id, r.dispatch_id, r.project_id, r.status, d.continuation_id
		FROM worker_results r JOIN worker_dispatches d ON d.id = r.dispatch_id
		WHERE NOT EXISTS (SELECT 1 FROM worker_result_consumptions x WHERE x.result_id = r.id AND x.consumer = $1)
		  AND r.work_unit_id IS NOT NULL AND r.phase_id IS NOT NULL
		  AND (d.continuation_id IS NOT NULL OR r.status = 'incomplete')
		ORDER BY r.created_at LIMIT $2`, persistence.ConsumerContinuation, limit)
	if err != nil {
		return nil, TranslateDBError("list unconsumed results", err)
	}
	defer rows.Close()
	var out []persistence.UnconsumedResult
	for rows.Next() {
		var u persistence.UnconsumedResult
		var st string
		if err := rows.Scan(&u.ResultID, &u.DispatchID, &u.ProjectID, &st, &u.ContinuationID); err != nil {
			return nil, TranslateDBError("scan unconsumed result", err)
		}
		u.Status = workerresult.WorkerResultStatus(st)
		out = append(out, u)
	}
	return out, rows.Err()
}
