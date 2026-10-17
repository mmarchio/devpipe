package dbrepo

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// StatusEventRepository persists lifecycle transition events on
// PostgreSQL (WBS 3.7).
type StatusEventRepository struct {
	exec executor
}

// NewStatusEventRepository binds the repository to an existing pool.
func NewStatusEventRepository(pool *pgxpool.Pool) *StatusEventRepository {
	return &StatusEventRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21, 3.7).
func (r *StatusEventRepository) WithTx(tx persistence.Transaction) persistence.StatusEventRepository {
	return &StatusEventRepository{exec: txExecutor{tx}}
}

// Record inserts one event (T3.7.1–T3.7.4).
func (r *StatusEventRepository) Record(ctx context.Context, e *persistence.StatusEvent) error {
	var id, projectID uuid.UUID
	var createdAt time.Time
	row := r.exec.QueryRow(ctx,
		`INSERT INTO project_status_events (id, project_id, from_status, to_status, reason, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, project_id, created_at`,
		e.ID, e.ProjectID, e.FromStatus, e.ToStatus, e.Reason, e.CreatedAt.UTC())
	if err := row.Scan(&id, &projectID, &createdAt); err != nil {
		return TranslateDBError("record status event", err)
	}
	e.ID, e.ProjectID, e.CreatedAt = id, projectID, createdAt
	return nil
}

// ListByProject returns the project's events oldest first.
func (r *StatusEventRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.StatusEvent, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT id, project_id, from_status, to_status, reason, created_at
		 FROM project_status_events WHERE project_id = $1
		 ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("list status events", err)
	}
	defer rows.Close()
	var out []*persistence.StatusEvent
	for rows.Next() {
		var e persistence.StatusEvent
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.FromStatus, &e.ToStatus, &e.Reason, &e.CreatedAt); err != nil {
			return nil, TranslateDBError("list status events", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list status events", err)
	}
	return out, nil
}
