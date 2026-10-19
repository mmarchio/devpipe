package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Work units (P16-69 / P16-70).

// ClaimUnit atomically claims a unit for an execution (WBS 16.22): a
// PENDING / READY unit whose dependencies are all DONE becomes EXECUTING
// and claimed; an in-progress unit already claimed by the same execution
// — or left claimed by an execution that is no longer active — is
// (re)claimed unchanged (resume). Anything else is ErrConflict.
func (r *EngineRepository) ClaimUnit(ctx context.Context, ex persistence.Executor, unitID, executionID uuid.UUID) error {
	x := r.ex(ex)
	tag, err := x.Exec(ctx, `UPDATE work_units SET status = 'EXECUTING', claimed_execution_id = $2,
		started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $1 AND (
		  (status IN ('PENDING', 'READY') AND (claimed_execution_id IS NULL OR claimed_execution_id = $2)
		    AND NOT EXISTS (SELECT 1 FROM work_unit_dependencies d JOIN work_units w ON w.id = d.depends_on_work_unit_id
		      WHERE d.work_unit_id = $1 AND w.status <> 'DONE'))
		  OR (status IN ('EXECUTING', 'INCOMPLETE', 'VALIDATING') AND (claimed_execution_id = $2
		    -- a claim left by an execution that is no longer active is taken over
		    OR NOT EXISTS (SELECT 1 FROM executions x WHERE x.id = claimed_execution_id
		      AND x.status IN ('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED')))))`, unitID, executionID)
	if err != nil {
		return TranslateDBError("claim work unit", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// SetUnitStatus is a status CAS from any of `from` (never to DONE: use
// MarkUnitDone).
func (r *EngineRepository) SetUnitStatus(ctx context.Context, ex persistence.Executor, unitID uuid.UUID, from []wbs.WorkUnitStatus, to wbs.WorkUnitStatus) error {
	if to == wbs.WorkUnitStatusDONE {
		return errors.New("dbrepo: DONE requires MarkUnitDone with a passing validation run")
	}
	tag, err := r.ex(ex).Exec(ctx, `UPDATE work_units SET status = $3, updated_at = now() WHERE id = $1 AND status = ANY($2)`,
		unitID, statusList(from), string(to))
	if err != nil {
		return TranslateDBError("set work unit status", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// MarkUnitDone sets DONE with the PASS validation run that justified it
// (WBS 16.58): only a VALIDATING unit claimed by the execution, and only
// when the run is a PASS of that unit.
func (r *EngineRepository) MarkUnitDone(ctx context.Context, ex persistence.Executor, unitID, executionID, runID uuid.UUID) error {
	tag, err := r.ex(ex).Exec(ctx, `UPDATE work_units SET status = 'DONE', done_validation_run_id = $3,
		completed_at = COALESCE(completed_at, now()), updated_at = now()
		WHERE id = $1 AND claimed_execution_id = $2 AND status = 'VALIDATING'
		  AND EXISTS (SELECT 1 FROM validation_runs v WHERE v.id = $3 AND v.work_unit_id = $1 AND v.status = 'PASS')`,
		unitID, executionID, runID)
	if err != nil {
		return TranslateDBError("mark work unit done", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// ListPlanUnits lists a plan's units in sequence order.
func (r *EngineRepository) ListPlanUnits(ctx context.Context, ex persistence.Executor, planID uuid.UUID) ([]*wbs.WorkUnit, error) {
	rows, err := r.ex(ex).Query(ctx, `SELECT `+workUnitColumns+` FROM work_units WHERE execution_plan_id = $1 ORDER BY sequence`, planID)
	if err != nil {
		return nil, TranslateDBError("list plan units", err)
	}
	defer rows.Close()
	var out []*wbs.WorkUnit
	for rows.Next() {
		u, err := scanWorkUnit(rows)
		if err != nil {
			return nil, TranslateDBError("scan work unit", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UnitDependencies maps each unit of the plan to the units it depends on.
func (r *EngineRepository) UnitDependencies(ctx context.Context, ex persistence.Executor, planID uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := r.ex(ex).Query(ctx, `SELECT d.work_unit_id, d.depends_on_work_unit_id FROM work_unit_dependencies d
		JOIN work_units w ON w.id = d.work_unit_id WHERE w.execution_plan_id = $1 ORDER BY 1, 2`, planID)
	if err != nil {
		return nil, TranslateDBError("list unit dependencies", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var u, d uuid.UUID
		if err := rows.Scan(&u, &d); err != nil {
			return nil, TranslateDBError("scan unit dependency", err)
		}
		out[u] = append(out[u], d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Plans and candidates (P16-71).

const candidateCols = `id, project_id, execution_id, phase_id, dispatch_id, wbs_version, wbs_hash, mode, payload, status,
	report, plan_id, created_at, decided_at, markdown`

func scanCandidate(row interface{ Scan(dest ...any) error }) (*persistence.PlanCandidateRecord, error) {
	var c persistence.PlanCandidateRecord
	var report []byte
	if err := row.Scan(&c.ID, &c.ProjectID, &c.ExecutionID, &c.PhaseID, &c.DispatchID, &c.WBSVersion, &c.WBSHash, &c.Mode,
		&c.Payload, &c.Status, &report, &c.PlanID, &c.CreatedAt, &c.DecidedAt, &c.Markdown); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(report, &c.Report)
	return &c, nil
}

// CreateCandidate inserts a candidate; an existing candidate for the same
// dispatch is returned instead (idempotent parse).
func (r *EngineRepository) CreateCandidate(ctx context.Context, ex persistence.Executor, c *persistence.PlanCandidateRecord) (*persistence.PlanCandidateRecord, error) {
	x := r.ex(ex)
	if c.DispatchID != nil {
		if got, err := r.GetCandidateByDispatch(ctx, x, *c.DispatchID); err == nil {
			return got, nil
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return nil, err
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.Status == "" {
		c.Status = persistence.CandidatePending
	}
	report := c.Report
	if report == nil {
		report = wbs.PlanProblems{}
	}
	var decided *time.Time
	if c.Status != persistence.CandidatePending {
		t := c.CreatedAt
		decided = &t
	}
	c.DecidedAt = decided
	var payload any
	if len(c.Payload) > 0 {
		payload = c.Payload
	}
	if _, err := x.Exec(ctx, `INSERT INTO plan_candidates (`+candidateCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		c.ID, c.ProjectID, c.ExecutionID, c.PhaseID, c.DispatchID, c.WBSVersion, c.WBSHash, c.Mode, payload, c.Status,
		jsonOf(report), c.PlanID, c.CreatedAt.UTC(), decided, c.Markdown); err != nil {
		return nil, TranslateDBError("create plan candidate", err)
	}
	return c, nil
}

// GetCandidateByDispatch returns the candidate parsed from a dispatch.
func (r *EngineRepository) GetCandidateByDispatch(ctx context.Context, ex persistence.Executor, dispatchID uuid.UUID) (*persistence.PlanCandidateRecord, error) {
	c, err := scanCandidate(r.ex(ex).QueryRow(ctx, `SELECT `+candidateCols+` FROM plan_candidates WHERE dispatch_id = $1`, dispatchID))
	if err != nil {
		return nil, TranslateDBError("get plan candidate", err)
	}
	return c, nil
}

// LatestCandidate returns the newest candidate for the execution's phase.
func (r *EngineRepository) LatestCandidate(ctx context.Context, ex persistence.Executor, executionID, phaseID uuid.UUID) (*persistence.PlanCandidateRecord, error) {
	c, err := scanCandidate(r.ex(ex).QueryRow(ctx, `SELECT `+candidateCols+` FROM plan_candidates
		WHERE execution_id = $1 AND phase_id = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, executionID, phaseID))
	if err != nil {
		return nil, TranslateDBError("latest plan candidate", err)
	}
	return c, nil
}

// DecideCandidate records ACCEPTED / REJECTED once.
func (r *EngineRepository) DecideCandidate(ctx context.Context, ex persistence.Executor, id uuid.UUID, status string, report wbs.PlanProblems, planID *uuid.UUID) error {
	if report == nil {
		report = wbs.PlanProblems{}
	}
	tag, err := r.ex(ex).Exec(ctx, `UPDATE plan_candidates SET status = $2, report = $3, plan_id = $4, decided_at = now()
		WHERE id = $1 AND status = 'PENDING'`, id, status, jsonOf(report), planID)
	if err != nil {
		return TranslateDBError("decide plan candidate", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// CountRejectedCandidates counts rejections for the execution's phase
// created after `since` (nil = all).
func (r *EngineRepository) CountRejectedCandidates(ctx context.Context, ex persistence.Executor, executionID, phaseID uuid.UUID, since *time.Time) (int, error) {
	var n int
	err := r.ex(ex).QueryRow(ctx, `SELECT count(*) FROM plan_candidates WHERE execution_id = $1 AND phase_id = $2
		AND status = 'REJECTED' AND ($3::timestamptz IS NULL OR created_at > $3)`, executionID, phaseID, since).Scan(&n)
	if err != nil {
		return 0, TranslateDBError("count rejected candidates", err)
	}
	return n, nil
}

// LastRecoveryResolved returns the time of the execution's latest
// EXECUTION_RECOVERY_RESOLVED event whose recovery reason matches.
func (r *EngineRepository) LastRecoveryResolved(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, reason string) (*time.Time, error) {
	var at *time.Time
	err := r.ex(ex).QueryRow(ctx, `SELECT max(created_at) FROM execution_events WHERE execution_id = $1 AND type = $2
		AND payload->>'reason' = $3`, executionID, string(execution.EventRecoveryResolved), reason).Scan(&at)
	if err != nil {
		return nil, TranslateDBError("last recovery resolved", err)
	}
	return at, nil
}

// LastLimitReset returns the time of the execution's last resolved
// recovery whose attempt limit is limit, or nil.
func (r *EngineRepository) LastLimitReset(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, limit string) (*time.Time, error) {
	var at *time.Time
	err := r.ex(ex).QueryRow(ctx, `SELECT max(created_at) FROM execution_events WHERE execution_id = $1
		AND ((type = $2 AND payload->>'limit' = $3) OR (type = $4 AND payload->'resets' ? $3))`,
		executionID, string(execution.EventRecoveryResolved), limit, string(execution.EventUnitReplanned)).Scan(&at)
	if err != nil {
		return nil, TranslateDBError("last limit reset", err)
	}
	return at, nil
}

// ReplanUnit replaces a unit's specification (objective, instructions,
// criteria, required tests, validation, checks, paths) and deletes its
// manual-criterion approvals, which referred to the old criteria.
func (r *EngineRepository) ReplanUnit(ctx context.Context, ex persistence.Executor, unitID uuid.UUID, spec *wbs.WorkUnitSpecification) error {
	val, checks, paths, err := phase16SpecJSON(spec)
	if err != nil {
		return err
	}
	acc, err := specJSON(spec.AcceptanceCriteria)
	if err != nil {
		return err
	}
	req, err := specJSON(spec.RequiredTests)
	if err != nil {
		return err
	}
	x := r.ex(ex)
	tag, err := x.Exec(ctx, `UPDATE work_units SET objective = $2, implementation_instructions = $3, acceptance_criteria = $4,
		required_tests = $5, validation_commands = $6, acceptance_checks = $7, paths = $8, updated_at = now() WHERE id = $1`,
		unitID, spec.Objective, spec.ImplementationInstructions, acc, req, val, checks, paths)
	if err != nil {
		return TranslateDBError("replan work unit", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	if _, err := x.Exec(ctx, `DELETE FROM acceptance_approvals WHERE work_unit_id = $1`, unitID); err != nil {
		return TranslateDBError("drop replanned unit approvals", err)
	}
	return nil
}

// CreatePlanWithUnits inserts a plan, its units and their dependencies in
// the caller's transaction (WBS 16.18): either all rows exist or none.
func (r *EngineRepository) CreatePlanWithUnits(ctx context.Context, ex persistence.Executor, plan *wbs.ExecutionPlan, units []wbs.PlannedUnit) ([]*wbs.WorkUnit, error) {
	if ex == nil {
		return nil, errors.New("dbrepo: CreatePlanWithUnits requires a transaction")
	}
	plans := NewPlanRepositoryForExecutor(ex)
	if err := plans.Create(ctx, plan); err != nil {
		return nil, err
	}
	wus := NewWorkUnitRepositoryForExecutor(ex)
	ids := map[string]uuid.UUID{}
	var out []*wbs.WorkUnit
	now := time.Now().UTC()
	for _, pu := range units {
		u, err := wbs.NewWorkUnit(plan.ID, pu.Sequence, pu.Name, pu.Kind)
		if err != nil {
			return nil, err
		}
		spec := pu.Spec
		spec.Key = pu.Key
		u.Specification = &spec
		u.Description = spec.Objective
		u.Status = pu.Status
		u.CarriedFromUnitID = pu.CarriedFrom
		if pu.Status == wbs.WorkUnitStatusDONE {
			u.CompletedAt = &now
			if pu.CarriedFrom != nil {
				var run *uuid.UUID
				_ = ex.QueryRow(ctx, `SELECT done_validation_run_id FROM work_units WHERE id = $1`, *pu.CarriedFrom).Scan(&run)
				u.DoneValidationRunID = run
			}
		}
		if err := wus.Create(ctx, u); err != nil {
			return nil, err
		}
		ids[pu.Key] = u.ID
		out = append(out, u)
	}
	for _, pu := range units {
		for _, d := range pu.DependsOnKeys {
			did, ok := ids[d]
			if !ok {
				return nil, fmt.Errorf("dbrepo: unknown dependency key %q", d)
			}
			if err := wus.AddDependency(ctx, ids[pu.Key], did); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// SupersedePlans marks every non-terminal plan of the phase except one as
// SUPERSEDED (historical rows are otherwise untouched, WBS 16.67).
func (r *EngineRepository) SupersedePlans(ctx context.Context, ex persistence.Executor, phaseID uuid.UUID, except uuid.UUID) error {
	if _, err := r.ex(ex).Exec(ctx, `UPDATE execution_plans SET status = 'SUPERSEDED', updated_at = now()
		WHERE phase_id = $1 AND id <> $2 AND status <> 'SUPERSEDED'`, phaseID, except); err != nil {
		return TranslateDBError("supersede plans", err)
	}
	return nil
}

// ListPhasePlans lists a phase's plans by version.
func (r *EngineRepository) ListPhasePlans(ctx context.Context, ex persistence.Executor, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error) {
	return NewPlanRepositoryForExecutor(r.ex(ex)).ListByPhase(ctx, phaseID)
}
