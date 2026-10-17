package dbrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertFacts are the durable inputs of the alert evaluator (P18-61).
type AlertFacts struct {
	RecoveryRequired []string
	Stalled          []string
	Deadlocked       []string
	// ValidationInfraFailures counts INFRA_ERROR validation runs finished
	// within the window.
	ValidationInfraFailures int
}

// AlertFactsRepository reads alert facts in one read-only snapshot.
type AlertFactsRepository struct{ pool *pgxpool.Pool }

// NewAlertFactsRepository binds the repository.
func NewAlertFactsRepository(pool *pgxpool.Pool) *AlertFactsRepository {
	return &AlertFactsRepository{pool: pool}
}

// stalledSQL: runnable (RUNNING — WAITING is not runnable, WBS 18.119)
// executions whose latest step activity, and the execution itself, are
// older than the threshold.
const stalledSQL = `SELECT e.id::text FROM executions e
	WHERE e.status = 'RUNNING' AND e.updated_at < now() - make_interval(secs => $1)
	AND COALESCE((SELECT GREATEST(max(s.intent_at), max(s.finished_at)) FROM execution_steps s WHERE s.execution_id = e.id), e.updated_at)
		< now() - make_interval(secs => $1)
	ORDER BY 1`

// deadlockedSQL: runnable executions on a unit plan with work left, none
// of it eligible (every unfinished unit waits on a unit that is not DONE),
// nothing in progress and nothing in flight (WBS 18.120).
const deadlockedSQL = `SELECT e.id::text FROM executions e
	JOIN execution_plans ep ON ep.phase_id = e.current_phase_id AND ep.status IN ('READY', 'EXECUTING')
	WHERE e.status = 'RUNNING' AND e.current_work_unit_id IS NULL
	AND EXISTS (SELECT 1 FROM work_units w WHERE w.execution_plan_id = ep.id AND w.status <> 'DONE')
	AND NOT EXISTS (SELECT 1 FROM work_units w WHERE w.execution_plan_id = ep.id AND w.status IN ('PENDING', 'READY', 'INCOMPLETE')
		AND NOT EXISTS (SELECT 1 FROM work_unit_dependencies d JOIN work_units dep ON dep.id = d.depends_on_work_unit_id
			WHERE d.work_unit_id = w.id AND dep.status <> 'DONE'))
	AND NOT EXISTS (SELECT 1 FROM work_units w WHERE w.execution_plan_id = ep.id AND w.status IN ('EXECUTING', 'VALIDATING'))
	AND NOT EXISTS (SELECT 1 FROM worker_dispatches d WHERE d.project_id = e.project_id AND d.status IN ('PENDING', 'SENDING', 'WAITING'))
	AND NOT EXISTS (SELECT 1 FROM validation_runs v WHERE v.execution_id = e.id AND v.status = 'RUNNING')
	ORDER BY 1`

// Facts reads the alert facts.
func (r *AlertFactsRepository) Facts(ctx context.Context, stall, infraWindow time.Duration) (*AlertFacts, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, TranslateDBError("alert facts", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	ids := func(q string, args ...any) ([]string, error) {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return nil, TranslateDBError("alert facts", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, TranslateDBError("alert facts", err)
			}
			out = append(out, id)
		}
		return out, tr("alert facts", rows.Err())
	}
	f := &AlertFacts{}
	if f.RecoveryRequired, err = ids(`SELECT id::text FROM executions WHERE status = 'RECOVERY_REQUIRED' ORDER BY 1`); err != nil {
		return nil, err
	}
	if f.Stalled, err = ids(stalledSQL, stall.Seconds()); err != nil {
		return nil, err
	}
	if f.Deadlocked, err = ids(deadlockedSQL); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM validation_runs WHERE status = 'INFRA_ERROR' AND finished_at > now() - make_interval(secs => $1)`,
		infraWindow.Seconds()).Scan(&f.ValidationInfraFailures); err != nil {
		return nil, TranslateDBError("alert facts", err)
	}
	return f, nil
}
