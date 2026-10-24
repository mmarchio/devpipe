package dbrepo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// MCPRepository persists the Phase 19 records: dispatch tokens, MCP
// requests, task reports, unit archive membership and the workflow
// outbox.
type MCPRepository struct {
	exec executor
}

var _ persistence.MCPRepository = (*MCPRepository)(nil)

// NewMCPRepository binds the repository to a pool.
func NewMCPRepository(pool *pgxpool.Pool) *MCPRepository {
	return &MCPRepository{exec: poolAdapter{pool}}
}

func (r *MCPRepository) ex(e persistence.Executor) executor {
	if e == nil {
		return r.exec
	}
	return e
}

// --- tokens -----------------------------------------------------------------

const tokenColumns = `token_hash, worker_prompt_id, project_id, execution_id, work_unit_id, role_key, state_key, created_at, expires_at, revoked_at`

// CreateToken stores a command prompt's token.
func (r *MCPRepository) CreateToken(ctx context.Context, ex persistence.Executor, rec *persistence.MCPTokenRecord) error {
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO mcp_dispatch_tokens (`+tokenColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL)`,
		rec.TokenHash, rec.WorkerPromptID, rec.ProjectID, rec.ExecutionID, rec.WorkUnitID, rec.RoleKey, rec.StateKey,
		rec.CreatedAt.UTC(), rec.ExpiresAt.UTC()); err != nil {
		return TranslateDBError("create mcp token", err)
	}
	return nil
}

// PromptHasToken reports whether the command prompt carries a token.
func (r *MCPRepository) PromptHasToken(ctx context.Context, ex persistence.Executor, promptID uuid.UUID) (bool, error) {
	var ok bool
	if err := r.ex(ex).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_dispatch_tokens WHERE worker_prompt_id = $1)`, promptID).Scan(&ok); err != nil {
		return false, TranslateDBError("check mcp token", err)
	}
	return ok, nil
}

// GetToken returns the token by hash.
func (r *MCPRepository) GetToken(ctx context.Context, ex persistence.Executor, hash string) (*persistence.MCPTokenRecord, error) {
	var t persistence.MCPTokenRecord
	err := r.ex(ex).QueryRow(ctx, `SELECT `+tokenColumns+` FROM mcp_dispatch_tokens WHERE token_hash = $1`, hash).
		Scan(&t.TokenHash, &t.WorkerPromptID, &t.ProjectID, &t.ExecutionID, &t.WorkUnitID, &t.RoleKey, &t.StateKey, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt)
	if err != nil {
		return nil, TranslateDBError("get mcp token", err)
	}
	return &t, nil
}

// RevokeExecutionTokens revokes the execution's tokens.
func (r *MCPRepository) RevokeExecutionTokens(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, at time.Time) error {
	if _, err := r.ex(ex).Exec(ctx, `UPDATE mcp_dispatch_tokens SET revoked_at = $2 WHERE execution_id = $1 AND revoked_at IS NULL`,
		executionID, at.UTC()); err != nil {
		return TranslateDBError("revoke mcp tokens", err)
	}
	return nil
}

// InFlightDispatch returns the prompt's dispatch being sent or awaiting its
// reply.
func (r *MCPRepository) InFlightDispatch(ctx context.Context, ex persistence.Executor, promptID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.ex(ex).QueryRow(ctx, `SELECT id FROM worker_dispatches WHERE worker_prompt_id = $1 AND status IN ('SENDING', 'WAITING')
		ORDER BY created_at DESC, id LIMIT 1`, promptID).Scan(&id)
	if err != nil {
		return uuid.Nil, TranslateDBError("get in-flight dispatch", err)
	}
	return id, nil
}

// --- requests ---------------------------------------------------------------

// GetRequest returns a request by idempotency key.
func (r *MCPRepository) GetRequest(ctx context.Context, ex persistence.Executor, dispatchID uuid.UUID, tool, key string) (*persistence.MCPRequestRecord, error) {
	var q persistence.MCPRequestRecord
	var result []byte
	err := r.ex(ex).QueryRow(ctx, `SELECT id, dispatch_id, tool, idempotency_key, payload_hash, result, created_at FROM mcp_requests
		WHERE dispatch_id = $1 AND tool = $2 AND idempotency_key = $3`, dispatchID, tool, key).
		Scan(&q.ID, &q.DispatchID, &q.Tool, &q.IdempotencyKey, &q.PayloadHash, &result, &q.CreatedAt)
	if err != nil {
		return nil, TranslateDBError("get mcp request", err)
	}
	q.Result = result
	return &q, nil
}

// CreateRequest stores a request.
func (r *MCPRepository) CreateRequest(ctx context.Context, ex persistence.Executor, rec *persistence.MCPRequestRecord) error {
	result := rec.Result
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO mcp_requests (id, dispatch_id, tool, idempotency_key, payload_hash, result, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, rec.ID, rec.DispatchID, rec.Tool, rec.IdempotencyKey, rec.PayloadHash, []byte(result),
		rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create mcp request", err)
	}
	return nil
}

// --- task reports -----------------------------------------------------------

const reportColumns = `id, project_id, execution_id, dispatch_id, work_unit_id, role_key, state_key, kind, status, criteria_met, summary, evidence, created_at`

// CreateTaskReport stores a report.
func (r *MCPRepository) CreateTaskReport(ctx context.Context, ex persistence.Executor, rec *persistence.TaskReportRecord) error {
	met := rec.CriteriaMet
	if met == nil {
		met = []bool{}
	}
	ev := rec.Evidence
	if ev == nil {
		ev = map[string]any{}
	}
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO task_reports (`+reportColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rec.ID, rec.ProjectID, rec.ExecutionID, rec.DispatchID, rec.WorkUnitID, rec.RoleKey, rec.StateKey, rec.Kind, rec.Status,
		jsonOf(met), rec.Summary, jsonOf(ev), rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create task report", err)
	}
	return nil
}

func scanReport(row interface{ Scan(dest ...any) error }) (*persistence.TaskReportRecord, error) {
	var t persistence.TaskReportRecord
	var met, ev []byte
	if err := row.Scan(&t.ID, &t.ProjectID, &t.ExecutionID, &t.DispatchID, &t.WorkUnitID, &t.RoleKey, &t.StateKey, &t.Kind, &t.Status, &met,
		&t.Summary, &ev, &t.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(met, &t.CriteriaMet); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(ev, &t.Evidence); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTaskReport returns the dispatch's report of the kind.
func (r *MCPRepository) GetTaskReport(ctx context.Context, ex persistence.Executor, dispatchID uuid.UUID, kind string) (*persistence.TaskReportRecord, error) {
	t, err := scanReport(r.ex(ex).QueryRow(ctx, `SELECT `+reportColumns+` FROM task_reports WHERE dispatch_id = $1 AND kind = $2`, dispatchID, kind))
	if err != nil {
		return nil, TranslateDBError("get task report", err)
	}
	return t, nil
}

// ListTaskReports returns the execution's (or the unit's) reports, oldest
// first.
func (r *MCPRepository) ListTaskReports(ctx context.Context, ex persistence.Executor, executionID uuid.UUID, unitID *uuid.UUID) ([]*persistence.TaskReportRecord, error) {
	rows, err := r.ex(ex).Query(ctx, `SELECT `+reportColumns+` FROM task_reports WHERE execution_id = $1 AND ($2::uuid IS NULL OR work_unit_id = $2)
		ORDER BY created_at, id`, executionID, unitID)
	if err != nil {
		return nil, TranslateDBError("list task reports", err)
	}
	defer rows.Close()
	out := []*persistence.TaskReportRecord{}
	for rows.Next() {
		t, err := scanReport(rows)
		if err != nil {
			return nil, TranslateDBError("list task reports", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list task reports", err)
	}
	return out, nil
}

// --- archive ----------------------------------------------------------------

// ArchiveUnit records the unit's archive membership once.
func (r *MCPRepository) ArchiveUnit(ctx context.Context, ex persistence.Executor, unitID uuid.UUID, dispatchID *uuid.UUID, at time.Time) error {
	tag, err := r.ex(ex).Exec(ctx, `UPDATE work_units SET archived_at = $3, archived_by_dispatch_id = $2 WHERE id = $1 AND archived_at IS NULL`,
		unitID, dispatchID, at.UTC())
	if err != nil {
		return TranslateDBError("archive work unit", err)
	}
	if tag.RowsAffected() == 0 {
		var n int
		if err := r.ex(ex).QueryRow(ctx, `SELECT count(*) FROM work_units WHERE id = $1`, unitID).Scan(&n); err != nil {
			return TranslateDBError("archive work unit", err)
		}
		if n == 0 {
			return persistence.ErrNotFound
		}
	}
	return nil
}

// ArchivedUnits returns the archived units of a plan.
func (r *MCPRepository) ArchivedUnits(ctx context.Context, ex persistence.Executor, planID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := r.ex(ex).Query(ctx, `SELECT id FROM work_units WHERE execution_plan_id = $1 AND archived_at IS NOT NULL`, planID)
	if err != nil {
		return nil, TranslateDBError("list archived units", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, TranslateDBError("list archived units", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list archived units", err)
	}
	return out, nil
}

// --- outbox -----------------------------------------------------------------

const outboxColumns = `id, project_id, execution_id, kind, payload, dedupe_key, status, attempts, next_attempt_at, claimed_by, claimed_until, last_error, created_at, finished_at`

func scanOutbox(row interface{ Scan(dest ...any) error }) (*persistence.OutboxRecord, error) {
	var o persistence.OutboxRecord
	var payload []byte
	var claimedBy *string
	if err := row.Scan(&o.ID, &o.ProjectID, &o.ExecutionID, &o.Kind, &payload, &o.DedupeKey, &o.Status, &o.Attempts, &o.NextAttemptAt,
		&claimedBy, &o.ClaimedUntil, &o.LastError, &o.CreatedAt, &o.FinishedAt); err != nil {
		return nil, err
	}
	if claimedBy != nil {
		o.ClaimedBy = *claimedBy
	}
	if err := json.Unmarshal(payload, &o.Payload); err != nil {
		return nil, err
	}
	return &o, nil
}

// EnqueueOutbox stores an action unless its dedupe key is queued.
func (r *MCPRepository) EnqueueOutbox(ctx context.Context, ex persistence.Executor, rec *persistence.OutboxRecord) error {
	p := rec.Payload
	if p == nil {
		p = map[string]any{}
	}
	if _, err := r.ex(ex).Exec(ctx, `INSERT INTO workflow_outbox (id, project_id, execution_id, kind, payload, dedupe_key, status, attempts, next_attempt_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', 0, $7, $7) ON CONFLICT (dedupe_key) DO NOTHING`,
		rec.ID, rec.ProjectID, rec.ExecutionID, rec.Kind, jsonOf(p), rec.DedupeKey, rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("enqueue outbox", err)
	}
	return nil
}

// ClaimOutbox leases up to limit due PENDING actions to owner. A row
// whose lease expired (its claimer crashed) is due again.
func (r *MCPRepository) ClaimOutbox(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]*persistence.OutboxRecord, error) {
	rows, err := r.exec.Query(ctx, `UPDATE workflow_outbox SET claimed_by = $1, claimed_until = $3
		WHERE id IN (SELECT id FROM workflow_outbox
			WHERE status = 'PENDING' AND next_attempt_at <= $2 AND (claimed_until IS NULL OR claimed_until <= $2)
			ORDER BY next_attempt_at, id LIMIT $4 FOR UPDATE SKIP LOCKED)
		RETURNING `+outboxColumns, owner, now.UTC(), now.Add(lease).UTC(), limit)
	if err != nil {
		return nil, TranslateDBError("claim outbox", err)
	}
	defer rows.Close()
	var out []*persistence.OutboxRecord
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, TranslateDBError("claim outbox", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("claim outbox", err)
	}
	return out, nil
}

// FinishOutbox marks a claimed action DONE; a lost claim is ErrConflict.
func (r *MCPRepository) FinishOutbox(ctx context.Context, id uuid.UUID, owner string, at time.Time) error {
	tag, err := r.exec.Exec(ctx, `UPDATE workflow_outbox SET status = 'DONE', attempts = attempts + 1, finished_at = $3, claimed_until = NULL
		WHERE id = $1 AND status = 'PENDING' AND claimed_by = $2`, id, owner, at.UTC())
	if err != nil {
		return TranslateDBError("finish outbox", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// FailOutbox records a failed attempt; a lost claim is ErrConflict.
func (r *MCPRepository) FailOutbox(ctx context.Context, id uuid.UUID, owner, msg string, next time.Time, dead bool, at time.Time) error {
	status := persistence.OutboxPending
	var finished *time.Time
	if dead {
		status = persistence.OutboxDead
		t := at.UTC()
		finished = &t
	}
	tag, err := r.exec.Exec(ctx, `UPDATE workflow_outbox SET status = $4, attempts = attempts + 1, last_error = $3, next_attempt_at = $5,
		finished_at = $6, claimed_until = NULL WHERE id = $1 AND status = 'PENDING' AND claimed_by = $2`,
		id, owner, msg, status, next.UTC(), finished)
	if err != nil {
		return TranslateDBError("fail outbox", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

// ListOutbox returns the project's actions, newest first.
func (r *MCPRepository) ListOutbox(ctx context.Context, projectID uuid.UUID, limit int) ([]*persistence.OutboxRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+outboxColumns+` FROM workflow_outbox WHERE project_id = $1 ORDER BY created_at DESC, id LIMIT $2`,
		projectID, limit)
	if err != nil {
		return nil, TranslateDBError("list outbox", err)
	}
	defer rows.Close()
	out := []*persistence.OutboxRecord{}
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, TranslateDBError("list outbox", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list outbox", err)
	}
	return out, nil
}
