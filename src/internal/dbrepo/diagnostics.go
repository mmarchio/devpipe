package dbrepo

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// Diagnostics (P18-73, WBS 18.185–18.187): bounded, read-only views made
// of identifiers, statuses and hashes — large content is referenced, never
// included.

// DiagnosticsRepository reads diagnostic facts.
type DiagnosticsRepository struct{ pool *pgxpool.Pool }

// NewDiagnosticsRepository binds the repository.
func NewDiagnosticsRepository(pool *pgxpool.Pool) *DiagnosticsRepository {
	return &DiagnosticsRepository{pool: pool}
}

// diagRows runs a query whose single column is a JSON object per row.
func diagRows(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]map[string]any, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("diagnostics", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, TranslateDBError("diagnostics", err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out, tr("diagnostics", rows.Err())
}

// System returns the system facts: executions (id / project / status /
// recovery condition), bounded to limit.
func (r *DiagnosticsRepository) System(ctx context.Context, limit int) (*persistence.SystemDiagnostics, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, TranslateDBError("diagnostics", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	d := &persistence.SystemDiagnostics{}
	if d.Executions, err = diagRows(ctx, tx, `SELECT json_build_object('id', id, 'project_id', project_id, 'status', status,
		'recovery_condition', COALESCE(recovery_evidence->>'condition', ''), 'wait_reason', COALESCE(wait_reason, ''), 'updated_at', updated_at)
		FROM executions ORDER BY (status IN ('RUNNING', 'WAITING', 'RECOVERY_REQUIRED')) DESC, updated_at DESC LIMIT $1`, limit); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM executions`).Scan(&d.ExecutionCount); err != nil {
		return nil, TranslateDBError("diagnostics", err)
	}
	if d.RecoveryConditions, err = diagRows(ctx, tx, `SELECT json_build_object('condition', c, 'executions', n) FROM (
		SELECT COALESCE(NULLIF(recovery_evidence->>'condition', ''), 'UNCLASSIFIED') AS c, count(*) AS n
		FROM executions WHERE status = 'RECOVERY_REQUIRED' GROUP BY 1) x ORDER BY c`); err != nil {
		return nil, err
	}
	return d, nil
}

// Project returns one project's facts, each list bounded to limit.
func (r *DiagnosticsRepository) Project(ctx context.Context, projectID uuid.UUID, limit int) (*persistence.ProjectDiagnostics, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, TranslateDBError("diagnostics", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	d := &persistence.ProjectDiagnostics{ProjectID: projectID.String()}
	if err := tx.QueryRow(ctx, `SELECT name, status FROM projects WHERE id = $1`, projectID).Scan(&d.Name, &d.Status); err != nil {
		return nil, TranslateDBError("diagnostics project", err)
	}
	queries := []struct {
		dst *[]map[string]any
		sql string
	}{
		{&d.Executions, `SELECT json_build_object('id', id, 'status', status, 'recovery_condition', COALESCE(recovery_evidence->>'condition', ''),
			'current_phase_id', current_phase_id, 'current_work_unit_id', current_work_unit_id, 'version', version)
			FROM executions WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`},
		{&d.Phases, `SELECT json_build_object('id', id, 'order', phase_order, 'status', status) FROM phases WHERE project_id = $1 ORDER BY phase_order LIMIT $2`},
		{&d.Plans, `SELECT json_build_object('id', ep.id, 'phase_id', ep.phase_id, 'version', ep.version, 'status', ep.status, 'wbs_version', ep.wbs_version,
			'wbs_hash', COALESCE((SELECT content_hash FROM wbs_versions v WHERE v.phase_id = ep.phase_id AND v.version = ep.wbs_version), ''))
			FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p.project_id = $1 ORDER BY ep.created_at DESC LIMIT $2`},
		{&d.Units, `SELECT json_build_object('id', w.id, 'unit_key', w.unit_key, 'status', w.status, 'plan_id', w.execution_plan_id,
			'objective_hash', encode(sha256(convert_to(w.objective, 'UTF8')), 'hex'))
			FROM work_units w WHERE w.project_id = $1 ORDER BY w.created_at DESC LIMIT $2`},
		{&d.Dispatches, `SELECT json_build_object('id', id, 'work_unit_id', work_unit_id, 'status', status, 'attempt', attempt, 'delivery', delivery,
			'error_code', error_code, 'request_id', request_id, 'correlation_id', correlation_id, 'worker_prompt_id', worker_prompt_id,
			'prompt_hash', prompt_hash, 'created_at', created_at)
			FROM worker_dispatches WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`},
		{&d.Validations, `SELECT json_build_object('id', id, 'work_unit_id', work_unit_id, 'status', status, 'attempt', attempt, 'correlation_id', correlation_id)
			FROM validation_runs WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2`},
		{&d.Checkpoints, `SELECT json_build_object('id', id, 'kind', kind, 'target_id', target_id, 'status', status, 'commit_sha', commit_sha)
			FROM repository_checkpoints WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`},
		{&d.EventTail, `SELECT json_build_object('seq', seq, 'type', type, 'execution_id', execution_id, 'correlation_id', payload->>'correlation_id',
			'at', created_at) FROM execution_events WHERE project_id = $1 ORDER BY seq DESC LIMIT $2`},
	}
	for _, q := range queries {
		if *q.dst, err = diagRows(ctx, tx, q.sql, projectID, limit); err != nil {
			return nil, err
		}
	}
	return d, nil
}
