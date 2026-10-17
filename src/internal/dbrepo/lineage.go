package dbrepo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// Lineage of one work unit (P18-72, WBS 18.205): every artifact that led
// to its result, by id and hash — never large content.

// LineageRepository reads unit lineage.
type LineageRepository struct{ pool *pgxpool.Pool }

// NewLineageRepository binds the repository.
func NewLineageRepository(pool *pgxpool.Pool) *LineageRepository {
	return &LineageRepository{pool: pool}
}

// UnitLineage reads the lineage in one read-only snapshot
// (persistence.ErrNotFound when the unit is not the project's).
func (r *LineageRepository) UnitLineage(ctx context.Context, projectID, unitID uuid.UUID) (*persistence.UnitLineage, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, TranslateDBError("lineage", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	l := &persistence.UnitLineage{ProjectID: projectID.String(), WorkUnitID: unitID.String(), Dispatches: []persistence.LineageDispatch{},
		Validations: []persistence.LineageValidation{}, Checkpoints: []persistence.LineageCheckpoint{}, Events: []persistence.LineageEvent{}, CorrelationIDs: []string{}}
	err = tx.QueryRow(ctx, `SELECT w.unit_key, w.name, w.status, ep.phase_id::text, ep.id::text, ep.version, ep.status, ep.wbs_version,
		COALESCE((SELECT content_hash FROM wbs_versions v WHERE v.phase_id = ep.phase_id AND v.version = ep.wbs_version), ''),
		encode(sha256(convert_to(w.objective, 'UTF8')), 'hex')
		FROM work_units w JOIN execution_plans ep ON ep.id = w.execution_plan_id WHERE w.id = $1 AND w.project_id = $2`, unitID, projectID).
		Scan(&l.UnitKey, &l.Name, &l.Status, &l.PhaseID, &l.PlanID, &l.PlanVersion, &l.PlanStatus, &l.WBSVersion, &l.WBSHash, &l.ObjectiveHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, TranslateDBError("lineage unit", err)
	}
	rows, err := tx.Query(ctx, `SELECT d.id::text, d.request_id::text, d.attempt, d.status, d.role_key, d.state_key, d.correlation_id, d.created_at,
		wp.id::text, wp.hash, wp.source, COALESCE(cp.id::text, ''), COALESCE(cp.hash, ''), COALESCE(cp.template_id::text, ''),
		COALESCE(cp.template_version, 0), COALESCE(cp.role_id::text, ''), COALESCE(cp.role_version, 0),
		COALESCE(cp.state_key, d.state_key), COALESCE(cp.projection_id::text, ''), COALESCE(cp.context_snapshot_id::text, ''),
		COALESCE((SELECT json_agg(json_build_object('type_key', f.type_key, 'scope', f.scope, 'version', f.version, 'content_hash', f.content_hash)
			ORDER BY f.type_key, f.scope) FROM context_snapshot_files f WHERE f.snapshot_id = cp.context_snapshot_id), '[]'),
		res.id::text, res.status, res.raw_response_hash, res.parsed_hash
		FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id LEFT JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id
		LEFT JOIN worker_results res ON res.dispatch_id = d.id
		WHERE d.project_id = $1 AND d.work_unit_id = $2 ORDER BY d.created_at, d.id`, projectID, unitID)
	if err != nil {
		return nil, TranslateDBError("lineage dispatches", err)
	}
	for rows.Next() {
		var d persistence.LineageDispatch
		var files []byte
		var resID, resStatus, resRaw, resParsed *string
		if err := rows.Scan(&d.ID, &d.RequestID, &d.Attempt, &d.Status, &d.RoleKey, &d.StateKey, &d.CorrelationID, &d.CreatedAt,
			&d.Prompt.WorkerPromptID, &d.Prompt.WorkerPromptHash, &d.Prompt.WorkerPromptSource, &d.Prompt.CanonicalPromptID,
			&d.Prompt.CanonicalPromptHash, &d.Prompt.TemplateID, &d.Prompt.TemplateVersion, &d.Prompt.RoleID, &d.Prompt.RoleVersion,
			&d.Prompt.StateKey, &d.Prompt.ProjectionID, &d.Prompt.ContextSnapshotID, &files, &resID, &resStatus, &resRaw, &resParsed); err != nil {
			rows.Close()
			return nil, TranslateDBError("lineage dispatch", err)
		}
		d.Prompt.StateFiles = []persistence.LineageStateFile{}
		_ = json.Unmarshal(files, &d.Prompt.StateFiles)
		if resID != nil {
			d.Result = &persistence.LineageResult{ID: *resID, Status: deref(resStatus), RawResponseHash: deref(resRaw), ParsedHash: deref(resParsed)}
		}
		d.CreatedAt = d.CreatedAt.UTC()
		l.Dispatches = append(l.Dispatches, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("lineage dispatches", err)
	}
	rows, err = tx.Query(ctx, `SELECT v.id::text, v.status, v.attempt, v.correlation_id, v.started_at, v.finished_at,
		(SELECT count(*) FROM validation_command_results c WHERE c.run_id = v.id)
		FROM validation_runs v WHERE v.project_id = $1 AND v.work_unit_id = $2 ORDER BY v.started_at, v.id`, projectID, unitID)
	if err != nil {
		return nil, TranslateDBError("lineage validation", err)
	}
	for rows.Next() {
		var v persistence.LineageValidation
		if err := rows.Scan(&v.ID, &v.Status, &v.Attempt, &v.CorrelationID, &v.StartedAt, &v.FinishedAt, &v.Commands); err != nil {
			rows.Close()
			return nil, TranslateDBError("lineage validation", err)
		}
		v.StartedAt = v.StartedAt.UTC()
		l.Validations = append(l.Validations, v)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT id::text, status, COALESCE(commit_sha, '') FROM repository_checkpoints
		WHERE project_id = $1 AND kind = 'UNIT' AND target_id = $2 ORDER BY created_at, id`, projectID, unitID)
	if err != nil {
		return nil, TranslateDBError("lineage checkpoints", err)
	}
	for rows.Next() {
		var c persistence.LineageCheckpoint
		if err := rows.Scan(&c.ID, &c.Status, &c.CommitSHA); err != nil {
			rows.Close()
			return nil, TranslateDBError("lineage checkpoints", err)
		}
		l.Checkpoints = append(l.Checkpoints, c)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT ev.seq, ev.type, COALESCE(ev.payload->>'correlation_id', ''), ev.created_at FROM execution_events ev
		WHERE ev.project_id = $1 AND (ev.payload::text LIKE '%' || $2 || '%') ORDER BY ev.seq LIMIT 500`, projectID, unitID.String())
	if err != nil {
		return nil, TranslateDBError("lineage events", err)
	}
	for rows.Next() {
		var e persistence.LineageEvent
		if err := rows.Scan(&e.Seq, &e.Type, &e.CorrelationID, &e.At); err != nil {
			rows.Close()
			return nil, TranslateDBError("lineage events", err)
		}
		e.At = e.At.UTC()
		l.Events = append(l.Events, e)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT DISTINCT c FROM (
		SELECT detail->>'correlation_id' AS c FROM execution_steps WHERE project_id = $1 AND work_unit_id = $2
		UNION SELECT correlation_id FROM worker_dispatches WHERE project_id = $1 AND work_unit_id = $2
		UNION SELECT correlation_id FROM validation_runs WHERE project_id = $1 AND work_unit_id = $2) x
		WHERE COALESCE(c, '') <> '' ORDER BY 1`, projectID, unitID)
	if err != nil {
		return nil, TranslateDBError("lineage correlation", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, TranslateDBError("lineage correlation", err)
		}
		l.CorrelationIDs = append(l.CorrelationIDs, c)
	}
	rows.Close()
	l.Complete = lineageComplete(l)
	return l, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func lineageComplete(l *persistence.UnitLineage) bool {
	if l.Status != "DONE" || l.WBSHash == "" || len(l.Dispatches) == 0 {
		return false
	}
	resulted := false
	for _, d := range l.Dispatches {
		if d.Prompt.CanonicalPromptHash == "" || d.Prompt.WorkerPromptHash == "" || d.Prompt.ContextSnapshotID == "" {
			return false
		}
		resulted = resulted || d.Result != nil
	}
	passed := false
	for _, v := range l.Validations {
		passed = passed || v.Status == "PASS"
	}
	checkpointed := false
	for _, c := range l.Checkpoints {
		checkpointed = checkpointed || c.Status == "CREATED" || c.Status == "SKIPPED"
	}
	return resulted && passed && checkpointed
}
