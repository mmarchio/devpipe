package dbrepo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// TransitionHistoryRepository implements persistence.TransitionHistoryRepository
// on PostgreSQL (WBS 6.28/6.29). The history is append-only and the sequence
// number is persisted per project (projects.state_entry_seq), allocated under
// a project-scoped advisory lock.
type TransitionHistoryRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.TransitionHistoryRepository = (*TransitionHistoryRepository)(nil)

// NewTransitionHistoryRepository binds the repository to an existing pool.
func NewTransitionHistoryRepository(pool *pgxpool.Pool) *TransitionHistoryRepository {
	return &TransitionHistoryRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *TransitionHistoryRepository) WithTx(tx persistence.Transaction) persistence.TransitionHistoryRepository {
	return &TransitionHistoryRepository{exec: txExecutor{tx}}
}

const transitionHistoryColumns = `id, project_id, state_machine_id, state_machine_version,
from_state_id, to_state_id, outcome, phase_id, work_unit_id, runtime_version, seq, created_at`

// transitionHistorySelect reads a BOOTSTRAP entry's NULL source state as
// uuid.Nil (P15-27) and the execution binding.
const transitionHistorySelect = `id, project_id, state_machine_id, state_machine_version,
COALESCE(from_state_id, '00000000-0000-0000-0000-000000000000'::uuid), to_state_id, outcome, phase_id, work_unit_id,
runtime_version, seq, created_at, execution_id`

func scanTransitionHistoryRow(row interface{ Scan(dest ...any) error }) (*persistence.TransitionHistoryRecord, error) {
	var rec persistence.TransitionHistoryRecord
	if err := row.Scan(
		&rec.ID, &rec.ProjectID, &rec.StateMachineID, &rec.StateMachineVersion,
		&rec.FromStateID, &rec.ToStateID, &rec.Outcome, &rec.PhaseID,
		&rec.WorkUnitID, &rec.RuntimeVersion, &rec.Seq, &rec.CreatedAt, &rec.ExecutionID); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Append records one transition (WBS 6.28, T6.28.1–T6.28.6). When
// allocateSeq is true the per-project sequence number is computed from the
// project's persisted counter under a project-scoped advisory lock with a
// bounded retry on the unique (project_id, seq) constraint (WBS 6.29,
// T6.29.1–T6.29.4): concurrent callers never duplicate a number and the
// counter is persisted, so the sequence survives restart.
func (r *TransitionHistoryRepository) Append(ctx context.Context, rec *persistence.TransitionHistoryRecord, allocateSeq bool) error {
	if rec.Seq == 0 && !allocateSeq {
		return &persistence.InfrastructureError{Op: "append transition history", Cause: errors.New("seq must be set when allocation is disabled")}
	}
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if allocateSeq {
		return r.appendWithSequence(ctx, rec, now)
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_transition_history (`+transitionHistoryColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rec.ID, rec.ProjectID, rec.StateMachineID, rec.StateMachineVersion,
		rec.FromStateID, rec.ToStateID, rec.Outcome, rec.PhaseID,
		rec.WorkUnitID, rec.RuntimeVersion, rec.Seq, rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("append transition history", err)
	}
	return nil
}

// appendWithSequence allocates the next sequence under the project-scoped
// advisory lock (the P6-17 / P3-25 per-scope deterministic-allocation rule)
// and inserts the row; a unique-constraint collision is retried (bounded)
// with a recomputed number.
func (r *TransitionHistoryRepository) appendWithSequence(ctx context.Context, rec *persistence.TransitionHistoryRecord, now time.Time) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if te, ok := r.exec.(txExecutor); ok {
			// Bound to a transaction: the caller's transaction IS the
			// serialization scope (same pattern as WBSVersionRepository).
			if err := r.allocateAndInsertOn(ctx, te.tx, rec, now); err != nil {
				return err
			}
			return nil
		}
		b, ok := r.exec.(persistence.TxBeginner)
		if !ok {
			return &persistence.InfrastructureError{Op: "append transition history", Cause: errors.New("executor cannot begin transactions")}
		}
		tx, err := b.Begin(ctx)
		if err != nil {
			return TranslateDBError("begin transition history sequence", err)
		}
		aerr := r.allocateAndInsertOn(ctx, tx, rec, now)
		if aerr == nil {
			if cerr := tx.Commit(ctx); cerr != nil {
				_ = tx.Rollback(ctx)
				return TranslateDBError("commit transition history sequence", cerr)
			}
			return nil
		}
		_ = tx.Rollback(ctx)
		if errors.Is(aerr, persistence.ErrConflict) {
			continue // recompute under a fresh advisory lock
		}
		return aerr
	}
	return persistence.ErrConflict
}

// allocateAndInsertOn runs inside one transaction: take the project-scoped
// advisory lock, read the persisted counter, insert the row at counter+1,
// and advance the counter.
func (r *TransitionHistoryRepository) allocateAndInsertOn(ctx context.Context, tx persistence.Transaction, rec *persistence.TransitionHistoryRecord, now time.Time) error {
	exec := txExecutor{tx}
	var lockKey int64
	if err := exec.QueryRow(ctx, `SELECT hashtext($1::text)`, rec.ProjectID).Scan(&lockKey); err != nil {
		return TranslateDBError("hash transition history scope", err)
	}
	if _, err := exec.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return TranslateDBError("transition history sequence lock", err)
	}
	var next int
	if err := exec.QueryRow(ctx,
		`SELECT state_entry_seq + 1 FROM projects WHERE id = $1`, rec.ProjectID).Scan(&next); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return persistence.ErrNotFound
		}
		return TranslateDBError("read transition history sequence", err)
	}
	if _, err := exec.Exec(ctx,
		`INSERT INTO state_transition_history (`+transitionHistoryColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rec.ID, rec.ProjectID, rec.StateMachineID, rec.StateMachineVersion,
		rec.FromStateID, rec.ToStateID, rec.Outcome, rec.PhaseID,
		rec.WorkUnitID, rec.RuntimeVersion, next, rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("append transition history", err)
	}
	if _, err := exec.Exec(ctx,
		`UPDATE projects SET state_entry_seq = $2 WHERE id = $1`, rec.ProjectID, next); err != nil {
		return TranslateDBError("advance transition history sequence", err)
	}
	rec.Seq = next
	return nil
}

// ListByProject lists a project's history in seq order (deterministic,
// T6.28.6 reads).
func (r *TransitionHistoryRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+transitionHistorySelect+` FROM state_transition_history
		 WHERE project_id = $1 ORDER BY seq ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list transition history", err)
	}
	defer rows.Close()
	var out []*persistence.TransitionHistoryRecord
	for rows.Next() {
		rec, err := scanTransitionHistoryRow(rows)
		if err != nil {
			return nil, TranslateDBError("scan transition history", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("iterate transition history", err)
	}
	return out, nil
}
