package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
)

// ---------------------------------------------------------------------------
// Validation (P16-72).

const runCols = `id, project_id, execution_id, scope, work_unit_id, phase_id, target_id, label, attempt, status, cwd,
	acceptance, started_at, finished_at`

func scanValRun(row interface{ Scan(dest ...any) error }) (*persistence.ValidationRunRecord, error) {
	var v persistence.ValidationRunRecord
	var scope string
	var acc []byte
	if err := row.Scan(&v.ID, &v.ProjectID, &v.ExecutionID, &scope, &v.WorkUnitID, &v.PhaseID, &v.TargetID, &v.Label,
		&v.Attempt, &v.Status, &v.CWD, &acc, &v.StartedAt, &v.FinishedAt); err != nil {
		return nil, err
	}
	v.Scope = execution.ValidationScope(scope)
	if len(acc) > 0 && string(acc) != "null" {
		v.Acceptance = &execution.AcceptanceResult{}
		_ = json.Unmarshal(acc, v.Acceptance)
	}
	return &v, nil
}

func (r *EngineRepository) loadRecords(ctx context.Context, runs []*persistence.ValidationRunRecord) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(runs))
	byID := map[uuid.UUID]*persistence.ValidationRunRecord{}
	for i, v := range runs {
		ids[i] = v.ID
		byID[v.ID] = v
	}
	rows, err := r.exec.Query(ctx, `SELECT run_id, name, program, args, cwd, exit_code, stdout, stderr, duration_ms, timeout_ms,
		timed_out, outcome, infra_reason FROM validation_command_results WHERE run_id = ANY($1) ORDER BY run_id, ord`, ids)
	if err != nil {
		return TranslateDBError("list validation results", err)
	}
	defer rows.Close()
	for rows.Next() {
		var run uuid.UUID
		var rec execution.ValidationRecord
		var args []byte
		var dur, tmo int64
		var outcome string
		if err := rows.Scan(&run, &rec.Name, &rec.Program, &args, &rec.CWD, &rec.ExitCode, &rec.Stdout, &rec.Stderr, &dur, &tmo,
			&rec.TimedOut, &outcome, &rec.InfraReason); err != nil {
			return TranslateDBError("scan validation result", err)
		}
		_ = json.Unmarshal(args, &rec.Args)
		rec.Duration, rec.Timeout = time.Duration(dur)*time.Millisecond, time.Duration(tmo)*time.Millisecond
		rec.Outcome = execution.ValidationOutcome(outcome)
		byID[run].Records = append(byID[run].Records, rec)
	}
	return rows.Err()
}

// StartValidationRun inserts a RUNNING run (committed before commands run,
// so a crash leaves evidence of the in-flight run — WBS 16.79).
func (r *EngineRepository) StartValidationRun(ctx context.Context, ex persistence.Executor, v *persistence.ValidationRunRecord) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.StartedAt.IsZero() {
		v.StartedAt = time.Now().UTC()
	}
	if v.Attempt < 1 {
		v.Attempt = 1
	}
	v.Status = persistence.RunRunning
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO validation_runs (`+runCols+`, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'RUNNING', $10, NULL, $11, NULL, $12)`,
		v.ID, v.ProjectID, v.ExecutionID, string(v.Scope), v.WorkUnitID, v.PhaseID, v.TargetID, v.Label, v.Attempt, v.CWD, v.StartedAt.UTC(),
		correlationOf(ctx)); err != nil {
		return TranslateDBError("start validation run", err)
	}
	return nil
}

// FinishValidationRun records every command result and the final status
// in one statement batch; the run becomes immutable.
func (r *EngineRepository) FinishValidationRun(ctx context.Context, ex persistence.Executor, id uuid.UUID, status string, records []execution.ValidationRecord, acceptance *execution.AcceptanceResult) error {
	do := func(x executor) error {
		now := time.Now().UTC()
		for i, rec := range records {
			args := rec.Args
			if args == nil {
				args = []string{}
			}
			outcome := rec.Outcome
			if outcome == execution.ValidationNotConfigured || outcome == "" {
				outcome = execution.ValidationInfraError
			}
			if _, err := x.Exec(ctx, `INSERT INTO validation_command_results (id, run_id, ord, name, program, args, cwd, exit_code,
				stdout, stderr, duration_ms, timeout_ms, timed_out, outcome, infra_reason, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
				uuid.New(), id, i+1, rec.Name, rec.Program, jsonOf(args), rec.CWD, rec.ExitCode, rec.Stdout, rec.Stderr,
				rec.Duration.Milliseconds(), rec.Timeout.Milliseconds(), rec.TimedOut, string(outcome), rec.InfraReason, now); err != nil {
				return TranslateDBError("insert validation result", err)
			}
		}
		var acc any
		if acceptance != nil {
			acc = jsonOf(acceptance)
		}
		tag, err := x.Exec(ctx, `UPDATE validation_runs SET status = $2, acceptance = $3, finished_at = $4
			WHERE id = $1 AND status = 'RUNNING'`, id, status, acc, now)
		if err != nil {
			return TranslateDBError("finish validation run", err)
		}
		if tag.RowsAffected() == 0 {
			return persistence.ErrConflict
		}
		return nil
	}
	if ex != nil {
		return do(ex)
	}
	return Run(ctx, r.pool, func(tx persistence.Transaction) error { return do(txExecutor{tx}) })
}

// GetValidationRun returns a run with its command results.
func (r *EngineRepository) GetValidationRun(ctx context.Context, id uuid.UUID) (*persistence.ValidationRunRecord, error) {
	v, err := scanValRun(r.exec.QueryRow(ctx, `SELECT `+runCols+` FROM validation_runs WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get validation run", err)
	}
	return v, r.loadRecords(ctx, []*persistence.ValidationRunRecord{v})
}

// LatestValidationRun returns the newest run of a scope for a target
// (unit / phase / hardening target / execution for FINAL).
func (r *EngineRepository) LatestValidationRun(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, scope execution.ValidationScope, target uuid.UUID) (*persistence.ValidationRunRecord, error) {
	v, err := scanValRun(r.ex(ex).QueryRow(ctx, `SELECT `+runCols+` FROM validation_runs WHERE execution_id = $1 AND scope = $2
		AND (work_unit_id = $3 OR phase_id = $3 OR target_id = $3) ORDER BY started_at DESC, id DESC LIMIT 1`,
		executionID, string(scope), target))
	if err != nil {
		return nil, TranslateDBError("latest validation run", err)
	}
	return v, r.loadRecords(ctx, []*persistence.ValidationRunRecord{v})
}

func (r *EngineRepository) runs(ctx context.Context, sql string, args ...any) ([]*persistence.ValidationRunRecord, error) {
	rows, err := r.exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("list validation runs", err)
	}
	var out []*persistence.ValidationRunRecord
	for rows.Next() {
		v, err := scanValRun(rows)
		if err != nil {
			rows.Close()
			return nil, TranslateDBError("scan validation run", err)
		}
		out = append(out, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, r.loadRecords(ctx, out)
}

// ListRunningValidations returns runs interrupted while RUNNING.
func (r *EngineRepository) ListRunningValidations(ctx context.Context, executionID uuid.UUID) ([]*persistence.ValidationRunRecord, error) {
	return r.runs(ctx, `SELECT `+runCols+` FROM validation_runs WHERE execution_id = $1 AND status = 'RUNNING' ORDER BY started_at`, executionID)
}

// ListValidationRuns returns every run of the execution.
func (r *EngineRepository) ListValidationRuns(ctx context.Context, executionID uuid.UUID) ([]*persistence.ValidationRunRecord, error) {
	return r.runs(ctx, `SELECT `+runCols+` FROM validation_runs WHERE execution_id = $1 ORDER BY started_at, id`, executionID)
}

// CountValidationRuns counts runs of a scope for a target.
func (r *EngineRepository) CountValidationRuns(ctx context.Context, executionID uuid.UUID, scope execution.ValidationScope, target uuid.UUID) (int, error) {
	var n int
	err := r.exec.QueryRow(ctx, `SELECT count(*) FROM validation_runs WHERE execution_id = $1 AND scope = $2
		AND (work_unit_id = $3 OR phase_id = $3 OR target_id = $3)`, executionID, string(scope), target).Scan(&n)
	if err != nil {
		return 0, TranslateDBError("count validation runs", err)
	}
	return n, nil
}

// CreateFixAttempt inserts a fix attempt; (target, attempt) is unique.
func (r *EngineRepository) CreateFixAttempt(ctx context.Context, ex persistence.Executor, f *persistence.FixAttemptRecord) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	ev := f.Evidence
	if ev == nil {
		ev = map[string]any{}
	}
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO fix_attempts (id, execution_id, target_kind, target_id, failed_run_id, attempt,
		dispatch_id, evidence, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		f.ID, f.ExecutionID, f.TargetKind, f.TargetID, f.FailedRunID, f.Attempt, f.DispatchID, jsonOf(ev), f.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create fix attempt", err)
	}
	return nil
}

// SetFixDispatch links the FIX dispatch to its attempt.
func (r *EngineRepository) SetFixDispatch(ctx context.Context, ex persistence.Executor, id, dispatchID uuid.UUID) error {
	if _, err := r.ex(ex).Exec(ctx, `UPDATE fix_attempts SET dispatch_id = $2 WHERE id = $1 AND dispatch_id IS NULL`, id, dispatchID); err != nil {
		return TranslateDBError("set fix dispatch", err)
	}
	return nil
}

// ListFixAttempts lists attempts of the execution (optionally one target).
func (r *EngineRepository) ListFixAttempts(ctx context.Context, executionID uuid.UUID, target *uuid.UUID) ([]*persistence.FixAttemptRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, execution_id, target_kind, target_id, failed_run_id, attempt, dispatch_id, evidence,
		created_at FROM fix_attempts WHERE execution_id = $1 AND ($2::uuid IS NULL OR target_id = $2) ORDER BY created_at, attempt`,
		executionID, target)
	if err != nil {
		return nil, TranslateDBError("list fix attempts", err)
	}
	defer rows.Close()
	var out []*persistence.FixAttemptRecord
	for rows.Next() {
		var f persistence.FixAttemptRecord
		var ev []byte
		if err := rows.Scan(&f.ID, &f.ExecutionID, &f.TargetKind, &f.TargetID, &f.FailedRunID, &f.Attempt, &f.DispatchID, &ev, &f.CreatedAt); err != nil {
			return nil, TranslateDBError("scan fix attempt", err)
		}
		_ = json.Unmarshal(ev, &f.Evidence)
		out = append(out, &f)
	}
	return out, rows.Err()
}

// ApproveCriterion records an operator approval of a MANUAL criterion
// (idempotent).
func (r *EngineRepository) ApproveCriterion(ctx context.Context, executionID, unitID uuid.UUID, index int, by, note string) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO acceptance_approvals (id, execution_id, work_unit_id, check_index, approved_by, note, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now()) ON CONFLICT (work_unit_id, check_index) DO NOTHING`,
		uuid.New(), executionID, unitID, index, by, note); err != nil {
		return TranslateDBError("approve criterion", err)
	}
	return nil
}

// Approvals returns the approved check indexes of a unit.
func (r *EngineRepository) Approvals(ctx context.Context, ex persistence.Executor, unitID uuid.UUID) (map[int]bool, error) {
	rows, err := r.ex(ex).Query(ctx, `SELECT check_index FROM acceptance_approvals WHERE work_unit_id = $1`, unitID)
	if err != nil {
		return nil, TranslateDBError("list approvals", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			return nil, TranslateDBError("scan approval", err)
		}
		out[i] = true
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Checkpoints (P16-73).

const cpCols = `id, project_id, execution_id, kind, target_id, checkpoint_key, status, COALESCE(commit_sha, ''),
	COALESCE(parent_sha, ''), COALESCE(tree_sha, ''), validation_run_id, error, created_at, completed_at`

func scanCheckpoint(row interface{ Scan(dest ...any) error }) (*persistence.RepoCheckpointRecord, error) {
	var c persistence.RepoCheckpointRecord
	var kind string
	if err := row.Scan(&c.ID, &c.ProjectID, &c.ExecutionID, &kind, &c.TargetID, &c.Key, &c.Status, &c.Commit, &c.Parent,
		&c.Tree, &c.ValidationRunID, &c.Error, &c.CreatedAt, &c.CompletedAt); err != nil {
		return nil, err
	}
	c.Kind = execution.RepoCheckpointKind(kind)
	return &c, nil
}

// CreateCheckpointIntent inserts a PENDING checkpoint; an existing row with
// the same key is returned instead (reconciliation, WBS 16.80).
func (r *EngineRepository) CreateCheckpointIntent(ctx context.Context, c *persistence.RepoCheckpointRecord) (*persistence.RepoCheckpointRecord, error) {
	if got, err := r.GetCheckpointByKey(ctx, c.Key); err == nil {
		return got, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.Status, c.CreatedAt = persistence.CheckpointPending, time.Now().UTC()
	_, err := r.exec.Exec(ctx, `INSERT INTO repository_checkpoints (id, project_id, execution_id, kind, target_id, checkpoint_key,
		status, validation_run_id, created_at) VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', $7, $8)
		ON CONFLICT (checkpoint_key) DO NOTHING`, c.ID, c.ProjectID, c.ExecutionID, string(c.Kind), c.TargetID, c.Key,
		c.ValidationRunID, c.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("create checkpoint intent", err)
	}
	return r.GetCheckpointByKey(ctx, c.Key)
}

// CompleteCheckpoint finalizes a PENDING checkpoint.
func (r *EngineRepository) CompleteCheckpoint(ctx context.Context, id uuid.UUID, status, commit, parent, tree, errMsg string) (*persistence.RepoCheckpointRecord, error) {
	tag, err := r.exec.Exec(ctx, `UPDATE repository_checkpoints SET status = $2, commit_sha = $3, parent_sha = $4, tree_sha = $5,
		error = $6, completed_at = now() WHERE id = $1 AND status = 'PENDING'`,
		id, status, nullStr(commit), nullStr(parent), nullStr(tree), errMsg)
	if err != nil {
		return nil, TranslateDBError("complete checkpoint", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	return r.GetRepoCheckpoint(ctx, id)
}

// GetRepoCheckpoint returns a checkpoint by id.
func (r *EngineRepository) GetRepoCheckpoint(ctx context.Context, id uuid.UUID) (*persistence.RepoCheckpointRecord, error) {
	c, err := scanCheckpoint(r.exec.QueryRow(ctx, `SELECT `+cpCols+` FROM repository_checkpoints WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get checkpoint", err)
	}
	return c, nil
}

// GetCheckpointByKey returns a checkpoint by key.
func (r *EngineRepository) GetCheckpointByKey(ctx context.Context, key string) (*persistence.RepoCheckpointRecord, error) {
	c, err := scanCheckpoint(r.exec.QueryRow(ctx, `SELECT `+cpCols+` FROM repository_checkpoints WHERE checkpoint_key = $1`, key))
	if err != nil {
		return nil, TranslateDBError("get checkpoint by key", err)
	}
	return c, nil
}

// ListRepoCheckpoints lists the execution's checkpoints chronologically.
func (r *EngineRepository) ListRepoCheckpoints(ctx context.Context, executionID uuid.UUID) ([]*persistence.RepoCheckpointRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+cpCols+` FROM repository_checkpoints WHERE execution_id = $1 ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, TranslateDBError("list checkpoints", err)
	}
	defer rows.Close()
	var out []*persistence.RepoCheckpointRecord
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, TranslateDBError("scan checkpoint", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Hardening (P16-74).

const passCols = `id, project_id, execution_id, generation, status, resume_status, round, baseline_checkpoint_id,
	baseline_fingerprint, repo_fingerprint, recovery_reason, version, started_at, completed_at, updated_at`

func scanPass(row interface{ Scan(dest ...any) error }) (*persistence.HardeningPassRecord, error) {
	var p persistence.HardeningPassRecord
	var status string
	var resume *string
	if err := row.Scan(&p.ID, &p.ProjectID, &p.ExecutionID, &p.Generation, &status, &resume, &p.Round, &p.BaselineCheckpointID,
		&p.BaselineFingerprint, &p.RepoFingerprint, &p.RecoveryReason, &p.Version, &p.StartedAt, &p.CompletedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Status = hardening.PassStatus(status)
	if resume != nil {
		rs := hardening.PassStatus(*resume)
		p.ResumeStatus = &rs
	}
	return &p, nil
}

// CreatePass inserts the pass of an execution's generation; an existing
// one is returned instead (false) — exactly one per generation (16.89).
func (r *EngineRepository) CreatePass(ctx context.Context, p *persistence.HardeningPassRecord) (*persistence.HardeningPassRecord, bool, error) {
	now := time.Now().UTC()
	tag, err := r.exec.Exec(ctx, `INSERT INTO hardening_passes (id, project_id, execution_id, generation, status, round,
		baseline_checkpoint_id, baseline_fingerprint, repo_fingerprint, version, started_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, $10, $10) ON CONFLICT (execution_id, generation) DO NOTHING`,
		p.ID, p.ProjectID, p.ExecutionID, p.Generation, string(p.Status), p.Round, p.BaselineCheckpointID, p.BaselineFingerprint,
		p.RepoFingerprint, now)
	if err != nil {
		return nil, false, TranslateDBError("create hardening pass", err)
	}
	got, err := r.GetPass(ctx, nil, p.ExecutionID, p.Generation)
	return got, err == nil && tag.RowsAffected() == 1, err
}

// GetPass returns the pass of an execution's generation.
func (r *EngineRepository) GetPass(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, generation int) (*persistence.HardeningPassRecord, error) {
	p, err := scanPass(r.ex(ex).QueryRow(ctx, `SELECT `+passCols+` FROM hardening_passes WHERE execution_id = $1 AND generation = $2`, executionID, generation))
	if err != nil {
		return nil, TranslateDBError("get hardening pass", err)
	}
	return p, nil
}

// GetPassByID returns a pass by id.
func (r *EngineRepository) GetPassByID(ctx context.Context, id uuid.UUID) (*persistence.HardeningPassRecord, error) {
	p, err := scanPass(r.exec.QueryRow(ctx, `SELECT `+passCols+` FROM hardening_passes WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get hardening pass", err)
	}
	return p, nil
}

// UpdatePass is a versioned CAS of the pass's mutable fields; the status
// change is validated.
func (r *EngineRepository) UpdatePass(ctx context.Context, ex persistence.Executor, p *persistence.HardeningPassRecord) (*persistence.HardeningPassRecord, error) {
	x := r.ex(ex)
	cur, err := scanPass(x.QueryRow(ctx, `SELECT `+passCols+` FROM hardening_passes WHERE id = $1`, p.ID))
	if err != nil {
		return nil, TranslateDBError("get hardening pass", err)
	}
	if cur.Version != p.Version {
		return nil, persistence.ErrConflict
	}
	if cur.Status != p.Status {
		if err := hardening.ValidateTransition(cur.Status, p.Status); err != nil {
			return nil, err
		}
	}
	var resume *string
	if p.ResumeStatus != nil {
		s := string(*p.ResumeStatus)
		resume = &s
	}
	tag, err := x.Exec(ctx, `UPDATE hardening_passes SET status = $3, resume_status = $4, round = $5, baseline_checkpoint_id = $6,
		baseline_fingerprint = $7, repo_fingerprint = $8, recovery_reason = $9, completed_at = $10, version = version + 1,
		updated_at = now() WHERE id = $1 AND version = $2`, p.ID, p.Version, string(p.Status), resume, p.Round,
		p.BaselineCheckpointID, p.BaselineFingerprint, p.RepoFingerprint, p.RecoveryReason, p.CompletedAt)
	if err != nil {
		return nil, TranslateDBError("update hardening pass", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrConflict
	}
	return scanPass(x.QueryRow(ctx, `SELECT `+passCols+` FROM hardening_passes WHERE id = $1`, p.ID))
}

// ListPasses lists an execution's passes by generation.
func (r *EngineRepository) ListPasses(ctx context.Context, executionID uuid.UUID) ([]*persistence.HardeningPassRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+passCols+` FROM hardening_passes WHERE execution_id = $1 ORDER BY generation`, executionID)
	if err != nil {
		return nil, TranslateDBError("list hardening passes", err)
	}
	defer rows.Close()
	var out []*persistence.HardeningPassRecord
	for rows.Next() {
		p, err := scanPass(rows)
		if err != nil {
			return nil, TranslateDBError("scan hardening pass", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const groupCols = `id, pass_id, round, group_key, fingerprint, members, classification, reason, destination, status, matrix,
	tests, lineage, checkpoint_id, tests_before, tests_after, refs_migrated, functions_removed, created_at, updated_at`

func scanGroup(row interface{ Scan(dest ...any) error }) (*persistence.HardeningGroupRecord, error) {
	var g persistence.HardeningGroupRecord
	var members, dest, matrix, tests, lineage []byte
	var cl, st string
	if err := row.Scan(&g.ID, &g.PassID, &g.Round, &g.Key, &g.Fingerprint, &members, &cl, &g.Reason, &dest, &st, &matrix,
		&tests, &lineage, &g.CheckpointID, &g.TestsBefore, &g.TestsAfter, &g.RefsMigrated, &g.FunctionsRemoved, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	g.Classification, g.Status = hardening.Classification(cl), hardening.GroupStatus(st)
	_ = json.Unmarshal(members, &g.Members)
	_ = json.Unmarshal(matrix, &g.Matrix)
	_ = json.Unmarshal(tests, &g.Tests)
	if len(dest) > 0 && string(dest) != "null" {
		g.Destination = &hardening.Destination{}
		_ = json.Unmarshal(dest, g.Destination)
	}
	if len(lineage) > 0 && string(lineage) != "null" {
		g.Lineage = &hardening.Lineage{}
		_ = json.Unmarshal(lineage, g.Lineage)
	}
	return &g, nil
}

func nullJSON(v any, isNil bool) any {
	if isNil {
		return nil
	}
	return jsonOf(v)
}

// UpsertGroup inserts a group of a round; an existing (pass, round, key)
// row is returned unchanged (re-analysis after a crash is idempotent).
func (r *EngineRepository) UpsertGroup(ctx context.Context, g *persistence.HardeningGroupRecord) (*persistence.HardeningGroupRecord, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	now := time.Now().UTC()
	if g.Matrix == nil {
		g.Matrix = []hardening.MatrixRow{}
	}
	if g.Tests == nil {
		g.Tests = []hardening.TestInfo{}
	}
	if _, err := r.exec.Exec(ctx, `INSERT INTO hardening_groups (`+groupCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19)
		ON CONFLICT (pass_id, round, group_key) DO NOTHING`,
		g.ID, g.PassID, g.Round, g.Key, g.Fingerprint, jsonOf(g.Members), string(g.Classification), g.Reason,
		nullJSON(g.Destination, g.Destination == nil), string(g.Status), jsonOf(g.Matrix), jsonOf(g.Tests),
		nullJSON(g.Lineage, g.Lineage == nil), g.CheckpointID, g.TestsBefore, g.TestsAfter, g.RefsMigrated, g.FunctionsRemoved, now); err != nil {
		return nil, TranslateDBError("upsert hardening group", err)
	}
	return scanGroup(r.exec.QueryRow(ctx, `SELECT `+groupCols+` FROM hardening_groups WHERE pass_id = $1 AND round = $2 AND group_key = $3`,
		g.PassID, g.Round, g.Key))
}

// UpdateGroup persists a group's mutable fields.
func (r *EngineRepository) UpdateGroup(ctx context.Context, g *persistence.HardeningGroupRecord) error {
	tag, err := r.exec.Exec(ctx, `UPDATE hardening_groups SET status = $2, matrix = $3, tests = $4, lineage = $5, checkpoint_id = $6,
		tests_before = $7, tests_after = $8, refs_migrated = $9, functions_removed = $10, destination = $11, updated_at = now()
		WHERE id = $1`, g.ID, string(g.Status), jsonOf(g.Matrix), jsonOf(g.Tests), nullJSON(g.Lineage, g.Lineage == nil),
		g.CheckpointID, g.TestsBefore, g.TestsAfter, g.RefsMigrated, g.FunctionsRemoved, nullJSON(g.Destination, g.Destination == nil))
	if err != nil {
		return TranslateDBError("update hardening group", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListGroups lists a pass's groups (round 0 = every round) by round and key.
func (r *EngineRepository) ListGroups(ctx context.Context, passID uuid.UUID, round int) ([]*persistence.HardeningGroupRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+groupCols+` FROM hardening_groups WHERE pass_id = $1 AND ($2 = 0 OR round = $2)
		ORDER BY round, group_key`, passID, round)
	if err != nil {
		return nil, TranslateDBError("list hardening groups", err)
	}
	defer rows.Close()
	var out []*persistence.HardeningGroupRecord
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, TranslateDBError("scan hardening group", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup returns a group by id.
func (r *EngineRepository) GetGroup(ctx context.Context, id uuid.UUID) (*persistence.HardeningGroupRecord, error) {
	g, err := scanGroup(r.exec.QueryRow(ctx, `SELECT `+groupCols+` FROM hardening_groups WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get hardening group", err)
	}
	return g, nil
}

// CreateOperations inserts a group's planned operations (idempotent).
func (r *EngineRepository) CreateOperations(ctx context.Context, groupID uuid.UUID, ops []hardening.Operation) error {
	return Run(ctx, r.pool, func(tx persistence.Transaction) error {
		for _, op := range ops {
			if _, err := tx.Exec(ctx, `INSERT INTO hardening_operations (id, group_id, seq, kind, package, status)
				VALUES ($1, $2, $3, $4, $5, 'PENDING') ON CONFLICT (group_id, seq) DO NOTHING`,
				uuid.New(), groupID, op.Seq, string(op.Kind), op.Package); err != nil {
				return TranslateDBError("create hardening operation", err)
			}
		}
		return nil
	})
}

// ListOperations lists a group's operations in order.
func (r *EngineRepository) ListOperations(ctx context.Context, groupID uuid.UUID) ([]*persistence.HardeningOperationRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT id, group_id, seq, kind, package, status, evidence, repo_fingerprint, validation_run_id,
		started_at, finished_at FROM hardening_operations WHERE group_id = $1 ORDER BY seq`, groupID)
	if err != nil {
		return nil, TranslateDBError("list hardening operations", err)
	}
	defer rows.Close()
	var out []*persistence.HardeningOperationRecord
	for rows.Next() {
		var o persistence.HardeningOperationRecord
		var kind, st string
		var ev []byte
		if err := rows.Scan(&o.ID, &o.GroupID, &o.Seq, &kind, &o.Package, &st, &ev, &o.RepoFingerprint, &o.ValidationRunID,
			&o.StartedAt, &o.FinishedAt); err != nil {
			return nil, TranslateDBError("scan hardening operation", err)
		}
		o.Kind, o.Status = hardening.OpKind(kind), hardening.OpStatus(st)
		_ = json.Unmarshal(ev, &o.Evidence)
		out = append(out, &o)
	}
	return out, rows.Err()
}

// UpdateOperation persists an operation's status / evidence.
func (r *EngineRepository) UpdateOperation(ctx context.Context, o *persistence.HardeningOperationRecord) error {
	ev := o.Evidence
	if ev == nil {
		ev = map[string]any{}
	}
	if _, err := r.exec.Exec(ctx, `UPDATE hardening_operations SET status = $2, evidence = $3, repo_fingerprint = $4,
		validation_run_id = $5, started_at = $6, finished_at = $7 WHERE id = $1`, o.ID, string(o.Status), jsonOf(ev),
		o.RepoFingerprint, o.ValidationRunID, o.StartedAt, o.FinishedAt); err != nil {
		return TranslateDBError("update hardening operation", err)
	}
	return nil
}

// UpsertReport stores the pass report.
func (r *EngineRepository) UpsertReport(ctx context.Context, passID uuid.UUID, rep hardening.Report) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO hardening_reports (pass_id, report, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (pass_id) DO UPDATE SET report = EXCLUDED.report, updated_at = now()`, passID, jsonOf(rep)); err != nil {
		return TranslateDBError("upsert hardening report", err)
	}
	return nil
}

// GetReport returns the pass report.
func (r *EngineRepository) GetReport(ctx context.Context, passID uuid.UUID) (*hardening.Report, error) {
	var b []byte
	if err := r.exec.QueryRow(ctx, `SELECT report FROM hardening_reports WHERE pass_id = $1`, passID).Scan(&b); err != nil {
		return nil, TranslateDBError("get hardening report", err)
	}
	var rep hardening.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// UpsertRetained records an accepted (intentionally retained) duplicate
// (WBS 16.148).
func (r *EngineRepository) UpsertRetained(ctx context.Context, projectID uuid.UUID, fingerprint, classification, reason string) error {
	if _, err := r.exec.Exec(ctx, `INSERT INTO hardening_retained_groups (project_id, fingerprint, classification, reason, recorded_at)
		VALUES ($1, $2, $3, $4, now()) ON CONFLICT (project_id, fingerprint) DO UPDATE SET classification = EXCLUDED.classification,
		reason = EXCLUDED.reason`, projectID, fingerprint, classification, reason); err != nil {
		return TranslateDBError("upsert retained group", err)
	}
	return nil
}

// ListRetained maps retained fingerprints to their reasons.
func (r *EngineRepository) ListRetained(ctx context.Context, projectID uuid.UUID) (map[string]string, error) {
	rows, err := r.exec.Query(ctx, `SELECT fingerprint, reason FROM hardening_retained_groups WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, TranslateDBError("list retained groups", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var f, reason string
		if err := rows.Scan(&f, &reason); err != nil {
			return nil, TranslateDBError("scan retained group", err)
		}
		out[f] = reason
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Completion (P16-75).

// CreateCompletion writes the completion record once; an existing record
// is returned unchanged (false) — the timestamp is never rewritten.
func (r *EngineRepository) CreateCompletion(ctx context.Context, ex persistence.Executor, c *persistence.CompletionRecord) (*persistence.CompletionRecord, bool, error) {
	x := r.ex(ex)
	tag, err := x.Exec(ctx, `INSERT INTO execution_completions (execution_id, project_id, final_phase_id, hardening_pass_id,
		final_validation_run_id, final_checkpoint_id, completed_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (execution_id) DO NOTHING`, c.ExecutionID, c.ProjectID, c.FinalPhaseID, c.HardeningPassID,
		c.FinalValidationRunID, c.FinalCheckpointID, c.CompletedAt.UTC())
	if err != nil {
		return nil, false, TranslateDBError("create completion", err)
	}
	got, err := r.getCompletion(ctx, x, c.ExecutionID)
	return got, err == nil && tag.RowsAffected() == 1, err
}

func (r *EngineRepository) getCompletion(ctx context.Context, x executor, id uuid.UUID) (*persistence.CompletionRecord, error) {
	var c persistence.CompletionRecord
	err := x.QueryRow(ctx, `SELECT execution_id, project_id, final_phase_id, hardening_pass_id, final_validation_run_id,
		final_checkpoint_id, completed_at FROM execution_completions WHERE execution_id = $1`, id).
		Scan(&c.ExecutionID, &c.ProjectID, &c.FinalPhaseID, &c.HardeningPassID, &c.FinalValidationRunID, &c.FinalCheckpointID, &c.CompletedAt)
	if err != nil {
		return nil, TranslateDBError("get completion", err)
	}
	return &c, nil
}

// GetCompletion returns the completion record.
func (r *EngineRepository) GetCompletion(ctx context.Context, executionID uuid.UUID) (*persistence.CompletionRecord, error) {
	return r.getCompletion(ctx, r.exec, executionID)
}
