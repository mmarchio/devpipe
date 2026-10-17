package dbrepo

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/invariants"
)

// IntegrityScanner is the read-only database invariant scanner (WBS 18.39 /
// 18.40 / 18.171 / 18.181): one bounded query per catalog invariant, all in
// one REPEATABLE READ READ ONLY transaction. It reports; it never repairs.
type IntegrityScanner struct{ pool *pgxpool.Pool }

// NewIntegrityScanner builds the scanner.
func NewIntegrityScanner(pool *pgxpool.Pool) *IntegrityScanner { return &IntegrityScanner{pool: pool} }

// IntegrityScope narrows a scan (nil = all).
type IntegrityScope struct {
	ProjectID   *uuid.UUID
	ExecutionID *uuid.UUID
}

// integrityCheck is one invariant query. It returns rows of
// (project_id, execution_id, entity ids, detail) and receives
// $1 = project filter, $2 = execution filter (NULL = no filter).
type integrityCheck struct {
	id  invariants.ID
	sql string
}

const activeExec = `('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED')`

var integrityChecks = []integrityCheck{
	{invariants.OneActiveExecution, `SELECT project_id::text, '', array_agg(id::text), count(*) || ' active executions'
		FROM executions WHERE status IN ` + activeExec + ` AND ($1::uuid IS NULL OR project_id = $1)
		GROUP BY project_id HAVING count(*) > 1`},
	{invariants.OneCurrentUnit, `SELECT min(project_id::text), claimed_execution_id::text, array_agg(id::text), count(*) || ' claimed units'
		FROM work_units WHERE claimed_execution_id IS NOT NULL AND status IN ('EXECUTING', 'INCOMPLETE', 'VALIDATING')
		AND ($1::uuid IS NULL OR project_id = $1) AND ($2::uuid IS NULL OR claimed_execution_id = $2)
		GROUP BY claimed_execution_id HAVING count(*) > 1`},
	{invariants.OneActiveDispatch, `SELECT project_id::text, '', array_agg(id::text), count(*) || ' in-flight dispatches'
		FROM worker_dispatches WHERE status IN ('PENDING', 'SENDING', 'WAITING') AND ($1::uuid IS NULL OR project_id = $1)
		GROUP BY project_id HAVING count(*) > 1`},
	{invariants.OneActiveContinuationChain, `SELECT min(project_id::text), '', array_agg(id::text), count(*) || ' active continuations for one unit'
		FROM continuations WHERE status NOT IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED')
		AND ($1::uuid IS NULL OR project_id = $1) GROUP BY work_unit_id HAVING count(*) > 1`},
	{invariants.OneCurrentPhase, `SELECT project_id::text, '', array_agg(id::text), count(*) || ' executing / validating phases'
		FROM phases WHERE status IN ('EXECUTING', 'VALIDATING') AND ($1::uuid IS NULL OR project_id = $1)
		GROUP BY project_id HAVING count(*) > 1`},
	// A DONE unit either ran (claimed by an execution: it needs its own
	// PASS run) or was carried into an amended plan from a DONE unit; a
	// DONE unit that did neither was never executed.
	{invariants.DoneRequiresValidation, `SELECT w.project_id::text, COALESCE(w.claimed_execution_id::text, ''), ARRAY[w.id::text],
		CASE WHEN w.claimed_execution_id IS NULL THEN 'DONE without having run (not claimed, not carried from a DONE unit)'
			ELSE 'DONE without a passing validation run' END
		FROM work_units w LEFT JOIN validation_runs v ON v.id = w.done_validation_run_id
		WHERE w.status = 'DONE' AND (
			(w.claimed_execution_id IS NOT NULL AND (v.id IS NULL OR v.status <> 'PASS'))
			OR (w.claimed_execution_id IS NULL AND NOT EXISTS (SELECT 1 FROM work_units src WHERE src.id = w.carried_from_unit_id AND src.status = 'DONE')))
		AND ($1::uuid IS NULL OR w.project_id = $1) AND ($2::uuid IS NULL OR w.claimed_execution_id = $2 OR w.claimed_execution_id IS NULL)`},
	// The checkpoint of a carried unit is the one of the unit it was
	// carried from (followed through successive amendments).
	{invariants.DoneRequiresCheckpoint, `SELECT w.project_id::text, COALESCE(w.claimed_execution_id::text, ''), ARRAY[w.id::text], 'DONE without a created checkpoint under the REQUIRED policy'
		FROM work_units w LEFT JOIN project_engine_settings s ON s.project_id = w.project_id
		WHERE w.status = 'DONE' AND COALESCE(s.checkpoint_policy, 'REQUIRED') = 'REQUIRED'
		AND NOT EXISTS (
			WITH RECURSIVE lineage(id, src, depth) AS (
				SELECT w.id, w.carried_from_unit_id, 0
				UNION ALL SELECT u.id, u.carried_from_unit_id, l.depth + 1 FROM work_units u JOIN lineage l ON u.id = l.src WHERE l.depth < 32)
			SELECT 1 FROM repository_checkpoints c JOIN lineage l ON c.target_id = l.id WHERE c.kind = 'UNIT' AND c.status = 'CREATED')
		AND ($1::uuid IS NULL OR w.project_id = $1) AND ($2::uuid IS NULL OR w.claimed_execution_id = $2 OR w.claimed_execution_id IS NULL)`},
	{invariants.CurrentPlanMatchesWBS, `SELECT p.project_id::text, '', ARRAY[ep.id::text], 'active plan v' || ep.version || ' planned from WBS v' || ep.wbs_version || ', current WBS v' || d.version
		FROM phases p JOIN execution_plans ep ON ep.phase_id = p.id AND ep.status IN ('READY', 'EXECUTING')
		JOIN phase_documents d ON d.phase_id = p.id AND d.doc_type = 'WBS'
		WHERE p.status IN ('EXECUTING', 'VALIDATING') AND ep.wbs_version < d.version AND ($1::uuid IS NULL OR p.project_id = $1)`},
	{invariants.CurrentUnitOnActivePlan, `SELECT e.project_id::text, e.id::text, ARRAY[w.id::text], 'current unit belongs to a superseded plan'
		FROM executions e JOIN work_units w ON w.id = e.current_work_unit_id JOIN execution_plans ep ON ep.id = w.execution_plan_id
		WHERE e.status IN ` + activeExec + ` AND ep.status = 'SUPERSEDED'
		AND ($1::uuid IS NULL OR e.project_id = $1) AND ($2::uuid IS NULL OR e.id = $2)`},
	{invariants.RuntimeBindingValid, `SELECT e.project_id::text, e.id::text, ARRAY[COALESCE(rt.id::text, ''), COALESCE(p.id::text, ''), COALESCE(w.id::text, '')],
		'runtime state / current phase / current unit of another project'
		FROM executions e LEFT JOIN runtime_state rt ON rt.id = e.runtime_state_id LEFT JOIN phases p ON p.id = e.current_phase_id
		LEFT JOIN work_units w ON w.id = e.current_work_unit_id
		WHERE ((rt.id IS NOT NULL AND rt.project_id <> e.project_id) OR (p.id IS NOT NULL AND p.project_id <> e.project_id)
			OR (w.id IS NOT NULL AND w.project_id <> e.project_id))
		AND ($1::uuid IS NULL OR e.project_id = $1) AND ($2::uuid IS NULL OR e.id = $2)`},
	{invariants.NoOrphans, `SELECT o.project_id, o.execution_id, ARRAY[o.id], o.detail FROM (
		SELECT s.project_id::text, s.execution_id::text, s.id::text, 'execution step of a missing execution' AS detail
			FROM execution_steps s WHERE NOT EXISTS (SELECT 1 FROM executions e WHERE e.id = s.execution_id)
		UNION ALL SELECT v.project_id::text, v.execution_id::text, v.id::text, 'validation run of a missing execution'
			FROM validation_runs v WHERE NOT EXISTS (SELECT 1 FROM executions e WHERE e.id = v.execution_id)
		UNION ALL SELECT c.project_id::text, c.execution_id::text, c.id::text, 'checkpoint of a missing execution'
			FROM repository_checkpoints c WHERE NOT EXISTS (SELECT 1 FROM executions e WHERE e.id = c.execution_id)
		UNION ALL SELECT w.project_id::text, '', w.id::text, 'work unit of a missing project'
			FROM work_units w WHERE NOT EXISTS (SELECT 1 FROM projects p WHERE p.id = w.project_id)
		UNION ALL SELECT d.project_id::text, '', d.id::text, 'dispatch of a missing work unit'
			FROM worker_dispatches d WHERE d.work_unit_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM work_units w WHERE w.id = d.work_unit_id)
		) o WHERE ($1::uuid IS NULL OR o.project_id = $1::text) AND ($2::uuid IS NULL OR o.execution_id = $2::text OR o.execution_id = '')`},
	{invariants.DependencyRefsValid, `SELECT w.project_id::text, '', ARRAY[w.id::text, x.id::text], 'dependency on a unit of another plan'
		FROM work_unit_dependencies dep JOIN work_units w ON w.id = dep.work_unit_id JOIN work_units x ON x.id = dep.depends_on_work_unit_id
		WHERE x.execution_plan_id <> w.execution_plan_id AND ($1::uuid IS NULL OR w.project_id = $1)`},
	{invariants.CheckpointConsistent, `SELECT c.project_id::text, c.execution_id::text, ARRAY[c.id::text], 'checkpoint of another project''s execution'
		FROM repository_checkpoints c JOIN executions e ON e.id = c.execution_id WHERE e.project_id <> c.project_id
		AND ($1::uuid IS NULL OR c.project_id = $1) AND ($2::uuid IS NULL OR c.execution_id = $2)`},
	{invariants.OwnershipConsistent, `SELECT o.project_id, o.execution_id, o.ids, o.detail FROM (
		SELECT d.project_id::text, '' AS execution_id, ARRAY[d.id::text, w.id::text] AS ids, 'dispatch references another project''s unit' AS detail
			FROM worker_dispatches d JOIN work_units w ON w.id = d.work_unit_id WHERE w.project_id <> d.project_id
		UNION ALL SELECT s.project_id::text, s.execution_id::text, ARRAY[s.id::text], 'step of another project''s execution'
			FROM execution_steps s JOIN executions e ON e.id = s.execution_id WHERE e.project_id <> s.project_id
		UNION ALL SELECT v.project_id::text, v.execution_id::text, ARRAY[v.id::text, w.id::text], 'validation run for another project''s unit'
			FROM validation_runs v JOIN work_units w ON w.id = v.work_unit_id WHERE w.project_id <> v.project_id
		UNION ALL SELECT r.project_id::text, '', ARRAY[r.id::text, d.id::text], 'worker result of another project''s dispatch'
			FROM worker_results r JOIN worker_dispatches d ON d.id = r.dispatch_id WHERE d.project_id <> r.project_id
		UNION ALL SELECT d.project_id::text, '', ARRAY[d.id::text, ws.id::text], 'dispatch sent on another project''s worker session'
			FROM worker_dispatches d JOIN worker_sessions ws ON ws.id = d.session_id WHERE ws.project_id <> d.project_id
		UNION ALL SELECT w.project_id::text, '', ARRAY[w.id::text, ep.id::text], 'work unit in another project''s plan'
			FROM work_units w JOIN execution_plans ep ON ep.id = w.execution_plan_id JOIN phases p ON p.id = ep.phase_id WHERE p.project_id <> w.project_id
		) o WHERE ($1::uuid IS NULL OR o.project_id = $1::text) AND ($2::uuid IS NULL OR o.execution_id = $2::text OR o.execution_id = '')`},
}

// Scan runs every invariant query in one read-only snapshot.
func (s *IntegrityScanner) Scan(ctx context.Context, scope IntegrityScope) ([]invariants.ID, []invariants.Violation, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, TranslateDBError("integrity scan", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var checked []invariants.ID
	var out []invariants.Violation
	for _, c := range integrityChecks {
		args := []any{scope.ProjectID}
		if strings.Contains(c.sql, "$2") {
			args = append(args, scope.ExecutionID)
		}
		rows, err := tx.Query(ctx, c.sql, args...)
		if err != nil {
			return nil, nil, TranslateDBError("integrity scan "+string(c.id), err)
		}
		for rows.Next() {
			var v invariants.Violation
			if err := rows.Scan(&v.ProjectID, &v.ExecutionID, &v.EntityIDs, &v.Detail); err != nil {
				rows.Close()
				return nil, nil, TranslateDBError("integrity scan "+string(c.id), err)
			}
			v.Invariant = c.id
			out = append(out, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, TranslateDBError("integrity scan "+string(c.id), err)
		}
		checked = append(checked, c.id)
	}
	return checked, out, nil
}

// CreatedCheckpoints lists created checkpoints (id, project, execution,
// commit) in scope, for the repository-object check (WBS 18.203).
func (s *IntegrityScanner) CreatedCheckpoints(ctx context.Context, scope IntegrityScope) ([][4]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, project_id::text, execution_id::text, commit_sha FROM repository_checkpoints
		WHERE status = 'CREATED' AND commit_sha IS NOT NULL AND ($1::uuid IS NULL OR project_id = $1) AND ($2::uuid IS NULL OR execution_id = $2)
		ORDER BY created_at, id`, scope.ProjectID, scope.ExecutionID)
	if err != nil {
		return nil, TranslateDBError("integrity checkpoints", err)
	}
	defer rows.Close()
	var out [][4]string
	for rows.Next() {
		var r [4]string
		if err := rows.Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
			return nil, TranslateDBError("integrity checkpoints", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExecutionsAfter returns up to limit execution ids ordered by id after
// the cursor (uuid.Nil = from the start) — the periodic integrity scan's
// round-robin batch (WBS 18.180).
func (s *IntegrityScanner) ExecutionsAfter(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM executions WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, TranslateDBError("list executions for integrity scan", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, TranslateDBError("list executions for integrity scan", err)
		}
		out = append(out, id)
	}
	return out, tr("list executions for integrity scan", rows.Err())
}
