package dbrepo

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/execution"
	"overseer/internal/persistence"
)

// ---------------------------------------------------------------------------
// Audit (P17-27).

// AuditRepository implements persistence.AuditRepository.
type AuditRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.AuditRepository = (*AuditRepository)(nil)

// NewAuditRepository binds the repository to a pool.
func NewAuditRepository(pool *pgxpool.Pool) *AuditRepository {
	return &AuditRepository{exec: poolAdapter{pool}, pool: pool}
}

const auditColumns = `id, seq, created_at, project_id, project_name, execution_id, phase_id, work_unit_id, actor_id, actor_name,
	actor_type, action, resource_kind, resource_id, result, http_status, reason, request_id, method, path, metadata`

// AppendAudit inserts one event as the next link of its hash chain
// (P18-56): inside ex when given, else in its own transaction.
func (r *AuditRepository) AppendAudit(ctx context.Context, ex persistence.Executor, e *audit.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	// Hashed exactly as stored (WBS 18.107).
	e.At = e.At.UTC().Truncate(audit.StoredPrecision)
	if ex != nil {
		return r.appendChained(ctx, ex, e)
	}
	return Run(ctx, r.pool, func(tx persistence.Transaction) error {
		return r.appendChained(ctx, tx, e)
	})
}

func scanAudit(row interface{ Scan(...any) error }) (*audit.Event, error) {
	var e audit.Event
	var actorType, action, result string
	var md []byte
	if err := row.Scan(&e.ID, &e.Seq, &e.At, &e.ProjectID, &e.ProjectName, &e.ExecutionID, &e.PhaseID, &e.WorkUnitID,
		&e.Actor.ID, &e.Actor.Name, &actorType, &action, &e.ResourceKind, &e.ResourceID, &result, &e.HTTPStatus, &e.Reason,
		&e.RequestID, &e.Method, &e.Path, &md); err != nil {
		return nil, err
	}
	e.Actor.Type, e.Result = accessType(actorType), audit.Result(result)
	e.Action = auditAction(action)
	_ = json.Unmarshal(md, &e.Metadata)
	return &e, nil
}

// ListAudit returns one keyset page (seq DESC).
func (r *AuditRepository) ListAudit(ctx context.Context, f audit.Filter) ([]audit.Event, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+itoa(len(args))))
	}
	if f.ProjectID != nil {
		add("project_id = ?", *f.ProjectID)
	}
	if f.ExecutionID != nil {
		add("execution_id = ?", *f.ExecutionID)
	}
	if f.PhaseID != nil {
		add("phase_id = ?", *f.PhaseID)
	}
	if f.WorkUnitID != nil {
		add("work_unit_id = ?", *f.WorkUnitID)
	}
	if f.Action != "" {
		add("action = ?", string(f.Action))
	}
	if f.Result != "" {
		add("result = ?", string(f.Result))
	}
	if f.From != nil {
		add("created_at >= ?", f.From.UTC())
	}
	if f.To != nil {
		add("created_at < ?", f.To.UTC())
	}
	if f.BeforeSeq > 0 {
		add("seq < ?", f.BeforeSeq)
	}
	q := `SELECT ` + auditColumns + ` FROM audit_events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)
	q += " ORDER BY seq DESC LIMIT $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, tr("list audit events", err)
	}
	defer rows.Close()
	out := []audit.Event{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, tr("scan audit event", err)
		}
		out = append(out, *e)
	}
	return out, tr("list audit events", rows.Err())
}

// GetAudit returns one event.
func (r *AuditRepository) GetAudit(ctx context.Context, id uuid.UUID) (*audit.Event, error) {
	e, err := scanAudit(r.exec.QueryRow(ctx, `SELECT `+auditColumns+` FROM audit_events WHERE id = $1`, id))
	if err != nil {
		return nil, tr("get audit event", err)
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Drift acknowledgements (P17-38).

// DriftAckRepository implements persistence.DriftAckRepository.
type DriftAckRepository struct{ exec executor }

var _ persistence.DriftAckRepository = (*DriftAckRepository)(nil)

// NewDriftAckRepository binds the repository to a pool.
func NewDriftAckRepository(pool *pgxpool.Pool) *DriftAckRepository {
	return &DriftAckRepository{exec: poolAdapter{pool}}
}

// AcknowledgeDrift records (or returns the existing) acknowledgement.
func (r *DriftAckRepository) AcknowledgeDrift(ctx context.Context, a *persistence.DriftAcknowledgement) (*persistence.DriftAcknowledgement, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := r.exec.Exec(ctx, `INSERT INTO state_file_drift_acknowledgements (id, project_id, state_file_id, fs_hash, acknowledged_by, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (state_file_id, fs_hash) DO NOTHING`,
		a.ID, a.ProjectID, a.StateFileID, a.FSHash, a.AcknowledgedBy, a.Reason, a.CreatedAt)
	if err != nil {
		return nil, tr("acknowledge drift", err)
	}
	return r.GetDriftAck(ctx, a.StateFileID, a.FSHash)
}

// GetDriftAck returns one acknowledgement.
func (r *DriftAckRepository) GetDriftAck(ctx context.Context, stateFileID uuid.UUID, fsHash string) (*persistence.DriftAcknowledgement, error) {
	var a persistence.DriftAcknowledgement
	err := r.exec.QueryRow(ctx, `SELECT id, project_id, state_file_id, fs_hash, acknowledged_by, reason, created_at
		FROM state_file_drift_acknowledgements WHERE state_file_id = $1 AND fs_hash = $2`, stateFileID, fsHash).
		Scan(&a.ID, &a.ProjectID, &a.StateFileID, &a.FSHash, &a.AcknowledgedBy, &a.Reason, &a.CreatedAt)
	if err != nil {
		return nil, tr("get drift acknowledgement", err)
	}
	return &a, nil
}

// ---------------------------------------------------------------------------
// Web read models (P17-29 .. P17-37).

// WebReadRepository implements persistence.WebReadRepository.
type WebReadRepository struct{ exec executor }

var _ persistence.WebReadRepository = (*WebReadRepository)(nil)

// NewWebReadRepository binds the repository to a pool.
func NewWebReadRepository(pool *pgxpool.Pool) *WebReadRepository {
	return &WebReadRepository{exec: poolAdapter{pool}}
}

// activeStatuses is the SQL list of active execution statuses.
const activeStatuses = `('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED')`

func scanEvents(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]execution.Event, error) {
	out := []execution.Event{}
	for rows.Next() {
		var e execution.Event
		var typ string
		var payload []byte
		var execSeq *int64
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ExecutionID, &typ, &payload, &e.CreatedAt, &execSeq, &e.ProjectSeq); err != nil {
			return nil, tr("scan event", err)
		}
		if execSeq != nil {
			e.ExecSeq = *execSeq
		}
		e.Type = execution.EventType(typ)
		_ = json.Unmarshal(payload, &e.Payload)
		out = append(out, e)
	}
	return out, tr("scan events", rows.Err())
}

const eventColumns = `id, project_id, execution_id, type, payload, created_at, exec_seq, project_seq`

// ListProjectEventsAfter lists events after a project sequence.
func (r *WebReadRepository) ListProjectEventsAfter(ctx context.Context, projectID uuid.UUID, afterSeq int64, limit int) ([]execution.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := r.exec.Query(ctx, `SELECT `+eventColumns+` FROM execution_events
		WHERE project_id = $1 AND project_seq > $2 ORDER BY project_seq LIMIT $3`, projectID, afterSeq, limit)
	if err != nil {
		return nil, tr("list project events", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// MaxProjectSeq returns the latest project sequence.
func (r *WebReadRepository) MaxProjectSeq(ctx context.Context, projectID uuid.UUID) (int64, error) {
	var n int64
	err := r.exec.QueryRow(ctx, `SELECT COALESCE(MAX(project_seq), 0) FROM execution_events WHERE project_id = $1`, projectID).Scan(&n)
	return n, tr("max project seq", err)
}

// DashboardCounts aggregates in two queries regardless of project count.
func (r *WebReadRepository) DashboardCounts(ctx context.Context) (persistence.DashboardCounts, error) {
	c := persistence.DashboardCounts{ProjectsByStatus: map[string]int{}}
	rows, err := r.exec.Query(ctx, `SELECT status, count(*) FROM projects GROUP BY status`)
	if err != nil {
		return c, tr("dashboard project counts", err)
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return c, tr("scan dashboard counts", err)
		}
		c.ProjectsByStatus[s] = n
		c.ProjectsTotal += n
	}
	rows.Close()
	err = r.exec.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM executions WHERE status = 'RECOVERY_REQUIRED'),
		(SELECT count(*) FROM executions WHERE status IN `+activeStatuses+`),
		(SELECT count(*) FROM worker_sessions WHERE status = 'ACTIVE'),
		(SELECT count(*) FROM worker_dispatches WHERE status IN ('PENDING', 'SENDING', 'WAITING')),
		(SELECT count(*) FROM validation_runs WHERE status = 'RUNNING')`).
		Scan(&c.RecoveryRequired, &c.ActiveExecutions, &c.ActiveSessions, &c.ActiveDispatches, &c.RunningValidations)
	return c, tr("dashboard activity counts", err)
}

// projectFacts is the shared lateral block computing each project's latest
// execution, current phase, its current plan + counts, the runtime state
// and the current unit.
const projectFacts = `
	LEFT JOIN LATERAL (SELECT e.id, e.number, e.status, e.generation, e.current_phase_id, e.current_work_unit_id,
			e.wait_reason, e.recovery_reason, e.recovery_evidence, e.running_at, e.completed_at,
			e.status IN ` + activeStatuses + ` AS active
		FROM executions e WHERE e.project_id = p.id ORDER BY e.number DESC LIMIT 1) ex ON true
	LEFT JOIN LATERAL (SELECT ph.id, ph.phase_order, ph.name, ph.status FROM phases ph
		WHERE ph.project_id = p.id AND (NOT (COALESCE(ex.active, false) AND ex.current_phase_id IS NOT NULL) OR ph.id = ex.current_phase_id)
		-- The execution's phase while active; else the first non-DONE phase;
		-- when every phase is DONE, the last phase.
		ORDER BY (ph.status = 'DONE'), CASE WHEN ph.status = 'DONE' THEN -ph.phase_order ELSE ph.phase_order END LIMIT 1) cp ON true
	LEFT JOIN LATERAL (SELECT pl.id, pl.version, pl.wbs_version, pl.status, pl.kind FROM execution_plans pl
		WHERE pl.phase_id = cp.id AND pl.status <> 'SUPERSEDED' ORDER BY pl.version DESC LIMIT 1) pl ON true
	LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE w.status = 'DONE') AS done, count(*) AS total
		FROM work_units w WHERE w.execution_plan_id = pl.id) uc ON true
	LEFT JOIN runtime_state rs ON rs.project_id = p.id
	LEFT JOIN workflow_states ws ON ws.id = rs.current_state_id
	LEFT JOIN work_units cu ON cu.id = COALESCE(CASE WHEN ex.active THEN ex.current_work_unit_id END, rs.work_unit_id)`

// ProjectListPage returns one filtered page in a single query.
func (r *WebReadRepository) ProjectListPage(ctx context.Context, f persistence.ProjectPageFilter) ([]persistence.ProjectListRow, int, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+itoa(len(args))))
	}
	if f.Status != "" {
		add("p.status = ?", f.Status)
	}
	if f.Name != "" {
		add("p.name ILIKE ?", "%"+escapeLike(f.Name)+"%")
	}
	if f.Active != nil {
		if *f.Active {
			where = append(where, "COALESCE(ex.active, false)")
		} else {
			where = append(where, "NOT COALESCE(ex.active, false)")
		}
	}
	if f.Recovery {
		where = append(where, "ex.status = 'RECOVERY_REQUIRED'")
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	from := ` FROM projects p ` + projectFacts + `
		LEFT JOIN LATERAL (SELECT created_at FROM execution_events ev WHERE ev.project_id = p.id ORDER BY project_seq DESC LIMIT 1) le ON true`
	if len(where) > 0 {
		from += " WHERE " + strings.Join(where, " AND ")
	}
	filterArgs := append([]any(nil), args...)
	args = append(args, limit, offset)
	q := `SELECT p.id, p.name, p.status, p.updated_at, cp.id, COALESCE(cp.phase_order, 0), COALESCE(cp.name, ''), COALESCE(cp.status, ''),
		COALESCE(uc.done, 0), COALESCE(uc.total, 0), COALESCE(ws.key, ''), cu.id, COALESCE(cu.unit_key, ''), COALESCE(cu.name, ''),
		COALESCE(ex.status, ''), GREATEST(p.updated_at, COALESCE(le.created_at, p.updated_at)),
		COALESCE(ex.active, false), COALESCE(ex.status = 'RECOVERY_REQUIRED', false), count(*) OVER ()` + from +
		" ORDER BY p.created_at DESC, p.id LIMIT $" + itoa(len(args)-1) + " OFFSET $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, tr("project list page", err)
	}
	defer rows.Close()
	out := []persistence.ProjectListRow{}
	total := 0
	for rows.Next() {
		var x persistence.ProjectListRow
		if err := rows.Scan(&x.ID, &x.Name, &x.Status, &x.UpdatedAt, &x.PhaseID, &x.PhaseOrder, &x.PhaseName, &x.PhaseStatus,
			&x.Done, &x.Total, &x.StateKey, &x.UnitID, &x.UnitKey, &x.UnitName, &x.ExecutionStatus, &x.LastActivity,
			&x.ActiveExecution, &x.RecoveryRequired, &total); err != nil {
			return nil, 0, tr("scan project row", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, tr("project list page", err)
	}
	if len(out) == 0 && offset > 0 {
		// The window count is unavailable on an empty page past the end.
		if err := r.exec.QueryRow(ctx, `SELECT count(*)`+from, filterArgs...).Scan(&total); err != nil {
			return nil, 0, tr("project list count", err)
		}
	}
	return out, total, nil
}

// escapeLike escapes LIKE metacharacters in user input.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ProjectOverview returns the overview facts in one query.
func (r *WebReadRepository) ProjectOverview(ctx context.Context, projectID uuid.UUID) (*persistence.ProjectOverviewRow, error) {
	var x persistence.ProjectOverviewRow
	var evidence []byte
	var active *bool
	err := r.exec.QueryRow(ctx, `SELECT p.id, p.name, p.description, p.status, p.version, p.updated_at,
		ex.id, COALESCE(ex.number, 0), COALESCE(ex.status, ''), COALESCE(ex.generation, 0), COALESCE(ex.wait_reason, ''),
		COALESCE(ex.recovery_reason, ''), ex.recovery_evidence, ex.active, ex.running_at, ex.completed_at,
		cp.id, COALESCE(cp.phase_order, 0), COALESCE(cp.name, ''), COALESCE(cp.status, ''),
		COALESCE((SELECT d.current_version FROM phase_documents d WHERE d.phase_id = cp.id AND d.doc_type = 'WBS' LIMIT 1),
			(SELECT max(v.version) FROM wbs_versions v WHERE v.phase_id = cp.id), 0),
		(SELECT count(*) FROM phases a WHERE a.project_id = p.id), (SELECT count(*) FROM phases a WHERE a.project_id = p.id AND a.status = 'DONE'),
		pl.id, COALESCE(pl.version, 0), COALESCE(pl.wbs_version, 0), COALESCE(pl.status, ''), COALESCE(pl.kind, ''),
		COALESCE(uc.done, 0), COALESCE(uc.total, 0),
		cu.id, COALESCE(cu.unit_key, ''), COALESCE(cu.name, ''), COALESCE(cu.status, ''), COALESCE(cu.kind, ''), cu.started_at,
		COALESCE(ws.key, ''), COALESCE(ws.name, ''), COALESCE(ws.kind, ''), rs.entered_at,
		uo.context_tokens, uo.context_window, uo.observed_at,
		COALESCE((SELECT count(*) FROM worker_dispatches wd WHERE wd.work_unit_id = cu.id), 0),
		(SELECT s.id FROM worker_sessions s WHERE s.project_id = p.id AND s.status = 'ACTIVE' ORDER BY s.created_at DESC LIMIT 1),
		COALESCE((SELECT upper(r.status) FROM worker_results r WHERE r.work_unit_id = cu.id ORDER BY r.created_at DESC LIMIT 1), ''),
		COALESCE((SELECT v.status FROM validation_runs v WHERE v.work_unit_id = cu.id ORDER BY v.started_at DESC LIMIT 1), ''),
		COALESCE((SELECT c.status FROM repository_checkpoints c WHERE c.kind = 'UNIT' AND c.target_id = cu.id ORDER BY c.created_at DESC LIMIT 1), ''),
		EXISTS (SELECT 1 FROM continuations c WHERE c.work_unit_id = cu.id
			AND c.status NOT IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED'))
		FROM projects p `+projectFacts+`
		LEFT JOIN LATERAL (SELECT o.context_tokens, o.context_window, o.observed_at FROM worker_usage_observations o
			JOIN worker_sessions s ON s.id = o.session_id AND s.status = 'ACTIVE'
			WHERE o.project_id = p.id ORDER BY o.seq DESC LIMIT 1) uo ON true
		WHERE p.id = $1`, projectID).Scan(&x.ProjectID, &x.Name, &x.Description, &x.Status, &x.ProjectVersion, &x.UpdatedAt,
		&x.ExecutionID, &x.ExecutionNumber, &x.ExecutionStatus, &x.Generation, &x.WaitReason, &x.RecoveryReason, &evidence, &active,
		&x.ExecutionStarted, &x.ExecutionComplete,
		&x.PhaseID, &x.PhaseOrder, &x.PhaseName, &x.PhaseStatus, &x.WBSVersion, &x.PhasesTotal, &x.PhasesDone,
		&x.PlanID, &x.PlanVersion, &x.PlanWBS, &x.PlanStatus, &x.PlanKind, &x.UnitsDone, &x.UnitsTotal,
		&x.UnitID, &x.UnitKey, &x.UnitName, &x.UnitStatus, &x.UnitKind, &x.UnitStartedAt,
		&x.StateKey, &x.StateName, &x.StateKind, &x.StateEntered, &x.ContextTokens, &x.ContextWindow, &x.ContextAt,
		&x.UnitAttempts, &x.SessionID, &x.UnitWorkerResult, &x.UnitValidation, &x.UnitCheckpoint, &x.ContinuationActive)
	if err != nil {
		return nil, tr("project overview", err)
	}
	x.ExecutionActive = active != nil && *active
	if len(evidence) > 0 {
		_ = json.Unmarshal(evidence, &x.RecoveryEvidence)
	}
	return &x, nil
}

// WorkUnitPage returns one filtered page of a plan's units in one query.
func (r *WebReadRepository) WorkUnitPage(ctx context.Context, f persistence.WorkUnitPageFilter) ([]persistence.WorkUnitRow, int, error) {
	args := []any{f.PlanID}
	var where []string
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+itoa(len(args))))
	}
	if f.Status != "" {
		add("u.status = ?", f.Status)
	}
	if f.Kind != "" {
		add("u.kind = ?", f.Kind)
	}
	switch f.Readiness {
	case "READY":
		where = append(where, "u.status <> 'DONE' AND cardinality(u.blocking) = 0")
	case "BLOCKED":
		where = append(where, "cardinality(u.blocking) > 0")
	}
	if f.Validation != "" {
		add("u.vstatus = ?", f.Validation)
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := `WITH u AS (SELECT w.id, w.sequence, w.unit_key, w.kind, w.name, w.status, w.started_at, w.completed_at,
			ARRAY(SELECT COALESCE(NULLIF(d2.unit_key, ''), '#' || d2.sequence::text) FROM work_unit_dependencies d
				JOIN work_units d2 ON d2.id = d.depends_on_work_unit_id WHERE d.work_unit_id = w.id ORDER BY d2.sequence) AS deps,
			ARRAY(SELECT COALESCE(NULLIF(d2.unit_key, ''), '#' || d2.sequence::text) FROM work_unit_dependencies d
				JOIN work_units d2 ON d2.id = d.depends_on_work_unit_id WHERE d.work_unit_id = w.id AND d2.status <> 'DONE' ORDER BY d2.sequence) AS blocking,
			(SELECT count(*) FROM worker_dispatches wd WHERE wd.work_unit_id = w.id) AS attempts,
			COALESCE((SELECT vr.status FROM validation_runs vr WHERE vr.work_unit_id = w.id ORDER BY vr.started_at DESC LIMIT 1), '') AS vstatus,
			COALESCE((SELECT rc.status FROM repository_checkpoints rc WHERE rc.kind = 'UNIT' AND rc.target_id = w.id ORDER BY rc.created_at DESC LIMIT 1), '') AS cstatus
		FROM work_units w WHERE w.execution_plan_id = $1)
		SELECT id, sequence, unit_key, kind, name, status, deps, blocking, attempts, vstatus, cstatus, started_at, completed_at, count(*) OVER ()
		FROM u`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit, offset)
	q += " ORDER BY sequence LIMIT $" + itoa(len(args)-1) + " OFFSET $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, tr("work unit page", err)
	}
	defer rows.Close()
	out := []persistence.WorkUnitRow{}
	total := 0
	for rows.Next() {
		var x persistence.WorkUnitRow
		if err := rows.Scan(&x.ID, &x.Sequence, &x.Key, &x.Kind, &x.Name, &x.Status, &x.Deps, &x.Blocking, &x.Attempts,
			&x.Validation, &x.Checkpoint, &x.StartedAt, &x.CompletedAt, &total); err != nil {
			return nil, 0, tr("scan work unit row", err)
		}
		if x.Key == "" {
			x.Key = "#" + itoa(x.Sequence)
		}
		out = append(out, x)
	}
	return out, total, tr("work unit page", rows.Err())
}

// PlanKindCounts counts a plan's units per kind.
func (r *WebReadRepository) PlanKindCounts(ctx context.Context, planID uuid.UUID) (map[string]int, error) {
	rows, err := r.exec.Query(ctx, `SELECT kind, count(*) FROM work_units WHERE execution_plan_id = $1 GROUP BY kind`, planID)
	if err != nil {
		return nil, tr("plan kind counts", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, tr("scan kind count", err)
		}
		out[k] = n
	}
	return out, tr("plan kind counts", rows.Err())
}

// PlanEdges lists a plan's persisted dependency edges (from = dependency,
// to = dependent).
func (r *WebReadRepository) PlanEdges(ctx context.Context, planID uuid.UUID, limit, offset int) ([]persistence.PlanEdge, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.exec.Query(ctx, `SELECT f.id, COALESCE(NULLIF(f.unit_key, ''), '#' || f.sequence::text), t.id,
		COALESCE(NULLIF(t.unit_key, ''), '#' || t.sequence::text), count(*) OVER ()
		FROM work_unit_dependencies d JOIN work_units t ON t.id = d.work_unit_id JOIN work_units f ON f.id = d.depends_on_work_unit_id
		WHERE t.execution_plan_id = $1 ORDER BY t.sequence, f.sequence LIMIT $2 OFFSET $3`, planID, limit, offset)
	if err != nil {
		return nil, 0, tr("plan edges", err)
	}
	defer rows.Close()
	out := []persistence.PlanEdge{}
	total := 0
	for rows.Next() {
		var e persistence.PlanEdge
		if err := rows.Scan(&e.FromID, &e.FromKey, &e.ToID, &e.ToKey, &total); err != nil {
			return nil, 0, tr("scan plan edge", err)
		}
		out = append(out, e)
	}
	return out, total, tr("plan edges", rows.Err())
}

// ValidationOutputRange reads a character range of stdout / stderr.
func (r *WebReadRepository) ValidationOutputRange(ctx context.Context, projectID, resultID uuid.UUID, stream string, offset, length int) (*persistence.OutputRange, error) {
	col := ""
	switch stream {
	case "stdout":
		col = "c.stdout"
	case "stderr":
		col = "c.stderr"
	default:
		return nil, persistence.ErrInvalidReference
	}
	if offset < 0 {
		offset = 0
	}
	if length <= 0 || length > 64<<10 {
		length = 64 << 10
	}
	out := &persistence.OutputRange{Stream: stream, Offset: offset}
	err := r.exec.QueryRow(ctx, `SELECT substr(`+col+`, $3 + 1, $4), char_length(`+col+`)
		FROM validation_command_results c JOIN validation_runs v ON v.id = c.run_id
		WHERE c.id = $2 AND v.project_id = $1`, projectID, resultID, offset, length).Scan(&out.Text, &out.Total)
	if err != nil {
		return nil, tr("validation output range", err)
	}
	return out, nil
}

// TimelinePage returns one newest-first keyset page of an execution's
// events.
func (r *WebReadRepository) TimelinePage(ctx context.Context, f persistence.TimelineFilter) ([]execution.Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{f.ExecutionID}
	q := `SELECT ` + eventColumns + ` FROM execution_events WHERE execution_id = $1`
	if len(f.Types) > 0 {
		types := make([]string, len(f.Types))
		for i, t := range f.Types {
			types[i] = string(t)
		}
		args = append(args, types)
		q += " AND type = ANY($" + itoa(len(args)) + ")"
	}
	if f.BeforeSeq > 0 {
		args = append(args, f.BeforeSeq)
		q += " AND exec_seq < $" + itoa(len(args))
	}
	args = append(args, limit)
	q += " ORDER BY exec_seq DESC LIMIT $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, tr("timeline page", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// SchedulerCounts aggregates scheduler activity in one query.
func (r *WebReadRepository) SchedulerCounts(ctx context.Context) (persistence.SchedulerCounts, error) {
	var c persistence.SchedulerCounts
	err := r.exec.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM executions WHERE status = 'RUNNING'),
		(SELECT count(*) FROM executions WHERE status = 'WAITING'),
		(SELECT count(*) FROM worker_dispatches WHERE status IN ('PENDING', 'SENDING', 'WAITING')),
		(SELECT count(*) FROM validation_runs WHERE status = 'RUNNING')`).Scan(&c.Runnable, &c.Waiting, &c.ActiveDispatches, &c.RunningValidations)
	return c, tr("scheduler counts", err)
}

func accessType(s string) access.PrincipalType { return access.PrincipalType(s) }

func auditAction(s string) access.Action { return access.Action(s) }

// tr translates a database error, keeping nil as nil.
func tr(op string, err error) error {
	if err == nil {
		return nil
	}
	return TranslateDBError(op, err)
}

// ---------------------------------------------------------------------------
// Configuration version (P17-46).

// ConfigVersionRepository reads and bumps projects.config_version.
type ConfigVersionRepository struct{ exec executor }

// NewConfigVersionRepository binds the repository to a pool.
func NewConfigVersionRepository(pool *pgxpool.Pool) *ConfigVersionRepository {
	return &ConfigVersionRepository{exec: poolAdapter{pool}}
}

// GetConfigVersion returns the project's configuration version.
func (r *ConfigVersionRepository) GetConfigVersion(ctx context.Context, projectID uuid.UUID) (int, error) {
	var v int
	err := r.exec.QueryRow(ctx, `SELECT config_version FROM projects WHERE id = $1`, projectID).Scan(&v)
	return v, tr("get config version", err)
}

// BumpConfigVersion increments the version; with expected set it only
// succeeds while the stored version equals expected (ErrConflict
// otherwise). It returns the new version.
func (r *ConfigVersionRepository) BumpConfigVersion(ctx context.Context, projectID uuid.UUID, expected *int) (int, error) {
	var v int
	var err error
	if expected == nil {
		err = r.exec.QueryRow(ctx, `UPDATE projects SET config_version = config_version + 1 WHERE id = $1 RETURNING config_version`, projectID).Scan(&v)
	} else {
		err = r.exec.QueryRow(ctx, `UPDATE projects SET config_version = config_version + 1 WHERE id = $1 AND config_version = $2
			RETURNING config_version`, projectID, *expected).Scan(&v)
		if err != nil && tr("bump config version", err) == persistence.ErrNotFound {
			if _, gerr := r.GetConfigVersion(ctx, projectID); gerr == nil {
				return 0, persistence.ErrConflict
			}
		}
	}
	return v, tr("bump config version", err)
}

// RevertConfigVersion undoes a bump to `to` when nobody bumped since.
func (r *ConfigVersionRepository) RevertConfigVersion(ctx context.Context, projectID uuid.UUID, to int) error {
	_, err := r.exec.Exec(ctx, `UPDATE projects SET config_version = $2 WHERE id = $1 AND config_version = $2 + 1`, projectID, to)
	return tr("revert config version", err)
}

// ---------------------------------------------------------------------------
// Validation / checkpoint / fix reads without command output (P17-58,
// P17-59).

const webRunColumns = `v.id, v.execution_id, v.scope, v.work_unit_id, v.phase_id, v.label, v.attempt, v.status, v.cwd, v.acceptance,
	(SELECT count(*) FROM validation_command_results c WHERE c.run_id = v.id),
	(SELECT count(*) FROM validation_command_results c WHERE c.run_id = v.id AND c.outcome <> 'PASS'),
	v.started_at, v.finished_at`

func scanWebRun(row interface{ Scan(...any) error }) (*persistence.ValidationRunRow, error) {
	var x persistence.ValidationRunRow
	var acc []byte
	if err := row.Scan(&x.ID, &x.ExecutionID, &x.Scope, &x.WorkUnitID, &x.PhaseID, &x.Label, &x.Attempt, &x.Status, &x.CWD, &acc,
		&x.Commands, &x.Failed, &x.StartedAt, &x.FinishedAt); err != nil {
		return nil, err
	}
	if len(acc) > 0 && string(acc) != "null" {
		var a execution.AcceptanceResult
		if json.Unmarshal(acc, &a) == nil {
			x.Acceptance = &a
		}
	}
	return &x, nil
}

// ValidationRuns lists runs newest first.
func (r *WebReadRepository) ValidationRuns(ctx context.Context, projectID uuid.UUID, f persistence.ValidationFilter) ([]persistence.ValidationRunRow, error) {
	args := []any{projectID}
	q := `SELECT ` + webRunColumns + ` FROM validation_runs v WHERE v.project_id = $1`
	if f.ExecutionID != nil {
		args = append(args, *f.ExecutionID)
		q += " AND v.execution_id = $" + itoa(len(args))
	}
	if f.WorkUnitID != nil {
		args = append(args, *f.WorkUnitID)
		q += " AND v.work_unit_id = $" + itoa(len(args))
	}
	if f.PhaseID != nil {
		args = append(args, *f.PhaseID)
		q += " AND v.phase_id = $" + itoa(len(args))
	}
	if f.Scope != "" {
		args = append(args, f.Scope)
		q += " AND v.scope = $" + itoa(len(args))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += " ORDER BY v.started_at DESC, v.id LIMIT $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, tr("list validation runs", err)
	}
	defer rows.Close()
	out := []persistence.ValidationRunRow{}
	for rows.Next() {
		x, err := scanWebRun(rows)
		if err != nil {
			return nil, tr("scan validation run", err)
		}
		out = append(out, *x)
	}
	return out, tr("list validation runs", rows.Err())
}

// ValidationRun returns one run with bounded command output heads.
func (r *WebReadRepository) ValidationRun(ctx context.Context, projectID, runID uuid.UUID, headLen int) (*persistence.ValidationRunRow, []persistence.CommandResultRow, error) {
	run, err := scanWebRun(r.exec.QueryRow(ctx, `SELECT `+webRunColumns+` FROM validation_runs v WHERE v.project_id = $1 AND v.id = $2`, projectID, runID))
	if err != nil {
		return nil, nil, tr("get validation run", err)
	}
	if headLen <= 0 || headLen > 64<<10 {
		headLen = 4096
	}
	rows, err := r.exec.Query(ctx, `SELECT id, ord, name, program, args, cwd, exit_code, duration_ms, timeout_ms, timed_out, outcome, infra_reason,
		substr(stdout, 1, $2), substr(stderr, 1, $2), char_length(stdout), char_length(stderr)
		FROM validation_command_results WHERE run_id = $1 ORDER BY ord`, runID, headLen)
	if err != nil {
		return nil, nil, tr("list command results", err)
	}
	defer rows.Close()
	out := []persistence.CommandResultRow{}
	for rows.Next() {
		var c persistence.CommandResultRow
		var args []byte
		if err := rows.Scan(&c.ID, &c.Ord, &c.Name, &c.Program, &args, &c.CWD, &c.ExitCode, &c.DurationMS, &c.TimeoutMS, &c.TimedOut,
			&c.Outcome, &c.InfraReason, &c.StdoutHead, &c.StderrHead, &c.StdoutLen, &c.StderrLen); err != nil {
			return nil, nil, tr("scan command result", err)
		}
		_ = json.Unmarshal(args, &c.Args)
		out = append(out, c)
	}
	return run, out, tr("list command results", rows.Err())
}

const webCheckpointColumns = `c.id, c.project_id, c.execution_id, c.kind, c.target_id, c.checkpoint_key, c.status, COALESCE(c.commit_sha, ''),
	COALESCE(c.parent_sha, ''), COALESCE(c.tree_sha, ''), c.validation_run_id, c.error, c.created_at, c.completed_at, COALESCE(v.status, '')`

func scanWebCheckpoint(row interface{ Scan(...any) error }) (*persistence.CheckpointRow, error) {
	var x persistence.CheckpointRow
	var kind string
	if err := row.Scan(&x.ID, &x.ProjectID, &x.ExecutionID, &kind, &x.TargetID, &x.Key, &x.Status, &x.Commit, &x.Parent, &x.Tree,
		&x.ValidationRunID, &x.Error, &x.CreatedAt, &x.CompletedAt, &x.ValidationStatus); err != nil {
		return nil, err
	}
	x.Kind = execution.RepoCheckpointKind(kind)
	return &x, nil
}

// Checkpoints lists checkpoints newest first.
func (r *WebReadRepository) Checkpoints(ctx context.Context, projectID uuid.UUID, f persistence.CheckpointFilter) ([]persistence.CheckpointRow, error) {
	args := []any{projectID}
	q := `SELECT ` + webCheckpointColumns + ` FROM repository_checkpoints c LEFT JOIN validation_runs v ON v.id = c.validation_run_id
		WHERE c.project_id = $1`
	if f.ExecutionID != nil {
		args = append(args, *f.ExecutionID)
		q += " AND c.execution_id = $" + itoa(len(args))
	}
	if f.TargetID != nil {
		args = append(args, *f.TargetID)
		q += " AND c.target_id = $" + itoa(len(args))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += " ORDER BY c.created_at DESC, c.id LIMIT $" + itoa(len(args))
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, tr("list checkpoints", err)
	}
	defer rows.Close()
	out := []persistence.CheckpointRow{}
	for rows.Next() {
		x, err := scanWebCheckpoint(rows)
		if err != nil {
			return nil, tr("scan checkpoint", err)
		}
		out = append(out, *x)
	}
	return out, tr("list checkpoints", rows.Err())
}

// Checkpoint returns one project-scoped checkpoint.
func (r *WebReadRepository) Checkpoint(ctx context.Context, projectID, id uuid.UUID) (*persistence.CheckpointRow, error) {
	x, err := scanWebCheckpoint(r.exec.QueryRow(ctx, `SELECT `+webCheckpointColumns+` FROM repository_checkpoints c
		LEFT JOIN validation_runs v ON v.id = c.validation_run_id WHERE c.project_id = $1 AND c.id = $2`, projectID, id))
	if err != nil {
		return nil, tr("get checkpoint", err)
	}
	return x, nil
}

// FixAttempts lists fix attempts (oldest first) with dispatch status.
func (r *WebReadRepository) FixAttempts(ctx context.Context, projectID uuid.UUID, executionID, targetID *uuid.UUID) ([]persistence.FixAttemptRow, error) {
	args := []any{projectID}
	q := `SELECT f.id, f.execution_id, f.target_kind, f.target_id, f.failed_run_id, f.attempt, f.dispatch_id, f.evidence, f.created_at,
		COALESCE(d.status, ''), COALESCE(d.delivery, ''), COALESCE(d.role_key, '')
		FROM fix_attempts f JOIN executions e ON e.id = f.execution_id LEFT JOIN worker_dispatches d ON d.id = f.dispatch_id
		WHERE e.project_id = $1`
	if executionID != nil {
		args = append(args, *executionID)
		q += " AND f.execution_id = $" + itoa(len(args))
	}
	if targetID != nil {
		args = append(args, *targetID)
		q += " AND f.target_id = $" + itoa(len(args))
	}
	q += " ORDER BY f.created_at, f.attempt LIMIT 500"
	rows, err := r.exec.Query(ctx, q, args...)
	if err != nil {
		return nil, tr("list fix attempts", err)
	}
	defer rows.Close()
	out := []persistence.FixAttemptRow{}
	for rows.Next() {
		var x persistence.FixAttemptRow
		var ev []byte
		if err := rows.Scan(&x.ID, &x.ExecutionID, &x.TargetKind, &x.TargetID, &x.FailedRunID, &x.Attempt, &x.DispatchID, &ev, &x.CreatedAt,
			&x.DispatchStatus, &x.DispatchDelivery, &x.DispatchRole); err != nil {
			return nil, tr("scan fix attempt", err)
		}
		_ = json.Unmarshal(ev, &x.Evidence)
		out = append(out, x)
	}
	return out, tr("list fix attempts", rows.Err())
}

// PhaseRows lists phases with plan + progress in one query.
func (r *WebReadRepository) PhaseRows(ctx context.Context, projectID uuid.UUID) ([]persistence.PhaseRow, error) {
	rows, err := r.exec.Query(ctx, `SELECT ph.id, ph.phase_order, ph.name, ph.description, ph.status,
		COALESCE(d.current_version, mv.v, 0), COALESCE(wv.content_hash, ''),
		pl.id, COALESCE(pl.version, 0), COALESCE(pl.wbs_version, 0), COALESCE(pl.status, ''), COALESCE(pl.kind, ''),
		COALESCE(uc.done, 0), COALESCE(uc.total, 0), ph.started_at, ph.completed_at, ph.updated_at
		FROM phases ph
		LEFT JOIN phase_documents d ON d.phase_id = ph.id AND d.doc_type = 'WBS'
		LEFT JOIN LATERAL (SELECT max(version) AS v FROM wbs_versions WHERE phase_id = ph.id) mv ON true
		LEFT JOIN wbs_versions wv ON wv.phase_id = ph.id AND wv.version = COALESCE(d.current_version, mv.v)
		LEFT JOIN LATERAL (SELECT p.id, p.version, p.wbs_version, p.status, p.kind FROM execution_plans p
			WHERE p.phase_id = ph.id AND p.status <> 'SUPERSEDED' ORDER BY p.version DESC LIMIT 1) pl ON true
		LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE w.status = 'DONE') AS done, count(*) AS total
			FROM work_units w WHERE w.execution_plan_id = pl.id) uc ON true
		WHERE ph.project_id = $1 ORDER BY ph.phase_order`, projectID)
	if err != nil {
		return nil, tr("phase rows", err)
	}
	defer rows.Close()
	out := []persistence.PhaseRow{}
	for rows.Next() {
		var x persistence.PhaseRow
		if err := rows.Scan(&x.ID, &x.Order, &x.Name, &x.Description, &x.Status, &x.WBSVersion, &x.WBSHash, &x.PlanID, &x.PlanVersion,
			&x.PlanWBS, &x.PlanStatus, &x.PlanKind, &x.Done, &x.Total, &x.StartedAt, &x.CompletedAt, &x.UpdatedAt); err != nil {
			return nil, tr("scan phase row", err)
		}
		out = append(out, x)
	}
	return out, tr("phase rows", rows.Err())
}

// UnitDetail returns one unit's definition and table facts.
func (r *WebReadRepository) UnitDetail(ctx context.Context, projectID, unitID uuid.UUID) (*persistence.UnitDetailRow, error) {
	var x persistence.UnitDetailRow
	var acc, tests, cons, paths []byte
	err := r.exec.QueryRow(ctx, `SELECT w.id, w.sequence, COALESCE(NULLIF(w.unit_key, ''), '#' || w.sequence::text), w.kind, w.name, w.status,
		ARRAY(SELECT COALESCE(NULLIF(d2.unit_key, ''), '#' || d2.sequence::text) FROM work_unit_dependencies d
			JOIN work_units d2 ON d2.id = d.depends_on_work_unit_id WHERE d.work_unit_id = w.id ORDER BY d2.sequence),
		ARRAY(SELECT COALESCE(NULLIF(d2.unit_key, ''), '#' || d2.sequence::text) FROM work_unit_dependencies d
			JOIN work_units d2 ON d2.id = d.depends_on_work_unit_id WHERE d.work_unit_id = w.id AND d2.status <> 'DONE' ORDER BY d2.sequence),
		(SELECT count(*) FROM worker_dispatches wd WHERE wd.work_unit_id = w.id),
		COALESCE((SELECT vr.status FROM validation_runs vr WHERE vr.work_unit_id = w.id ORDER BY vr.started_at DESC LIMIT 1), ''),
		COALESCE((SELECT rc.status FROM repository_checkpoints rc WHERE rc.kind = 'UNIT' AND rc.target_id = w.id ORDER BY rc.created_at DESC LIMIT 1), ''),
		w.started_at, w.completed_at, w.project_id, w.execution_plan_id, p.phase_id, p.version, w.objective, w.implementation_instructions,
		w.description, w.acceptance_criteria, w.acceptance_checks, w.required_tests, w.constraints, w.validation_commands, w.paths,
		w.claimed_execution_id, w.done_validation_run_id
		FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id JOIN phases ph ON ph.id = p.phase_id
		WHERE w.id = $2 AND ph.project_id = $1`, projectID, unitID).Scan(&x.ID, &x.Sequence, &x.Key, &x.Kind, &x.Name, &x.Status,
		&x.Deps, &x.Blocking, &x.Attempts, &x.WorkUnitRow.Validation, &x.Checkpoint, &x.StartedAt, &x.CompletedAt, &x.ProjectID,
		&x.PlanID, &x.PhaseID, &x.PlanVersion, &x.Objective, &x.Instructions, &x.Description, &acc, &x.AcceptanceChecks, &tests, &cons,
		&x.ValidationCommands, &paths, &x.ClaimedExecution, &x.DoneRunID)
	if err != nil {
		return nil, tr("unit detail", err)
	}
	_ = json.Unmarshal(acc, &x.AcceptanceText)
	_ = json.Unmarshal(tests, &x.RequiredTests)
	_ = json.Unmarshal(cons, &x.Constraints)
	_ = json.Unmarshal(paths, &x.Paths)
	return &x, nil
}

// ProjectMetrics aggregates execution / context / planning metrics from
// persisted telemetry in a fixed number of queries (P17-63 / P17-64).
// Current plans only: superseded plans are history, not progress.
func (r *WebReadRepository) ProjectMetrics(ctx context.Context, projectID uuid.UUID) (*persistence.ProjectMetricsRow, error) {
	x := &persistence.ProjectMetricsRow{UnitsByKind: map[string]int{}}
	err := r.exec.QueryRow(ctx, `WITH cur AS (
			SELECT DISTINCT ON (p.phase_id) p.id FROM execution_plans p JOIN phases ph ON ph.id = p.phase_id
			WHERE ph.project_id = $1 AND p.status <> 'SUPERSEDED' ORDER BY p.phase_id, p.version DESC),
		u AS (SELECT w.* FROM work_units w WHERE w.execution_plan_id IN (SELECT id FROM cur)),
		disp AS (SELECT work_unit_id, count(*) AS n FROM worker_dispatches WHERE project_id = $1 AND work_unit_id IS NOT NULL GROUP BY work_unit_id),
		cont AS (SELECT work_unit_id, count(*) AS n FROM continuations WHERE project_id = $1 GROUP BY work_unit_id)
		SELECT
			(SELECT count(*) FROM u WHERE status = 'DONE'),
			(SELECT count(*) FROM u),
			(SELECT avg(extract(epoch FROM completed_at - started_at)) FROM u WHERE status = 'DONE' AND started_at IS NOT NULL AND completed_at IS NOT NULL),
			(SELECT count(*) FROM u LEFT JOIN disp ON disp.work_unit_id = u.id LEFT JOIN cont ON cont.work_unit_id = u.id
				WHERE u.status = 'DONE' AND COALESCE(disp.n, 0) = 1 AND COALESCE(cont.n, 0) = 0),
			(SELECT count(*) FROM u JOIN disp ON disp.work_unit_id = u.id),
			(SELECT count(*) FROM continuations WHERE project_id = $1),
			(SELECT count(*) FROM continuations WHERE project_id = $1 AND strategy = 'fresh_session'),
			(SELECT count(*) FROM validation_runs WHERE project_id = $1 AND scope = 'UNIT' AND status <> 'RUNNING'),
			(SELECT count(*) FROM validation_runs WHERE project_id = $1 AND scope = 'UNIT' AND status = 'FAIL'),
			(SELECT count(*) FROM fix_attempts f JOIN executions e ON e.id = f.execution_id WHERE e.project_id = $1),
			(SELECT avg(o.context_tokens::float8 / o.context_window) FROM u
				JOIN LATERAL (SELECT d.id FROM worker_dispatches d WHERE d.work_unit_id = u.id ORDER BY d.created_at DESC LIMIT 1) ld ON true
				JOIN worker_usage_observations o ON o.dispatch_id = ld.id
				WHERE u.status = 'DONE' AND o.context_tokens IS NOT NULL AND o.context_window > 0),
			(SELECT avg(c)::float8 FROM (SELECT count(d.depends_on_work_unit_id) AS c FROM u LEFT JOIN work_unit_dependencies d ON d.work_unit_id = u.id GROUP BY u.id) z),
			(SELECT count(*) FROM execution_plans p JOIN phases ph ON ph.id = p.phase_id WHERE ph.project_id = $1 AND p.kind = 'AMENDMENT')`,
		projectID).Scan(&x.UnitsDone, &x.UnitsTotal, &x.AvgUnitSeconds, &x.SingleInvocationDone, &x.UnitsDispatched, &x.Continuations, &x.Resets,
		&x.ValidationRuns, &x.ValidationFails, &x.FixAttempts, &x.AvgContextAtCompletion, &x.AvgDependenciesPerUnit, &x.PlanAmendments)
	if err != nil {
		return nil, tr("project metrics", err)
	}
	rows, err := r.exec.Query(ctx, `SELECT w.kind, count(*) FROM work_units w WHERE w.execution_plan_id IN (
			SELECT DISTINCT ON (p.phase_id) p.id FROM execution_plans p JOIN phases ph ON ph.id = p.phase_id
			WHERE ph.project_id = $1 AND p.status <> 'SUPERSEDED' ORDER BY p.phase_id, p.version DESC) GROUP BY w.kind`, projectID)
	if err != nil {
		return nil, tr("project kind metrics", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, tr("scan kind metric", err)
		}
		x.UnitsByKind[k] = n
	}
	return x, tr("project kind metrics", rows.Err())
}

// PhaseMetrics aggregates per-phase metrics in one query (P17-64).
func (r *WebReadRepository) PhaseMetrics(ctx context.Context, projectID uuid.UUID) ([]persistence.PhaseMetricsRow, error) {
	rows, err := r.exec.Query(ctx, `SELECT ph.id, ph.phase_order, ph.name, ph.status,
		CASE WHEN ph.started_at IS NOT NULL THEN extract(epoch FROM COALESCE(ph.completed_at, now()) - ph.started_at) END,
		COALESCE((SELECT count(*) FROM work_units w WHERE w.execution_plan_id = (SELECT p.id FROM execution_plans p
			WHERE p.phase_id = ph.id AND p.status <> 'SUPERSEDED' ORDER BY p.version DESC LIMIT 1)), 0),
		(SELECT count(*) FROM plan_candidates c WHERE c.phase_id = ph.id),
		(SELECT count(*) FROM continuations c WHERE c.phase_id = ph.id),
		(SELECT count(*) FROM validation_runs v WHERE v.phase_id = ph.id),
		(SELECT count(*) FROM fix_attempts f JOIN work_units w ON w.id = f.target_id JOIN execution_plans p ON p.id = w.execution_plan_id
			WHERE p.phase_id = ph.id)
		FROM phases ph WHERE ph.project_id = $1 ORDER BY ph.phase_order`, projectID)
	if err != nil {
		return nil, tr("phase metrics", err)
	}
	defer rows.Close()
	out := []persistence.PhaseMetricsRow{}
	for rows.Next() {
		var x persistence.PhaseMetricsRow
		if err := rows.Scan(&x.PhaseID, &x.Order, &x.Name, &x.Status, &x.DurationSeconds, &x.Units, &x.PlanAttempts, &x.Continuations,
			&x.ValidationAttempts, &x.Fixes); err != nil {
			return nil, tr("scan phase metric", err)
		}
		out = append(out, x)
	}
	return out, tr("phase metrics", rows.Err())
}
