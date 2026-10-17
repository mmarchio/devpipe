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
	"overseer/internal/persistence"
)

// ExecutionRepository implements persistence.ExecutionRepository (P15-31 ..
// P15-38).
type ExecutionRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.ExecutionRepository = (*ExecutionRepository)(nil)

// NewExecutionRepository binds the repository to a pool.
func NewExecutionRepository(pool *pgxpool.Pool) *ExecutionRepository {
	return &ExecutionRepository{exec: poolAdapter{pool}, pool: pool}
}

func (r *ExecutionRepository) ex(e persistence.Executor) executor {
	if e == nil {
		return r.exec
	}
	return e
}

// ---------------------------------------------------------------------------
// Settings (P15-31).

// GetSettings returns stored settings or defaults (false).
func (r *ExecutionRepository) GetSettings(ctx context.Context, projectID uuid.UUID) (execution.Settings, bool, error) {
	var s execution.Settings
	var policy string
	var lease int
	err := r.exec.QueryRow(ctx, `SELECT require_git_repository, dirty_policy, materialize_state_files,
		max_bootstrap_control_transitions, bootstrap_lease_seconds FROM project_execution_settings WHERE project_id = $1`, projectID).
		Scan(&s.RequireGitRepository, &policy, &s.MaterializeStateFiles, &s.MaxBootstrapControlTransitions, &lease)
	if err != nil {
		if errors.Is(TranslateDBError("get execution settings", err), persistence.ErrNotFound) {
			return execution.DefaultSettings(), false, nil
		}
		return execution.Settings{}, false, TranslateDBError("get execution settings", err)
	}
	s.DirtyPolicy, s.BootstrapLease = execution.DirtyPolicy(policy), time.Duration(lease)*time.Second
	return s, true, nil
}

// UpsertSettings stores settings.
func (r *ExecutionRepository) UpsertSettings(ctx context.Context, projectID uuid.UUID, s execution.Settings) error {
	_, err := r.exec.Exec(ctx, `INSERT INTO project_execution_settings (project_id, require_git_repository, dirty_policy,
		materialize_state_files, max_bootstrap_control_transitions, bootstrap_lease_seconds, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (project_id) DO UPDATE SET require_git_repository = EXCLUDED.require_git_repository,
		  dirty_policy = EXCLUDED.dirty_policy, materialize_state_files = EXCLUDED.materialize_state_files,
		  max_bootstrap_control_transitions = EXCLUDED.max_bootstrap_control_transitions,
		  bootstrap_lease_seconds = EXCLUDED.bootstrap_lease_seconds, updated_at = now()`,
		projectID, s.RequireGitRepository, string(s.DirtyPolicy), s.MaterializeStateFiles, s.MaxBootstrapControlTransitions,
		int(s.BootstrapLease/time.Second))
	if err != nil {
		return TranslateDBError("upsert execution settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Executions (P15-32 .. P15-34).

var execColNames = []string{"id", "project_id", "number", "status", "generation", "current_session_id", "state_machine_id",
	"state_machine_version", "state_machine_hash", "initial_phase_id", "configuration_snapshot_id", "wbs_version", "wbs_hash",
	"plan_id", "plan_state", "initial_state_key", "first_actionable_state_id", "first_actionable_state_key", "initial_role_key",
	"initial_work_unit_id", "initial_template_key", "initial_context_snapshot_id", "initial_worker_prompt_id", "initial_cache_key",
	"initial_canonical_key", "plan_hash", "bootstrap_stage", "failed_stage", "failure_code", "failure_class", "failure_reason",
	"claim_owner", "claim_expires_at", "started_at", "ready_at", "running_at", "completed_at", "failed_at", "cancelled_at",
	"version", "created_at", "updated_at",
	"current_phase_id", "current_work_unit_id", "runtime_state_id", "driver_owner", "driver_expires_at", "driver_epoch",
	"last_scheduled_at", "wait_reason", "recovery_reason", "recovery_evidence", "paused_at", "finalization_generation",
	"app_version", "schema_version", "app_config_hash"}

var execCols = strings.Join(execColNames, ", ")

func scanExecution(row interface{ Scan(dest ...any) error }) (*persistence.ExecutionRecord, error) {
	var e persistence.ExecutionRecord
	var status, planState, stage string
	var snapID *uuid.UUID
	var faKey, cacheKey, canonKey, failedStage, failureCode, failureClass, claimOwner *string
	var driverOwner, waitReason, recoveryReason, appVersion, appConfigHash *string
	var schemaVersion *int
	var evidence []byte
	if err := row.Scan(&e.ID, &e.ProjectID, &e.Number, &status, &e.Generation, &e.CurrentSessionID, &e.StateMachineID,
		&e.StateMachineVersion, &e.StateMachineHash, &e.InitialPhaseID, &snapID, &e.WBSVersion, &e.WBSHash,
		&e.PlanID, &planState, &e.InitialStateKey, &e.FirstActionableStateID, &faKey, &e.InitialRoleKey,
		&e.InitialWorkUnitID, &e.InitialTemplateKey, &e.InitialContextSnapshotID, &e.InitialWorkerPromptID, &cacheKey,
		&canonKey, &e.PlanHash, &stage, &failedStage, &failureCode, &failureClass, &e.FailureReason,
		&claimOwner, &e.ClaimExpiresAt, &e.StartedAt, &e.ReadyAt, &e.RunningAt, &e.CompletedAt, &e.FailedAt, &e.CancelledAt,
		&e.Version, &e.CreatedAt, &e.UpdatedAt,
		&e.CurrentPhaseID, &e.CurrentWorkUnitID, &e.RuntimeStateID, &driverOwner, &e.DriverExpiresAt, &e.DriverEpoch,
		&e.LastScheduledAt, &waitReason, &recoveryReason, &evidence, &e.PausedAt, &e.FinalizationGeneration,
		&appVersion, &schemaVersion, &appConfigHash); err != nil {
		return nil, err
	}
	if len(evidence) > 0 {
		_ = json.Unmarshal(evidence, &e.RecoveryEvidence)
	}
	e.Status, e.PlanState, e.Stage = execution.ExecutionStatus(status), execution.PlanState(planState), execution.BootstrapStage(stage)
	if snapID != nil {
		e.ConfigurationSnapshotID = *snapID
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	e.FirstActionableStateKey, e.InitialCacheKey, e.InitialCanonicalKey = deref(faKey), deref(cacheKey), deref(canonKey)
	e.FailedStage = execution.BootstrapStage(deref(failedStage))
	e.FailureCode = execution.BootstrapErrorCode(deref(failureCode))
	e.FailureClass = execution.FailureClass(deref(failureClass))
	e.ClaimOwner = deref(claimOwner)
	e.DriverOwner, e.WaitReason, e.RecoveryReason = deref(driverOwner), deref(waitReason), deref(recoveryReason)
	e.AppVersion, e.AppConfigHash = deref(appVersion), deref(appConfigHash)
	if schemaVersion != nil {
		e.SchemaVersion = *schemaVersion
	}
	return &e, nil
}

// LockProject takes the project-scoped bootstrap advisory lock (WBS 15.8).
func (r *ExecutionRepository) LockProject(ctx context.Context, ex persistence.Executor, projectID uuid.UUID) error {
	if _, err := r.ex(ex).Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('execution-bootstrap:' || $1::text))`, projectID); err != nil {
		return TranslateDBError("lock project bootstrap", err)
	}
	return nil
}

func insertStageRow(ctx context.Context, ex executor, id uuid.UUID, from *execution.BootstrapStage, to execution.BootstrapStage,
	status execution.ExecutionStatus, reason string, at time.Time) error {
	var f *string
	if from != nil {
		s := string(*from)
		f = &s
	}
	_, err := ex.Exec(ctx, `INSERT INTO execution_bootstrap_transitions (id, execution_id, sequence, from_stage, to_stage, status, reason, created_at)
		SELECT $1, $2, COALESCE(MAX(sequence), 0) + 1, $3, $4, $5, $6, $7 FROM execution_bootstrap_transitions WHERE execution_id = $2`,
		uuid.New(), id, f, string(to), string(status), reason, at.UTC())
	if err != nil {
		return TranslateDBError("insert bootstrap transition", err)
	}
	return nil
}

// Create allocates the next project-local number (never reused, WBS 15.5)
// and inserts the execution at stage SNAPSHOTTING with the history
// NULL → VALIDATING → SNAPSHOTTING.
func (r *ExecutionRepository) Create(ctx context.Context, ex persistence.Executor, e *persistence.ExecutionRecord) error {
	x := r.ex(ex)
	if err := x.QueryRow(ctx, `UPDATE projects SET execution_seq = execution_seq + 1 WHERE id = $1 RETURNING execution_seq`,
		e.ProjectID).Scan(&e.Number); err != nil {
		return TranslateDBError("allocate execution number", err)
	}
	now := time.Now().UTC()
	if e.StartedAt.IsZero() {
		e.StartedAt = now
	}
	e.CreatedAt, e.UpdatedAt = now, now
	e.Stage = execution.StageSnapshotting
	if e.Version == 0 {
		e.Version = 1
	}
	_, err := x.Exec(ctx, `INSERT INTO executions (id, project_id, number, status, generation, current_session_id, state_machine_id,
		state_machine_version, state_machine_hash, initial_phase_id, wbs_version, wbs_hash, plan_id, plan_state,
		initial_state_key, first_actionable_state_key, initial_role_key, initial_work_unit_id, initial_template_key, plan_hash,
		bootstrap_stage, started_at, version, created_at, updated_at, configuration_snapshot_id, claim_owner, claim_expires_at,
		app_version, schema_version, app_config_hash)
		VALUES ($1, $2, $3, $4, $5, NULL, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $23, NULL, $24, $25,
		$26, $27, $28)`,
		e.ID, e.ProjectID, e.Number, string(e.Status), e.Generation, e.StateMachineID, e.StateMachineVersion, e.StateMachineHash,
		e.InitialPhaseID, e.WBSVersion, e.WBSHash, e.PlanID, string(e.PlanState), e.InitialStateKey, nullStr(e.FirstActionableStateKey),
		e.InitialRoleKey, e.InitialWorkUnitID, e.InitialTemplateKey, e.PlanHash, string(e.Stage), e.StartedAt.UTC(), e.Version, now,
		nullStr(e.ClaimOwner), utcPtr(e.ClaimExpiresAt), nullStr(e.AppVersion), nullInt(e.SchemaVersion), nullStr(e.AppConfigHash))
	if err != nil {
		return TranslateDBError("insert execution", err)
	}
	v := execution.StageValidating
	if err := insertStageRow(ctx, x, e.ID, nil, execution.StageValidating, e.Status, "bootstrap requested", now); err != nil {
		return err
	}
	return insertStageRow(ctx, x, e.ID, &v, execution.StageSnapshotting, e.Status, "readiness validated", now)
}

// nullInt maps 0 to NULL.
func nullInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r *ExecutionRepository) one(ctx context.Context, ex executor, where string, args ...any) (*persistence.ExecutionRecord, error) {
	e, err := scanExecution(ex.QueryRow(ctx, `SELECT `+execCols+` FROM executions WHERE `+where, args...))
	if err != nil {
		return nil, TranslateDBError("get execution", err)
	}
	return e, nil
}

// Get returns a project-scoped execution.
func (r *ExecutionRepository) Get(ctx context.Context, ex persistence.Executor, projectID, id uuid.UUID) (*persistence.ExecutionRecord, error) {
	return r.one(ctx, r.ex(ex), `id = $1 AND project_id = $2`, id, projectID)
}

// GetByID returns an execution by id.
func (r *ExecutionRepository) GetByID(ctx context.Context, ex persistence.Executor, id uuid.UUID) (*persistence.ExecutionRecord, error) {
	return r.one(ctx, r.ex(ex), `id = $1`, id)
}

const activeSQL = `('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED')`

// GetActive returns the project's active execution (ErrNotFound when none).
func (r *ExecutionRepository) GetActive(ctx context.Context, ex persistence.Executor, projectID uuid.UUID) (*persistence.ExecutionRecord, error) {
	return r.one(ctx, r.ex(ex), `project_id = $1 AND status IN `+activeSQL, projectID)
}

func (r *ExecutionRepository) many(ctx context.Context, sql string, args ...any) ([]*persistence.ExecutionRecord, error) {
	rows, err := r.exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("list executions", err)
	}
	defer rows.Close()
	var out []*persistence.ExecutionRecord
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, TranslateDBError("scan execution", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list executions", err)
	}
	return out, nil
}

// ListByProject lists newest first with the total count.
func (r *ExecutionRepository) ListByProject(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]*persistence.ExecutionRecord, int, error) {
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM executions WHERE project_id = $1`, projectID).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count executions", err)
	}
	if limit <= 0 {
		limit = 50
	}
	out, err := r.many(ctx, `SELECT `+execCols+` FROM executions WHERE project_id = $1 ORDER BY number DESC LIMIT $2 OFFSET $3`,
		projectID, limit, offset)
	return out, total, err
}

// AdvanceStage is a versioned CAS of the bootstrap stage plus a history row
// (P15-33). A stale version is persistence.ErrConflict.
func (r *ExecutionRepository) AdvanceStage(ctx context.Context, ex persistence.Executor, id uuid.UUID, expectedVersion int,
	to execution.BootstrapStage, reason string, mut *persistence.ExecutionMutation) (*persistence.ExecutionRecord, error) {
	x := r.ex(ex)
	cur, err := r.one(ctx, x, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if cur.Version != expectedVersion {
		return nil, persistence.ErrConflict
	}
	if err := execution.ValidateStageAdvance(cur.Stage, to, cur.FailedStage); err != nil {
		return nil, err
	}
	if mut == nil {
		mut = &persistence.ExecutionMutation{}
	}
	now := time.Now().UTC()
	clear := mut.ClearFailure || cur.Stage == execution.StageFailed
	tag, err := x.Exec(ctx, `UPDATE executions SET bootstrap_stage = $3,
		first_actionable_state_id = COALESCE($4, first_actionable_state_id),
		first_actionable_state_key = COALESCE($5, first_actionable_state_key),
		initial_context_snapshot_id = COALESCE($6, initial_context_snapshot_id),
		initial_worker_prompt_id = COALESCE($7, initial_worker_prompt_id),
		initial_cache_key = COALESCE($8, initial_cache_key),
		initial_canonical_key = COALESCE($9, initial_canonical_key),
		failed_stage = CASE WHEN $10 THEN NULL ELSE failed_stage END,
		failure_code = CASE WHEN $10 THEN NULL ELSE failure_code END,
		failure_class = CASE WHEN $10 THEN NULL ELSE failure_class END,
		failure_reason = CASE WHEN $10 THEN '' ELSE failure_reason END,
		version = version + 1, updated_at = $11
		WHERE id = $1 AND version = $2`,
		id, expectedVersion, string(to), mut.FirstActionableStateID, mut.FirstActionableStateKey, mut.InitialContextSnapshotID,
		mut.InitialWorkerPromptID, mut.InitialCacheKey, mut.InitialCanonicalKey, clear, now)
	if err != nil {
		return nil, TranslateDBError("advance execution stage", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	from := cur.Stage
	if err := insertStageRow(ctx, x, id, &from, to, cur.Status, reason, now); err != nil {
		return nil, err
	}
	return r.one(ctx, x, `id = $1`, id)
}

// UpdateStatus is a versioned status CAS (ValidateTransition) with a
// history row recording the status stage (WBS 15.83).
func (r *ExecutionRepository) UpdateStatus(ctx context.Context, ex persistence.Executor, id uuid.UUID, expectedVersion int,
	to execution.ExecutionStatus, reason string) (*persistence.ExecutionRecord, error) {
	x := r.ex(ex)
	cur, err := r.one(ctx, x, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if cur.Version != expectedVersion {
		return nil, persistence.ErrConflict
	}
	if err := execution.ValidateTransition(cur.Status, to); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	col := map[execution.ExecutionStatus]string{
		execution.ExecutionReady: "ready_at", execution.ExecutionRunning: "running_at", execution.ExecutionCompleted: "completed_at",
		execution.ExecutionFailed: "failed_at", execution.ExecutionCancelled: "cancelled_at",
	}[to]
	set := ""
	if col != "" {
		set = ", " + col + " = COALESCE(" + col + ", $4)"
	}
	tag, err := x.Exec(ctx, `UPDATE executions SET status = $3`+set+`, version = version + 1, updated_at = $4
		WHERE id = $1 AND version = $2`, id, expectedVersion, string(to), now)
	if err != nil {
		return nil, TranslateDBError("update execution status", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	st := cur.Stage
	if err := insertStageRow(ctx, x, id, &st, st, to, reason, now); err != nil {
		return nil, err
	}
	return r.one(ctx, x, `id = $1`, id)
}

// RecordFailure moves the stage to FAILED, persisting the failed stage and
// classification (WBS 15.86); terminal additionally fails the execution.
func (r *ExecutionRepository) RecordFailure(ctx context.Context, ex persistence.Executor, id uuid.UUID, expectedVersion int,
	failedStage execution.BootstrapStage, code execution.BootstrapErrorCode, class execution.FailureClass, reason string, terminal bool) (*persistence.ExecutionRecord, error) {
	x := r.ex(ex)
	cur, err := r.one(ctx, x, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if cur.Version != expectedVersion {
		return nil, persistence.ErrConflict
	}
	if cur.Stage == execution.StageComplete {
		return nil, execution.ErrIllegalStage
	}
	if failedStage == "" || failedStage == execution.StageFailed {
		failedStage = cur.Stage
		if failedStage == execution.StageFailed {
			failedStage = cur.FailedStage
		}
	}
	status := cur.Status
	if terminal {
		if err := execution.ValidateTransition(cur.Status, execution.ExecutionFailed); err != nil {
			return nil, err
		}
		status = execution.ExecutionFailed
	}
	now := time.Now().UTC()
	tag, err := x.Exec(ctx, `UPDATE executions SET bootstrap_stage = 'FAILED', failed_stage = $3, failure_code = $4, failure_class = $5,
		failure_reason = $6, status = $7, failed_at = CASE WHEN $7 = 'FAILED' THEN $8 ELSE failed_at END,
		claim_owner = NULL, claim_expires_at = NULL, version = version + 1, updated_at = $8
		WHERE id = $1 AND version = $2`, id, expectedVersion, string(failedStage), string(code), string(class), reason, string(status), now)
	if err != nil {
		return nil, TranslateDBError("record execution failure", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	from := cur.Stage
	if err := insertStageRow(ctx, x, id, &from, execution.StageFailed, status, string(code)+": "+reason, now); err != nil {
		return nil, err
	}
	return r.one(ctx, x, `id = $1`, id)
}

// ListStageHistory lists the bootstrap history chronologically.
func (r *ExecutionRepository) ListStageHistory(ctx context.Context, executionID uuid.UUID) ([]persistence.StageTransition, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, execution_id, sequence, from_stage, to_stage, status, reason, created_at
		FROM execution_bootstrap_transitions WHERE execution_id = $1 ORDER BY sequence`, executionID)
	if err != nil {
		return nil, TranslateDBError("list bootstrap transitions", err)
	}
	defer rows.Close()
	out := []persistence.StageTransition{}
	for rows.Next() {
		var t persistence.StageTransition
		var from *string
		var to, status string
		if err := rows.Scan(&t.ID, &t.ExecutionID, &t.Sequence, &from, &to, &status, &t.Reason, &t.CreatedAt); err != nil {
			return nil, TranslateDBError("scan bootstrap transition", err)
		}
		if from != nil {
			f := execution.BootstrapStage(*from)
			t.FromStage = &f
		}
		t.ToStage, t.Status = execution.BootstrapStage(to), execution.ExecutionStatus(status)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Claim takes the bootstrap claim when unowned, expired or already ours
// (WBS 15.8).
func (r *ExecutionRepository) Claim(ctx context.Context, id uuid.UUID, owner string, now time.Time, ttl time.Duration) (bool, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE executions SET claim_owner = $2, claim_expires_at = $3
		WHERE id = $1 AND status IN ('INITIALIZING', 'READY')
		  AND (claim_owner IS NULL OR claim_owner = $2 OR claim_expires_at IS NULL OR claim_expires_at <= $4)`,
		id, owner, now.Add(ttl).UTC(), now.UTC())
	if err != nil {
		return false, TranslateDBError("claim execution", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Release drops our claim.
func (r *ExecutionRepository) Release(ctx context.Context, id uuid.UUID, owner string) error {
	_, err := r.exec.Exec(ctx, `UPDATE executions SET claim_owner = NULL, claim_expires_at = NULL WHERE id = $1 AND claim_owner = $2`, id, owner)
	if err != nil {
		return TranslateDBError("release execution", err)
	}
	return nil
}

// ListRecoverable lists interrupted bootstraps (INITIALIZING, stage not
// FAILED, claim free/expired) and READY executions of READY projects.
func (r *ExecutionRepository) ListRecoverable(ctx context.Context, now time.Time) ([]*persistence.ExecutionRecord, error) {
	return r.many(ctx, `SELECT `+execCols+` FROM executions e WHERE
		((e.status = 'INITIALIZING' AND e.bootstrap_stage <> 'FAILED')
		  OR (e.status = 'READY' AND EXISTS (SELECT 1 FROM projects p WHERE p.id = e.project_id AND p.status = 'READY')))
		AND (e.claim_owner IS NULL OR e.claim_expires_at IS NULL OR e.claim_expires_at <= $1)
		ORDER BY e.created_at`, now.UTC())
}

// ---------------------------------------------------------------------------
// Configuration snapshots, baselines, checkpoints (P15-35, P15-36).

// CreateSnapshot inserts the snapshot and sets the execution reference.
func (r *ExecutionRepository) CreateSnapshot(ctx context.Context, ex persistence.Executor, s *persistence.ConfigurationSnapshotRecord) error {
	x := r.ex(ex)
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if _, err := x.Exec(ctx, `INSERT INTO execution_configuration_snapshots (id, execution_id, project_id, project_version, canonical, hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.ID, s.ExecutionID, s.ProjectID, s.ProjectVersion, string(s.Canonical), s.Hash, s.CreatedAt.UTC()); err != nil {
		return TranslateDBError("insert configuration snapshot", err)
	}
	if _, err := x.Exec(ctx, `UPDATE executions SET configuration_snapshot_id = $2 WHERE id = $1`, s.ExecutionID, s.ID); err != nil {
		return TranslateDBError("bind configuration snapshot", err)
	}
	return nil
}

// GetSnapshot returns an execution's configuration snapshot.
func (r *ExecutionRepository) GetSnapshot(ctx context.Context, executionID uuid.UUID) (*persistence.ConfigurationSnapshotRecord, error) {
	var s persistence.ConfigurationSnapshotRecord
	var canonical string
	err := r.exec.QueryRow(ctx, `SELECT id, execution_id, project_id, project_version, canonical, hash, created_at
		FROM execution_configuration_snapshots WHERE execution_id = $1`, executionID).
		Scan(&s.ID, &s.ExecutionID, &s.ProjectID, &s.ProjectVersion, &canonical, &s.Hash, &s.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("get configuration snapshot", err)
	}
	s.Canonical = []byte(canonical)
	return &s, nil
}

// CreateBaseline inserts a baseline once per (execution, kind).
func (r *ExecutionRepository) CreateBaseline(ctx context.Context, b *persistence.BaselineRecord) (*persistence.BaselineRecord, error) {
	if b.CapturedAt.IsZero() {
		b.CapturedAt = time.Now().UTC()
	}
	_, err := r.exec.Exec(ctx, `INSERT INTO execution_repository_baselines (id, execution_id, kind, working_directory, is_git, head_commit,
		branch, dirty, fingerprint, hash, captured_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (execution_id, kind) DO NOTHING`,
		b.ID, b.ExecutionID, string(b.Kind), b.Snapshot.WorkingDirectory, b.Snapshot.IsGitRepository, b.Snapshot.HeadCommit,
		b.Snapshot.Branch, b.Snapshot.Dirty, b.Snapshot.Fingerprint, b.Hash, b.CapturedAt.UTC())
	if err != nil {
		return nil, TranslateDBError("insert repository baseline", err)
	}
	return r.GetBaseline(ctx, b.ExecutionID, b.Kind)
}

// GetBaseline returns one baseline.
func (r *ExecutionRepository) GetBaseline(ctx context.Context, executionID uuid.UUID, kind persistence.BaselineKind) (*persistence.BaselineRecord, error) {
	var b persistence.BaselineRecord
	var k string
	err := r.exec.QueryRow(ctx, `SELECT id, execution_id, kind, working_directory, is_git, head_commit, branch, dirty, fingerprint, hash, captured_at
		FROM execution_repository_baselines WHERE execution_id = $1 AND kind = $2`, executionID, string(kind)).
		Scan(&b.ID, &b.ExecutionID, &k, &b.Snapshot.WorkingDirectory, &b.Snapshot.IsGitRepository, &b.Snapshot.HeadCommit,
			&b.Snapshot.Branch, &b.Snapshot.Dirty, &b.Snapshot.Fingerprint, &b.Hash, &b.CapturedAt)
	if err != nil {
		return nil, TranslateDBError("get repository baseline", err)
	}
	b.Kind = persistence.BaselineKind(k)
	return &b, nil
}

// CreateCheckpoint inserts the checkpoint.
func (r *ExecutionRepository) CreateCheckpoint(ctx context.Context, ex persistence.Executor, c *persistence.CheckpointRecord) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.Checkpoint.Normalize()
	art, err := json.Marshal(c.Checkpoint)
	if err != nil {
		return err
	}
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO execution_bootstrap_checkpoints (id, execution_id, stage, artifacts, checkpoint_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, c.ID, c.Checkpoint.ExecutionID, execution.CheckpointStage, art, c.Hash, c.CreatedAt.UTC()); err != nil {
		return TranslateDBError("insert bootstrap checkpoint", err)
	}
	return nil
}

// GetCheckpoint returns an execution's checkpoint.
func (r *ExecutionRepository) GetCheckpoint(ctx context.Context, executionID uuid.UUID) (*persistence.CheckpointRecord, error) {
	var c persistence.CheckpointRecord
	var art []byte
	err := r.exec.QueryRow(ctx, `SELECT id, artifacts, checkpoint_hash, created_at FROM execution_bootstrap_checkpoints WHERE execution_id = $1`,
		executionID).Scan(&c.ID, &art, &c.Hash, &c.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("get bootstrap checkpoint", err)
	}
	if err := json.Unmarshal(art, &c.Checkpoint); err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Events (P15-37).

// AppendEvent inserts one event.
func (r *ExecutionRepository) AppendEvent(ctx context.Context, ex persistence.Executor, e *execution.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	insert := func(x executor) error {
		// Per-project sequence (WBS 17.76, P17-28): assigned under a
		// project-scoped advisory transaction lock held to commit, so
		// project_seq is gapless and commit-ordered — the SSE event id.
		if _, err := x.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('project-events:' || $1::text))`, e.ProjectID); err != nil {
			return TranslateDBError("lock project events", err)
		}
		if e.ExecutionID != nil {
			// Per-execution sequence (WBS 16.82): assigned under an
			// execution-scoped advisory lock; the unique constraint is the
			// backstop.
			if _, err := x.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('execution-events:' || $1::text))`, *e.ExecutionID); err != nil {
				return TranslateDBError("lock execution events", err)
			}
		}
		if err := x.QueryRow(ctx, `INSERT INTO execution_events (id, project_id, execution_id, type, payload, created_at, exec_seq, project_seq)
			VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $3::uuid IS NULL THEN NULL ELSE
			  (SELECT COALESCE(MAX(exec_seq), 0) + 1 FROM execution_events WHERE execution_id = $3) END,
			  (SELECT COALESCE(MAX(project_seq), 0) + 1 FROM execution_events WHERE project_id = $2))
			RETURNING COALESCE(exec_seq, 0), project_seq`, e.ID, e.ProjectID, e.ExecutionID, string(e.Type), b, e.CreatedAt.UTC()).Scan(&e.ExecSeq, &e.ProjectSeq); err != nil {
			return TranslateDBError("insert execution event", err)
		}
		return nil
	}
	if ex != nil || r.pool == nil {
		return insert(r.ex(ex))
	}
	return Run(ctx, r.pool, func(tx persistence.Transaction) error { return insert(txExecutor{tx}) })
}

// ListEvents lists a project's events (optionally one execution's)
// chronologically.
func (r *ExecutionRepository) ListEvents(ctx context.Context, projectID uuid.UUID, executionID *uuid.UUID) ([]execution.Event, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, project_id, execution_id, type, payload, created_at FROM execution_events
		WHERE project_id = $1 AND ($2::uuid IS NULL OR execution_id = $2) ORDER BY seq`, projectID, executionID)
	if err != nil {
		return nil, TranslateDBError("list execution events", err)
	}
	defer rows.Close()
	out := []execution.Event{}
	for rows.Next() {
		var e execution.Event
		var typ string
		var payload []byte
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ExecutionID, &typ, &payload, &e.CreatedAt); err != nil {
			return nil, TranslateDBError("scan execution event", err)
		}
		e.Type = execution.EventType(typ)
		_ = json.Unmarshal(payload, &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Runtime binding (P15-38).

// GetRuntimeByExecution returns the runtime bound to the execution;
// ErrNotFound when the project's runtime is bound elsewhere (WBS 15.55).
func (r *ExecutionRepository) GetRuntimeByExecution(ctx context.Context, ex persistence.Executor, executionID uuid.UUID) (*persistence.RuntimeStateRecord, error) {
	rs, err := scanRuntimeState(r.ex(ex).QueryRow(ctx, `SELECT `+runtimeStateColumns+` FROM runtime_state WHERE execution_id = $1`, executionID))
	if err != nil {
		return nil, TranslateDBError("get runtime by execution", err)
	}
	return rs, nil
}

// BindRuntime creates or binds the project's single runtime row
// (PLAN Decision 7): a row already bound to this execution is reused
// unchanged; an unbound row or one bound to another (terminal) execution
// is rebound and reset to rec's position; none → created.
func (r *ExecutionRepository) BindRuntime(ctx context.Context, ex persistence.Executor, rec *persistence.RuntimeStateRecord) (*persistence.RuntimeStateRecord, error) {
	x := r.ex(ex)
	if rec.ExecutionID == nil {
		return nil, errors.New("dbrepo: bind runtime requires an execution")
	}
	cur, err := scanRuntimeState(x.QueryRow(ctx, `SELECT `+runtimeStateColumns+` FROM runtime_state WHERE project_id = $1 FOR UPDATE`, rec.ProjectID))
	now := time.Now().UTC()
	if err != nil {
		if !errors.Is(TranslateDBError("get runtime", err), persistence.ErrNotFound) {
			return nil, TranslateDBError("get runtime", err)
		}
		if rec.ID == uuid.Nil {
			rec.ID = uuid.New()
		}
		rec.Version, rec.EnteredAt, rec.CreatedAt, rec.UpdatedAt = 1, now, now, now
		if _, err := x.Exec(ctx, `INSERT INTO runtime_state (id, project_id, state_machine_id, state_machine_version,
			current_state_id, phase_id, work_unit_id, entered_at, version, created_at, updated_at, execution_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $8, $8, $9)`,
			rec.ID, rec.ProjectID, rec.StateMachineID, rec.StateMachineVersion, rec.CurrentStateID, rec.PhaseID, rec.WorkUnitID, now, rec.ExecutionID); err != nil {
			return nil, TranslateDBError("create runtime state", err)
		}
		return rec, nil
	}
	if cur.ExecutionID != nil && *cur.ExecutionID == *rec.ExecutionID {
		return cur, nil
	}
	if _, err := x.Exec(ctx, `UPDATE runtime_state SET execution_id = $2, state_machine_id = $3, state_machine_version = $4,
		current_state_id = $5, phase_id = $6, work_unit_id = $7, entered_at = $8, version = version + 1, updated_at = $8
		WHERE id = $1`, cur.ID, rec.ExecutionID, rec.StateMachineID, rec.StateMachineVersion, rec.CurrentStateID, rec.PhaseID, rec.WorkUnitID, now); err != nil {
		return nil, TranslateDBError("rebind runtime state", err)
	}
	return scanRuntimeState(x.QueryRow(ctx, `SELECT `+runtimeStateColumns+` FROM runtime_state WHERE id = $1`, cur.ID))
}

// AppendHistory inserts a history row with the next per-project sequence
// under the history advisory lock; must run inside a transaction.
func (r *ExecutionRepository) AppendHistory(ctx context.Context, ex persistence.Executor, rec *persistence.TransitionHistoryRecord) error {
	x := r.ex(ex)
	if _, err := x.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, rec.ProjectID); err != nil {
		return TranslateDBError("transition history sequence lock", err)
	}
	var next int
	if err := x.QueryRow(ctx, `UPDATE projects SET state_entry_seq = state_entry_seq + 1 WHERE id = $1 RETURNING state_entry_seq`,
		rec.ProjectID).Scan(&next); err != nil {
		return TranslateDBError("advance transition history sequence", err)
	}
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	var from *uuid.UUID
	if rec.FromStateID != uuid.Nil {
		f := rec.FromStateID
		from = &f
	}
	if _, err := x.Exec(ctx, `INSERT INTO state_transition_history (id, project_id, state_machine_id, state_machine_version,
		from_state_id, to_state_id, outcome, phase_id, work_unit_id, runtime_version, seq, created_at, execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rec.ID, rec.ProjectID, rec.StateMachineID, rec.StateMachineVersion, from, rec.ToStateID, rec.Outcome, rec.PhaseID,
		rec.WorkUnitID, rec.RuntimeVersion, next, rec.CreatedAt.UTC(), rec.ExecutionID); err != nil {
		return TranslateDBError("append transition history", err)
	}
	rec.Seq = next
	return nil
}

// ListExecutionHistory lists an execution's runtime history in seq order.
func (r *ExecutionRepository) ListExecutionHistory(ctx context.Context, executionID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+transitionHistorySelect+` FROM state_transition_history WHERE execution_id = $1 ORDER BY seq`, executionID)
	if err != nil {
		return nil, TranslateDBError("list execution history", err)
	}
	defer rows.Close()
	var out []*persistence.TransitionHistoryRecord
	for rows.Next() {
		rec, err := scanTransitionHistoryRow(rows)
		if err != nil {
			return nil, TranslateDBError("scan execution history", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetArtifacts CASes the artifact columns without a stage change.
func (r *ExecutionRepository) SetArtifacts(ctx context.Context, ex persistence.Executor, id uuid.UUID, expectedVersion int, mut *persistence.ExecutionMutation) (*persistence.ExecutionRecord, error) {
	x := r.ex(ex)
	if mut == nil {
		mut = &persistence.ExecutionMutation{}
	}
	tag, err := x.Exec(ctx, `UPDATE executions SET
		first_actionable_state_id = COALESCE($3, first_actionable_state_id),
		first_actionable_state_key = COALESCE($4, first_actionable_state_key),
		initial_context_snapshot_id = COALESCE($5, initial_context_snapshot_id),
		initial_worker_prompt_id = COALESCE($6, initial_worker_prompt_id),
		initial_cache_key = COALESCE($7, initial_cache_key),
		initial_canonical_key = COALESCE($8, initial_canonical_key),
		version = version + 1, updated_at = now()
		WHERE id = $1 AND version = $2`,
		id, expectedVersion, mut.FirstActionableStateID, mut.FirstActionableStateKey, mut.InitialContextSnapshotID,
		mut.InitialWorkerPromptID, mut.InitialCacheKey, mut.InitialCanonicalKey)
	if err != nil {
		return nil, TranslateDBError("set execution artifacts", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	return r.one(ctx, x, `id = $1`, id)
}

// StateFileVersionHashes returns the content hash of each existing
// state-file version id (immutable rows).
func (r *ExecutionRepository) StateFileVersionHashes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.exec.Query(ctx, `SELECT id, content_hash FROM state_file_versions WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, TranslateDBError("state file version hashes", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, TranslateDBError("scan state file version hash", err)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// WorkerPromptHash returns a worker prompt's content hash.
func (r *ExecutionRepository) WorkerPromptHash(ctx context.Context, id uuid.UUID) (string, error) {
	var h string
	if err := r.exec.QueryRow(ctx, `SELECT hash FROM worker_prompts WHERE id = $1`, id).Scan(&h); err != nil {
		return "", TranslateDBError("worker prompt hash", err)
	}
	return h, nil
}

// ActivityCounts reads worker / continuation activity counts.
func (r *ExecutionRepository) ActivityCounts(ctx context.Context, projectID uuid.UUID) (int, int, int, int, error) {
	var d, c, rs, np int
	err := r.exec.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM worker_dispatches WHERE project_id = $1),
		(SELECT count(*) FROM continuations WHERE project_id = $1),
		(SELECT count(*) FROM continuations WHERE project_id = $1 AND session_confirmed_at IS NOT NULL),
		(SELECT count(*) FROM continuations WHERE project_id = $1 AND recovery_reason = 'NO_PROGRESS')`, projectID).Scan(&d, &c, &rs, &np)
	if err != nil {
		return 0, 0, 0, 0, TranslateDBError("activity counts", err)
	}
	return d, c, rs, np, nil
}

// StatesWithProtocol returns the states (of the given ids) that pin a
// response protocol.
func (r *ExecutionRepository) StatesWithProtocol(ctx context.Context, stateIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(stateIDs) == 0 {
		return out, nil
	}
	rows, err := r.exec.Query(ctx, `SELECT DISTINCT state_id FROM state_protocol_references WHERE state_id = ANY($1)`, stateIDs)
	if err != nil {
		return nil, TranslateDBError("state protocol references", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, TranslateDBError("scan state protocol reference", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
