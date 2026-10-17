package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// EngineRepository implements persistence.EngineRepository (P16-64 …
// P16-75).
type EngineRepository struct {
	exec  executor
	pool  *pgxpool.Pool
	execs *ExecutionRepository
}

var _ persistence.EngineRepository = (*EngineRepository)(nil)

// NewEngineRepository binds the repository to a pool.
func NewEngineRepository(pool *pgxpool.Pool) *EngineRepository {
	return &EngineRepository{exec: poolAdapter{pool}, pool: pool, execs: NewExecutionRepository(pool)}
}

func (r *EngineRepository) ex(e persistence.Executor) executor {
	if e == nil {
		return r.exec
	}
	return e
}

func jsonOf(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// ---------------------------------------------------------------------------
// Settings (P16-64).

// GetEngineSettings returns stored settings or defaults (false).
func (r *EngineRepository) GetEngineSettings(ctx context.Context, projectID uuid.UUID) (execution.EngineSettings, hardening.Settings, bool, error) {
	var s execution.EngineSettings
	var policy string
	var unit, phase, final, atomic, hard []byte
	var timeout, lease int
	err := r.exec.QueryRow(ctx, `SELECT max_fix_attempts, max_plan_attempts, checkpoint_policy, unit_validation, phase_validation,
		final_validation, validation_timeout_seconds, output_cap_bytes, step_budget, driver_lease_seconds, atomic_policy, hardening
		FROM project_engine_settings WHERE project_id = $1`, projectID).
		Scan(&s.MaxFixAttempts, &s.MaxPlanAttempts, &policy, &unit, &phase, &final, &timeout, &s.OutputCapBytes, &s.StepBudget, &lease, &atomic, &hard)
	if err != nil {
		if errors.Is(TranslateDBError("get engine settings", err), persistence.ErrNotFound) {
			return execution.DefaultEngineSettings(), hardening.DefaultSettings(), false, nil
		}
		return execution.EngineSettings{}, hardening.Settings{}, false, TranslateDBError("get engine settings", err)
	}
	s.CheckpointPolicy = execution.CheckpointPolicy(policy)
	s.ValidationTimeout, s.DriverLease = time.Duration(timeout)*time.Second, time.Duration(lease)*time.Second
	_ = json.Unmarshal(unit, &s.UnitValidation)
	_ = json.Unmarshal(phase, &s.PhaseValidation)
	_ = json.Unmarshal(final, &s.FinalValidation)
	_ = json.Unmarshal(atomic, &s.AtomicPolicy)
	s.Normalize()
	h := hardening.DefaultSettings()
	_ = json.Unmarshal(hard, &h)
	if h.Exclusions == nil {
		h.Exclusions = []string{}
	}
	return s, h, true, nil
}

// UpsertEngineSettings stores settings.
func (r *EngineRepository) UpsertEngineSettings(ctx context.Context, projectID uuid.UUID, s execution.EngineSettings, h hardening.Settings) error {
	s.Normalize()
	if h.Exclusions == nil {
		h.Exclusions = []string{}
	}
	_, err := r.exec.Exec(ctx, `INSERT INTO project_engine_settings (project_id, max_fix_attempts, max_plan_attempts,
		checkpoint_policy, unit_validation, phase_validation, final_validation, validation_timeout_seconds, output_cap_bytes,
		step_budget, driver_lease_seconds, atomic_policy, hardening, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		ON CONFLICT (project_id) DO UPDATE SET max_fix_attempts = EXCLUDED.max_fix_attempts,
		  max_plan_attempts = EXCLUDED.max_plan_attempts, checkpoint_policy = EXCLUDED.checkpoint_policy,
		  unit_validation = EXCLUDED.unit_validation, phase_validation = EXCLUDED.phase_validation,
		  final_validation = EXCLUDED.final_validation, validation_timeout_seconds = EXCLUDED.validation_timeout_seconds,
		  output_cap_bytes = EXCLUDED.output_cap_bytes, step_budget = EXCLUDED.step_budget,
		  driver_lease_seconds = EXCLUDED.driver_lease_seconds, atomic_policy = EXCLUDED.atomic_policy,
		  hardening = EXCLUDED.hardening, updated_at = now()`,
		projectID, s.MaxFixAttempts, s.MaxPlanAttempts, string(s.CheckpointPolicy), jsonOf(s.UnitValidation), jsonOf(s.PhaseValidation),
		jsonOf(s.FinalValidation), int(s.ValidationTimeout/time.Second), s.OutputCapBytes, s.StepBudget, int(s.DriverLease/time.Second),
		jsonOf(s.AtomicPolicy), jsonOf(h))
	if err != nil {
		return TranslateDBError("upsert engine settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Driver lease (P16-65).

// AcquireDriver takes the execution's driver lease when it is free,
// expired, or already ours. A takeover from another owner (or after
// expiry) increments the fencing epoch.
func (r *EngineRepository) AcquireDriver(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (int64, bool, error) {
	var epoch int64
	err := r.exec.QueryRow(ctx, `UPDATE executions SET
		driver_epoch = CASE WHEN driver_owner = $2 AND driver_expires_at > $3 THEN driver_epoch ELSE driver_epoch + 1 END,
		driver_owner = $2, driver_expires_at = $4
		WHERE id = $1 AND status <> 'COMPLETED' AND (driver_owner IS NULL OR driver_expires_at <= $3 OR driver_owner = $2)
		RETURNING driver_epoch`, id, owner, now.UTC(), now.Add(ttl).UTC()).Scan(&epoch)
	if err != nil {
		if errors.Is(TranslateDBError("acquire driver", err), persistence.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, TranslateDBError("acquire driver", err)
	}
	return epoch, true, nil
}

// RenewDriver extends a held lease (same owner and epoch, not expired).
func (r *EngineRepository) RenewDriver(ctx context.Context, id uuid.UUID, owner string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE executions SET driver_expires_at = $5 WHERE id = $1 AND status <> 'COMPLETED'
		AND driver_owner = $2 AND driver_epoch = $3 AND driver_expires_at > $4`, id, owner, epoch, now.UTC(), now.Add(ttl).UTC())
	if err != nil {
		return false, TranslateDBError("renew driver", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseDriver frees a held lease (no-op when not held).
func (r *EngineRepository) ReleaseDriver(ctx context.Context, id uuid.UUID, owner string, epoch int64) error {
	if _, err := r.exec.Exec(ctx, `UPDATE executions SET driver_owner = NULL, driver_expires_at = NULL
		WHERE id = $1 AND status <> 'COMPLETED' AND driver_owner = $2 AND driver_epoch = $3`, id, owner, epoch); err != nil {
		return TranslateDBError("release driver", err)
	}
	return nil
}

// CheckDriver locks the execution row and verifies the caller still holds
// the lease at the expected epoch (fencing, WBS 16.4).
func (r *EngineRepository) CheckDriver(ctx context.Context, ex persistence.Executor, id uuid.UUID, owner string, epoch int64, now time.Time) error {
	var ok bool
	err := r.ex(ex).QueryRow(ctx, `SELECT driver_owner = $2 AND driver_epoch = $3 AND driver_expires_at > $4
		FROM executions WHERE id = $1 FOR UPDATE`, id, owner, epoch, now.UTC()).Scan(&ok)
	if err != nil {
		return TranslateDBError("check driver", err)
	}
	if !ok {
		return persistence.ErrConflict
	}
	return nil
}

// ---------------------------------------------------------------------------
// Position (P16-66).

// UpdatePosition is a versioned CAS of the engine position. A status
// change is validated and recorded in the status history.
func (r *EngineRepository) UpdatePosition(ctx context.Context, ex persistence.Executor, id uuid.UUID, expectedVersion int, upd persistence.PositionUpdate) (*persistence.ExecutionRecord, error) {
	x := r.ex(ex)
	cur, err := r.execs.one(ctx, x, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if cur.Version != expectedVersion {
		return nil, persistence.ErrConflict
	}
	status := cur.Status
	if upd.Status != "" && upd.Status != cur.Status {
		if err := execution.ValidateTransition(cur.Status, upd.Status); err != nil {
			return nil, err
		}
		status = upd.Status
	}
	now := time.Now().UTC()
	phase, unit := cur.CurrentPhaseID, cur.CurrentWorkUnitID
	if upd.SetPhase {
		phase = upd.PhaseID
	}
	if upd.SetUnit {
		unit = upd.UnitID
	}
	rt := cur.RuntimeStateID
	if upd.RuntimeStateID != nil {
		rt = upd.RuntimeStateID
	}
	wait := nullStr(cur.WaitReason)
	if upd.SetWait {
		wait = nullStr(upd.WaitReason)
	}
	rec, recEv := nullStr(cur.RecoveryReason), cur.RecoveryEvidence
	if upd.SetRecovery {
		rec, recEv = nullStr(upd.RecoveryReason), upd.RecoveryEvidence
		if upd.RecoveryReason == "" {
			recEv = nil
		}
	}
	var evJSON []byte
	if recEv != nil {
		evJSON = jsonOf(recEv)
	}
	session := cur.CurrentSessionID
	if upd.SessionID != nil {
		session = upd.SessionID
	}
	col := map[execution.ExecutionStatus]string{
		execution.ExecutionCompleted: "completed_at", execution.ExecutionFailed: "failed_at",
		execution.ExecutionCancelled: "cancelled_at", execution.ExecutionPaused: "paused_at",
	}[status]
	set := ""
	if col != "" && status != cur.Status {
		set = ", " + col + " = COALESCE(" + col + ", $4)"
		if col == "paused_at" {
			set = ", paused_at = $4"
		}
	}
	tag, err := x.Exec(ctx, `UPDATE executions SET status = $3`+set+`, current_phase_id = $5, current_work_unit_id = $6,
		runtime_state_id = $7, wait_reason = $8, recovery_reason = $9, recovery_evidence = $10, current_session_id = $11,
		version = version + 1, updated_at = $4 WHERE id = $1 AND version = $2`,
		id, expectedVersion, string(status), now, phase, unit, rt, wait, rec, evJSON, session)
	if err != nil {
		return nil, TranslateDBError("update execution position", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	if status != cur.Status {
		st := cur.Stage
		reason := upd.Reason
		if reason == "" {
			reason = string(cur.Status) + " → " + string(status)
		}
		if err := insertStageRow(ctx, x, id, &st, st, status, reason, now); err != nil {
			return nil, err
		}
	}
	return r.execs.one(ctx, x, `id = $1`, id)
}

// ListSchedulable returns RUNNING / WAITING executions whose project is
// RUNNING with auto_continue enabled and whose driver lease is free or
// expired, least recently scheduled first (WBS 16.6, PLAN Decision 14).
func (r *EngineRepository) ListSchedulable(ctx context.Context, now time.Time, limit int) ([]persistence.SchedulableExecution, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.exec.Query(ctx, `SELECT e.id, e.project_id, e.last_scheduled_at, e.status FROM executions e
		JOIN projects p ON p.id = e.project_id
		JOIN project_settings ps ON ps.project_id = e.project_id
		WHERE e.status IN ('RUNNING', 'WAITING') AND p.status = 'RUNNING' AND ps.auto_continue
		  AND (e.driver_owner IS NULL OR e.driver_expires_at <= $1)
		ORDER BY e.last_scheduled_at NULLS FIRST, e.id LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, TranslateDBError("list schedulable executions", err)
	}
	defer rows.Close()
	var out []persistence.SchedulableExecution
	for rows.Next() {
		var s persistence.SchedulableExecution
		var st string
		if err := rows.Scan(&s.ExecutionID, &s.ProjectID, &s.LastScheduledAt, &st); err != nil {
			return nil, TranslateDBError("scan schedulable execution", err)
		}
		s.Status = execution.ExecutionStatus(st)
		out = append(out, s)
	}
	return out, rows.Err()
}

// TouchScheduled records a scheduling turn.
func (r *EngineRepository) TouchScheduled(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := r.exec.Exec(ctx, `UPDATE executions SET last_scheduled_at = $2 WHERE id = $1 AND status <> 'COMPLETED'`, id, now.UTC()); err != nil {
		return TranslateDBError("touch scheduled", err)
	}
	return nil
}

// ListEngineActive returns RUNNING / WAITING executions (restart recovery).
func (r *EngineRepository) ListEngineActive(ctx context.Context) ([]*persistence.ExecutionRecord, error) {
	return r.execs.many(ctx, `SELECT `+execCols+` FROM executions WHERE status IN ('RUNNING', 'WAITING') ORDER BY created_at`)
}

// ---------------------------------------------------------------------------
// Steps (P16-67).

const stepCols = `id, execution_id, project_id, seq, action, state_key, runtime_version, attempt, idempotency_key, status,
	phase_id, work_unit_id, dispatch_id, context_snapshot_id, worker_prompt_id, validation_run_id, checkpoint_id, plan_id,
	continuation_id, outcome, detail, error, intent_at, finished_at`

func scanStep(row interface{ Scan(dest ...any) error }) (*persistence.StepRecord, error) {
	var s persistence.StepRecord
	var action, status string
	var outcome *string
	var detail []byte
	if err := row.Scan(&s.ID, &s.ExecutionID, &s.ProjectID, &s.Seq, &action, &s.StateKey, &s.RuntimeVersion, &s.Attempt,
		&s.IdempotencyKey, &status, &s.PhaseID, &s.WorkUnitID, &s.DispatchID, &s.ContextSnapshotID, &s.WorkerPromptID,
		&s.ValidationRunID, &s.CheckpointID, &s.PlanID, &s.ContinuationID, &outcome, &detail, &s.Error, &s.IntentAt, &s.FinishedAt); err != nil {
		return nil, err
	}
	s.Action, s.Status = execution.StepAction(action), execution.StepStatus(status)
	if outcome != nil {
		s.Outcome = *outcome
	}
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &s.Detail)
	}
	return &s, nil
}

// AppendStepIntent inserts an INTENT row with the next sequence. An
// existing row with the same idempotency key is returned instead (false).
func (r *EngineRepository) AppendStepIntent(ctx context.Context, ex persistence.Executor, s *persistence.StepRecord) (*persistence.StepRecord, bool, error) {
	x := r.ex(ex)
	if existing, err := r.FindStepByKey(ctx, x, s.IdempotencyKey); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, false, err
	}
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.IntentAt.IsZero() {
		s.IntentAt = time.Now().UTC()
	}
	if s.Attempt < 1 {
		s.Attempt = 1
	}
	detail := s.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	err := x.QueryRow(ctx, `INSERT INTO execution_steps (id, execution_id, project_id, seq, action, state_key, runtime_version,
		attempt, idempotency_key, status, phase_id, work_unit_id, dispatch_id, context_snapshot_id, worker_prompt_id,
		validation_run_id, checkpoint_id, plan_id, continuation_id, detail, intent_at)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(seq), 0) + 1 FROM execution_steps WHERE execution_id = $2), $4, $5, $6, $7, $8,
		'INTENT', $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19) RETURNING seq`,
		s.ID, s.ExecutionID, s.ProjectID, string(s.Action), s.StateKey, s.RuntimeVersion, s.Attempt, s.IdempotencyKey,
		s.PhaseID, s.WorkUnitID, s.DispatchID, s.ContextSnapshotID, s.WorkerPromptID, s.ValidationRunID, s.CheckpointID,
		s.PlanID, s.ContinuationID, jsonOf(detail), s.IntentAt.UTC()).Scan(&s.Seq)
	if err != nil {
		return nil, false, TranslateDBError("append step intent", err)
	}
	s.Status = execution.StepIntent
	return s, true, nil
}

func refsSQL(refs persistence.StepRefs) []any {
	var detail []byte
	if refs.Detail != nil {
		detail = jsonOf(refs.Detail)
	}
	return []any{refs.DispatchID, refs.ContextSnapshotID, refs.WorkerPromptID, refs.ValidationRunID, refs.CheckpointID,
		refs.PlanID, refs.ContinuationID, detail}
}

const refsSet = `dispatch_id = COALESCE($2, dispatch_id), context_snapshot_id = COALESCE($3, context_snapshot_id),
	worker_prompt_id = COALESCE($4, worker_prompt_id), validation_run_id = COALESCE($5, validation_run_id),
	checkpoint_id = COALESCE($6, checkpoint_id), plan_id = COALESCE($7, plan_id),
	continuation_id = COALESCE($8, continuation_id), detail = COALESCE(detail || $9::jsonb, detail)`

// FinishStep records a step's result (INTENT → APPLIED / FAILED /
// ABANDONED); a finished step is immutable.
func (r *EngineRepository) FinishStep(ctx context.Context, ex persistence.Executor, id uuid.UUID, status execution.StepStatus, outcome, errMsg string, refs persistence.StepRefs) error {
	args := append([]any{id}, refsSQL(refs)...)
	args = append(args, string(status), nullStr(outcome), errMsg, time.Now().UTC())
	tag, err := r.ex(ex).Exec(ctx, `UPDATE execution_steps SET `+refsSet+`, status = $10, outcome = $11, error = $12,
		finished_at = $13 WHERE id = $1 AND status = 'INTENT'`, args...)
	if err != nil {
		return TranslateDBError("finish step", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// SetStepRefs records artifact references on an INTENT step.
func (r *EngineRepository) SetStepRefs(ctx context.Context, ex persistence.Executor, id uuid.UUID, refs persistence.StepRefs) error {
	args := append([]any{id}, refsSQL(refs)...)
	tag, err := r.ex(ex).Exec(ctx, `UPDATE execution_steps SET `+refsSet+` WHERE id = $1 AND status = 'INTENT'`, args...)
	if err != nil {
		return TranslateDBError("set step refs", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// GetStep returns a step by id.
func (r *EngineRepository) GetStep(ctx context.Context, id uuid.UUID) (*persistence.StepRecord, error) {
	s, err := scanStep(r.exec.QueryRow(ctx, `SELECT `+stepCols+` FROM execution_steps WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get step", err)
	}
	return s, nil
}

// FindStepByKey returns the step with the idempotency key.
func (r *EngineRepository) FindStepByKey(ctx context.Context, ex persistence.Executor, key string) (*persistence.StepRecord, error) {
	s, err := scanStep(r.ex(ex).QueryRow(ctx, `SELECT `+stepCols+` FROM execution_steps WHERE idempotency_key = $1`, key))
	if err != nil {
		return nil, TranslateDBError("find step", err)
	}
	return s, nil
}

// LatestStep returns the execution's highest-sequence step.
func (r *EngineRepository) LatestStep(ctx context.Context, ex persistence.Executor, executionID uuid.UUID) (*persistence.StepRecord, error) {
	s, err := scanStep(r.ex(ex).QueryRow(ctx, `SELECT `+stepCols+` FROM execution_steps WHERE execution_id = $1 ORDER BY seq DESC LIMIT 1`, executionID))
	if err != nil {
		return nil, TranslateDBError("latest step", err)
	}
	return s, nil
}

func (r *EngineRepository) steps(ctx context.Context, sql string, args ...any) ([]*persistence.StepRecord, error) {
	rows, err := r.exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("list steps", err)
	}
	defer rows.Close()
	var out []*persistence.StepRecord
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, TranslateDBError("scan step", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListPendingSteps returns the execution's INTENT steps in order.
func (r *EngineRepository) ListPendingSteps(ctx context.Context, executionID uuid.UUID) ([]*persistence.StepRecord, error) {
	return r.steps(ctx, `SELECT `+stepCols+` FROM execution_steps WHERE execution_id = $1 AND status = 'INTENT' ORDER BY seq`, executionID)
}

// ListSteps pages the execution's steps in sequence order.
func (r *EngineRepository) ListSteps(ctx context.Context, executionID uuid.UUID, limit, offset int) ([]*persistence.StepRecord, int, error) {
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM execution_steps WHERE execution_id = $1`, executionID).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count steps", err)
	}
	switch {
	case limit < 0:
		limit = 1 << 30 // every step
	case limit == 0:
		limit = 100
	}
	out, err := r.steps(ctx, `SELECT `+stepCols+` FROM execution_steps WHERE execution_id = $1 ORDER BY seq LIMIT $2 OFFSET $3`, executionID, limit, offset)
	return out, total, err
}

// ---------------------------------------------------------------------------
// Events (P16-68).

// ListTimeline returns an execution's events in exec_seq order.
func (r *EngineRepository) ListTimeline(ctx context.Context, executionID uuid.UUID) ([]execution.Event, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, project_id, execution_id, type, payload, COALESCE(exec_seq, 0), created_at
		FROM execution_events WHERE execution_id = $1 ORDER BY exec_seq NULLS FIRST, seq`, executionID)
	if err != nil {
		return nil, TranslateDBError("list timeline", err)
	}
	defer rows.Close()
	out := []execution.Event{}
	for rows.Next() {
		var e execution.Event
		var typ string
		var payload []byte
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ExecutionID, &typ, &payload, &e.ExecSeq, &e.CreatedAt); err != nil {
			return nil, TranslateDBError("scan timeline", err)
		}
		e.Type = execution.EventType(typ)
		_ = json.Unmarshal(payload, &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func statusList(ss []wbs.WorkUnitStatus) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

var _ = strings.Join
