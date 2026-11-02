package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// TaskScopeRepository stores the task scope a project's orchestrator
// works on: MVP (MVP tasks only) or FULL (MVP and FULL tasks).
type TaskScopeRepository struct {
	exec executor
}

// NewTaskScopeRepository binds the repository to an existing pool.
func NewTaskScopeRepository(pool *pgxpool.Pool) *TaskScopeRepository {
	return &TaskScopeRepository{exec: poolAdapter{pool}}
}

// TaskScope returns the project's selected scope; a project that never
// chose one works on every task (FULL).
func (r *TaskScopeRepository) TaskScope(ctx context.Context, projectID uuid.UUID) (wbs.TaskScope, error) {
	var s string
	err := r.exec.QueryRow(ctx, `SELECT scope FROM project_task_scope WHERE project_id = $1`, projectID).Scan(&s)
	if err != nil {
		if err = TranslateDBError("get task scope", err); errors.Is(err, persistence.ErrNotFound) {
			return wbs.TaskScopeFULL, nil
		}
		return "", err
	}
	return wbs.ParseTaskScope(s)
}

// SetTaskScope records the project's selected scope.
func (r *TaskScopeRepository) SetTaskScope(ctx context.Context, projectID uuid.UUID, scope wbs.TaskScope) error {
	_, err := r.exec.Exec(ctx, `INSERT INTO project_task_scope (project_id, scope) VALUES ($1, $2)
		ON CONFLICT (project_id) DO UPDATE SET scope = EXCLUDED.scope, updated_at = now()`, projectID, scope.String())
	if err != nil {
		return TranslateDBError("set task scope", err)
	}
	return nil
}
