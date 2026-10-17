package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/phase"
)

const phaseColumns = `id, project_id, phase_order, name, description, status, created_at, updated_at, started_at, completed_at, failure_reason, failed_at`

// PhaseRepository implements ordered-phase persistence on PostgreSQL.
type PhaseRepository struct {
	exec executor
}

// NewPhaseRepository binds the repository to an existing pool.
func NewPhaseRepository(pool *pgxpool.Pool) *PhaseRepository {
	return &PhaseRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *PhaseRepository) WithTx(tx persistence.Transaction) persistence.PhaseRepository {
	return &PhaseRepository{exec: txExecutor{tx}}
}

func scanPhaseRow(row interface{ Scan(dest ...any) error }) (*phase.Phase, error) {
	var (
		p      phase.Phase
		status string
	)
	if err := row.Scan(&p.ID, &p.ProjectID, &p.Order, &p.Name, &p.Description, &status, &p.CreatedAt, &p.UpdatedAt, &p.StartedAt, &p.CompletedAt, &p.FailureReason, &p.FailedAt); err != nil {
		return nil, err
	}
	s, err := phase.ParseStatus(status)
	if err != nil {
		return nil, err
	}
	p.Status = s
	return &p, nil
}

// Append creates a new phase with a deterministic order: the next value
// after the project's high-water mark (projects.max_phase_order — the
// largest order ever issued, 0 when none), so appends always land at the
// end and deleted orders are never reused, even after the highest-order
// phase is deleted (WBS 2.7, T2.7.1–T2.7.5; Phase 4 DoD invariant). The
// computation runs in a transaction serialized by a project-scoped
// advisory lock, and a bounded retry covers any residual
// unique-violation race, so concurrent appends always receive distinct
// orders (T2.7.6).
func (r *PhaseRepository) Append(ctx context.Context, projectID uuid.UUID, name, description string) (*phase.Phase, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		p, err := r.appendOnce(ctx, projectID, name, description)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, persistence.ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *PhaseRepository) appendOnce(ctx context.Context, projectID uuid.UUID, name, description string) (*phase.Phase, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin phase append", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx) // no-op after commit
		}
	}()

	// Serialize appends per project without blocking other projects.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, projectID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxOrder int
	if err := tx.QueryRow(ctx, `SELECT max_phase_order FROM projects WHERE id = $1`, projectID).Scan(&maxOrder); err != nil {
		return nil, TranslateDBError("phase order mark", err)
	}
	p, err := phase.New(projectID, maxOrder+1, name, description)
	if err != nil {
		return nil, err
	}
	var status string
	if err := tx.QueryRow(ctx,
		`INSERT INTO phases (id, project_id, phase_order, name, description, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+phaseColumns,
		p.ID, p.ProjectID, p.Order, p.Name, p.Description, p.Status.String(), p.CreatedAt.UTC()).
		Scan(&p.ID, &p.ProjectID, &p.Order, &p.Name, &p.Description, &status, &p.CreatedAt, &p.UpdatedAt, &p.StartedAt, &p.CompletedAt, &p.FailureReason, &p.FailedAt); err != nil {
		return nil, TranslateDBError("insert phase", err)
	}
	s, err := phase.ParseStatus(status)
	if err != nil {
		return nil, err
	}
	p.Status = s
	// Advance the persistent high-water mark so a later deletion of this
	// phase can never cause its order to be reused (Phase 4 DoD).
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET max_phase_order = $2 WHERE id = $1`,
		projectID, p.Order); err != nil {
		return nil, TranslateDBError("update phase order mark", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit phase append", err)
		}
	}
	return p, nil
}

// beginTx returns the serializing transaction for the append (WBS 2.7).
// When the repository is already bound to a transaction (WithTx), the
// append joins that transaction so the whole outer unit stays atomic
// (WBS 2.21) and the caller owns the commit (joined=true). Otherwise it
// starts its own transaction.
func (r *PhaseRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
	if te, ok := r.exec.(txExecutor); ok {
		return te.tx, true, nil
	}
	if b, ok := r.exec.(persistence.TxBeginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return nil, false, err
		}
		return tx, false, nil
	}
	return nil, false, &persistence.InfrastructureError{Op: "phase append", Cause: errors.New("executor cannot begin transactions")}
}

// Get returns the phase with the given id (T2.8.1); unknown ids yield
// persistence.ErrNotFound.
func (r *PhaseRepository) Get(ctx context.Context, id uuid.UUID) (*phase.Phase, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+phaseColumns+` FROM phases WHERE id = $1`, id)
	p, err := scanPhaseRow(row)
	if err != nil {
		return nil, TranslateDBError("get phase", err)
	}
	return p, nil
}

// Update persists name/description/status changes (WBS 2.8). Order and
// ProjectID are immutable by design.
func (r *PhaseRepository) Update(ctx context.Context, p *phase.Phase) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE phases SET name = $2, description = $3, status = $4, updated_at = now() WHERE id = $1`,
		p.ID, p.Name, p.Description, p.Status.String())
	if err != nil {
		return TranslateDBError("update phase", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// Delete removes the phase (WBS 2.8). The project's high-water mark is
// untouched — deletion never lowers it, which is what keeps historical
// phase numbers from being reused (Phase 4 DoD invariant).
func (r *PhaseRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM phases WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete phase", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByProject returns the project's phases in ascending phase_order
// (T2.8.2).
func (r *PhaseRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*phase.Phase, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+phaseColumns+` FROM phases WHERE project_id = $1 ORDER BY phase_order ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list phases", err)
	}
	defer rows.Close()
	var out []*phase.Phase
	for rows.Next() {
		p, err := scanPhaseRow(rows)
		if err != nil {
			return nil, TranslateDBError("list phases", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list phases", err)
	}
	return out, nil
}

// GetNextIncomplete returns the lowest-order phase that is not DONE
// (T2.8.3), using phase order rather than creation time, or (nil, nil)
// when every phase is complete (T2.8.4).
func (r *PhaseRepository) GetNextIncomplete(ctx context.Context, projectID uuid.UUID) (*phase.Phase, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+phaseColumns+` FROM phases
		 WHERE project_id = $1 AND status <> 'DONE'
		 ORDER BY phase_order ASC LIMIT 1`, projectID)
	p, err := scanPhaseRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, TranslateDBError("next incomplete phase", err)
	}
	return p, nil
}

// MarkStarted records the start transition (T2.8.5): status EXECUTING and
// StartedAt set once.
func (r *PhaseRepository) MarkStarted(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE phases SET status = 'EXECUTING', started_at = COALESCE(started_at, now()), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("mark phase started", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// MarkCompleted records the completion transition (T2.8.6): status DONE
// and CompletedAt.
func (r *PhaseRepository) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE phases SET status = 'DONE', completed_at = COALESCE(completed_at, now()), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("mark phase completed", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// MarkFailed records the failure transition (WBS 4.23): status FAILED, the
// failure reason, and FailedAt set once (COALESCE, matching
// MarkStarted/MarkCompleted).
func (r *PhaseRepository) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE phases SET status = 'FAILED', failure_reason = $2, failed_at = COALESCE(failed_at, now()), updated_at = now() WHERE id = $1`,
		id, reason)
	if err != nil {
		return TranslateDBError("mark phase failed", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
