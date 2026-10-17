package dbrepo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RecoveryScanRepository reads every unfinished durable operation in one
// REPEATABLE READ READ ONLY snapshot (P18-26, WBS 18.6).
type RecoveryScanRepository struct{ pool *pgxpool.Pool }

// NewRecoveryScanRepository builds the repository.
func NewRecoveryScanRepository(pool *pgxpool.Pool) *RecoveryScanRepository {
	return &RecoveryScanRepository{pool: pool}
}

// Scan returns the unfinished operations.
func (r *RecoveryScanRepository) Scan(ctx context.Context) (*persistence.RecoveryScanData, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, tr("recovery scan", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	out := &persistence.RecoveryScanData{}
	rows, err := tx.Query(ctx, `SELECT id, project_id, status, COALESCE(driver_owner, ''), driver_expires_at, COALESCE(recovery_reason, ''), recovery_evidence
		FROM executions WHERE status IN ('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'RECOVERY_REQUIRED') ORDER BY id`)
	if err != nil {
		return nil, tr("recovery scan executions", err)
	}
	for rows.Next() {
		var e persistence.ScanExecution
		var ev []byte
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Status, &e.DriverOwner, &e.DriverExpiresAt, &e.RecoveryReason, &ev); err != nil {
			rows.Close()
			return nil, tr("recovery scan executions", err)
		}
		if len(ev) > 0 {
			_ = json.Unmarshal(ev, &e.Evidence)
		}
		out.Executions = append(out.Executions, e)
	}
	rows.Close()
	// Dispatches: in flight, or ambiguous and referenced by an unfinished
	// step / a recovery-required execution of the project.
	rows, err = tx.Query(ctx, `SELECT d.id, d.project_id, s.execution_id, d.status, COALESCE(d.delivery, ''), d.state_key, d.created_at
		FROM worker_dispatches d
		LEFT JOIN LATERAL (SELECT st.execution_id FROM execution_steps st WHERE st.dispatch_id = d.id ORDER BY st.seq DESC LIMIT 1) s ON true
		WHERE d.status IN ('PENDING', 'SENDING', 'WAITING')
		   OR (d.delivery = 'DELIVERY_UNKNOWN' AND EXISTS (SELECT 1 FROM executions e WHERE e.project_id = d.project_id
		       AND e.status = 'RECOVERY_REQUIRED'))
		ORDER BY d.created_at, d.id`)
	if err != nil {
		return nil, tr("recovery scan dispatches", err)
	}
	for rows.Next() {
		var d persistence.ScanDispatch
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.ExecutionID, &d.Status, &d.Delivery, &d.StateKey, &d.CreatedAt); err != nil {
			rows.Close()
			return nil, tr("recovery scan dispatches", err)
		}
		out.Dispatches = append(out.Dispatches, d)
	}
	rows.Close()
	type q struct {
		kind, sql string
	}
	for _, x := range []q{
		{"RUNNING_VALIDATION", `SELECT id, project_id, execution_id, jsonb_build_object('scope', scope, 'label', label, 'started_at', started_at)
			FROM validation_runs WHERE status = 'RUNNING' ORDER BY id`},
		{"PENDING_CHECKPOINT", `SELECT id, project_id, execution_id, jsonb_build_object('kind', kind, 'target_id', target_id)
			FROM repository_checkpoints WHERE status = 'PENDING' ORDER BY id`},
		{"INTENT_STEP", `SELECT id, project_id, execution_id, jsonb_build_object('action', action, 'state', state_key, 'seq', seq,
			'dispatch_id', dispatch_id, 'checkpoint_id', checkpoint_id, 'validation_run_id', validation_run_id, 'intent_at', intent_at)
			FROM execution_steps WHERE status = 'INTENT' ORDER BY execution_id, seq`},
		{"ACTIVE_CONTINUATION", `SELECT c.id, c.project_id, NULL::uuid, jsonb_build_object('status', c.status, 'work_unit_id', c.work_unit_id)
			FROM continuations c WHERE c.status NOT IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED') ORDER BY c.id`},
		{"PRECOMPUTE_JOB", `SELECT id, project_id, NULL::uuid, jsonb_build_object('status', status, 'owner', owner)
			FROM precompute_jobs WHERE status IN ('PENDING', 'RUNNING') ORDER BY id`},
	} {
		rows, err := tx.Query(ctx, x.sql)
		if err != nil {
			return nil, tr("recovery scan "+x.kind, err)
		}
		for rows.Next() {
			it := persistence.ScanItem{Kind: x.kind}
			var detail []byte
			var exec *uuid.UUID
			if err := rows.Scan(&it.ID, &it.ProjectID, &exec, &detail); err != nil {
				rows.Close()
				return nil, tr("recovery scan "+x.kind, err)
			}
			it.ExecutionID = exec
			_ = json.Unmarshal(detail, &it.Detail)
			out.Items = append(out.Items, it)
		}
		rows.Close()
	}
	return out, tx.Commit(ctx)
}

var _ = time.Now
