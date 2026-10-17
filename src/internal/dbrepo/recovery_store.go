package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RecoveryStore implements persistence.RecoveryStore (P18-27 / P18-28 /
// P18-32).
type RecoveryStore struct{ pool *pgxpool.Pool }

// NewRecoveryStore builds the store.
func NewRecoveryStore(pool *pgxpool.Pool) *RecoveryStore { return &RecoveryStore{pool: pool} }

func (s *RecoveryStore) ex(e persistence.Executor) executor {
	if e != nil {
		return e
	}
	return poolAdapter{s.pool}
}

// RecordPlan stores a plan once per (execution, hash).
func (s *RecoveryStore) RecordPlan(ctx context.Context, p *persistence.RecoveryPlanRecord) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO recovery_plans (id, execution_id, project_id, plan_hash, condition, classification, plan, source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (execution_id, plan_hash) DO NOTHING`,
		p.ID, p.ExecutionID, p.ProjectID, p.PlanHash, p.Condition, p.Classification, p.Plan, p.Source, p.CreatedAt.UTC())
	return tr("record recovery plan", err)
}

// ListPlans returns an execution's recorded plans (oldest first).
func (s *RecoveryStore) ListPlans(ctx context.Context, executionID uuid.UUID) ([]persistence.RecoveryPlanRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, execution_id, project_id, plan_hash, condition, classification, plan, source, created_at
		FROM recovery_plans WHERE execution_id = $1 ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, tr("list recovery plans", err)
	}
	defer rows.Close()
	var out []persistence.RecoveryPlanRecord
	for rows.Next() {
		var p persistence.RecoveryPlanRecord
		if err := rows.Scan(&p.ID, &p.ExecutionID, &p.ProjectID, &p.PlanHash, &p.Condition, &p.Classification, &p.Plan, &p.Source, &p.CreatedAt); err != nil {
			return nil, tr("list recovery plans", err)
		}
		out = append(out, p)
	}
	return out, tr("list recovery plans", rows.Err())
}

const actionCols = `id, execution_id, project_id, idempotency_key, action, plan_hash, evidence_hash, actor_id, actor_name, reason, confirmed,
	correlation_id, status, result, error, created_at, finished_at`

func scanAction(row interface{ Scan(...any) error }) (*persistence.RecoveryActionRecord, error) {
	var a persistence.RecoveryActionRecord
	var res []byte
	if err := row.Scan(&a.ID, &a.ExecutionID, &a.ProjectID, &a.IdempotencyKey, &a.Action, &a.PlanHash, &a.EvidenceHash, &a.ActorID,
		&a.ActorName, &a.Reason, &a.Confirmed, &a.CorrelationID, &a.Status, &res, &a.Error, &a.CreatedAt, &a.FinishedAt); err != nil {
		return nil, tr("read recovery action", err)
	}
	_ = json.Unmarshal(res, &a.Result)
	return &a, nil
}

// BeginAction inserts a RUNNING action; a reused key returns the existing
// record with ErrConflict.
func (s *RecoveryStore) BeginAction(ctx context.Context, a *persistence.RecoveryActionRecord) (*persistence.RecoveryActionRecord, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO recovery_actions (id, execution_id, project_id, idempotency_key, action, plan_hash, evidence_hash,
		actor_id, actor_name, reason, confirmed, correlation_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'RUNNING', $13) ON CONFLICT (idempotency_key) DO NOTHING`,
		a.ID, a.ExecutionID, a.ProjectID, a.IdempotencyKey, a.Action, a.PlanHash, a.EvidenceHash, a.ActorID, a.ActorName, a.Reason,
		a.Confirmed, a.CorrelationID, a.CreatedAt.UTC())
	if err != nil {
		return nil, tr("begin recovery action", err)
	}
	if tag.RowsAffected() == 0 {
		prev, err := s.GetActionByKey(ctx, a.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		return prev, persistence.ErrConflict
	}
	a.Status = "RUNNING"
	return a, nil
}

// FinishAction records the outcome.
func (s *RecoveryStore) FinishAction(ctx context.Context, id uuid.UUID, status string, result map[string]any, errText string) error {
	if result == nil {
		result = map[string]any{}
	}
	b, _ := json.Marshal(result)
	_, err := s.pool.Exec(context.WithoutCancel(ctx), `UPDATE recovery_actions SET status = $2, result = $3, error = $4, finished_at = $5 WHERE id = $1 AND status = 'RUNNING'`,
		id, status, b, errText, time.Now().UTC())
	return tr("finish recovery action", err)
}

// GetActionByKey returns the action with key.
func (s *RecoveryStore) GetActionByKey(ctx context.Context, key string) (*persistence.RecoveryActionRecord, error) {
	return scanAction(s.pool.QueryRow(ctx, `SELECT `+actionCols+` FROM recovery_actions WHERE idempotency_key = $1`, key))
}

// ListActions returns an execution's actions (oldest first).
func (s *RecoveryStore) ListActions(ctx context.Context, executionID uuid.UUID) ([]persistence.RecoveryActionRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+actionCols+` FROM recovery_actions WHERE execution_id = $1 ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, tr("list recovery actions", err)
	}
	defer rows.Close()
	var out []persistence.RecoveryActionRecord
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, tr("list recovery actions", rows.Err())
}

// InsertResolution stores an operator resolution (one per dispatch).
func (s *RecoveryStore) InsertResolution(ctx context.Context, r *persistence.DispatchResolutionRecord) error {
	ev, _ := json.Marshal(r.Evidence)
	_, err := s.pool.Exec(ctx, `INSERT INTO dispatch_resolutions (id, dispatch_id, execution_id, project_id, resolution, action_id, actor_id, reason, evidence, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, r.ID, r.DispatchID, r.ExecutionID, r.ProjectID, r.Resolution, r.ActionID, r.ActorID,
		r.Reason, ev, r.CreatedAt.UTC())
	return tr("insert dispatch resolution", err)
}

// GetResolution returns the resolution of a dispatch.
func (s *RecoveryStore) GetResolution(ctx context.Context, dispatchID uuid.UUID) (*persistence.DispatchResolutionRecord, error) {
	var r persistence.DispatchResolutionRecord
	var ev []byte
	err := s.pool.QueryRow(ctx, `SELECT id, dispatch_id, execution_id, project_id, resolution, action_id, actor_id, reason, evidence, consumed_at, created_at
		FROM dispatch_resolutions WHERE dispatch_id = $1`, dispatchID).Scan(&r.ID, &r.DispatchID, &r.ExecutionID, &r.ProjectID, &r.Resolution,
		&r.ActionID, &r.ActorID, &r.Reason, &ev, &r.ConsumedAt, &r.CreatedAt)
	if err != nil {
		return nil, tr("get dispatch resolution", err)
	}
	_ = json.Unmarshal(ev, &r.Evidence)
	return &r, nil
}

// ConsumeResolution marks a resolution applied (exactly once).
func (s *RecoveryStore) ConsumeResolution(ctx context.Context, ex persistence.Executor, dispatchID uuid.UUID) (bool, error) {
	tag, err := s.ex(ex).Exec(ctx, `UPDATE dispatch_resolutions SET consumed_at = now() WHERE dispatch_id = $1 AND consumed_at IS NULL`, dispatchID)
	if err != nil {
		return false, tr("consume dispatch resolution", err)
	}
	return tag.RowsAffected() == 1, nil
}

var _ = errors.New

// ReleaseResolution un-consumes a resolution (its resend was provably not
// sent).
func (s *RecoveryStore) ReleaseResolution(ctx context.Context, dispatchID uuid.UUID) error {
	_, err := s.pool.Exec(context.WithoutCancel(ctx), `UPDATE dispatch_resolutions SET consumed_at = NULL WHERE dispatch_id = $1`, dispatchID)
	return tr("release dispatch resolution", err)
}
