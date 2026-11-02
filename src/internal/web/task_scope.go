package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// TaskScopeStore reads and records the task scope a project's
// orchestrator works on.
type TaskScopeStore interface {
	TaskScope(ctx context.Context, projectID uuid.UUID) (wbs.TaskScope, error)
	SetTaskScope(ctx context.Context, projectID uuid.UUID, scope wbs.TaskScope) error
}

type taskScopeAPI struct {
	store TaskScopeStore
}

// RegisterTaskScopeRoutes exposes the project's task scope:
// GET/PUT /api/v1/projects/{id}/task-scope with {"scope": "MVP" | "FULL"}
// (MVP: MVP tasks only; FULL: MVP and FULL tasks).
func RegisterTaskScopeRoutes(r *Router, store TaskScopeStore) error {
	api := &taskScopeAPI{store: store}
	if err := r.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}/task-scope", api.handleGet); err != nil {
		return err
	}
	return r.RegisterAPI(http.MethodPut, "/api/v1/projects/{id}/task-scope", api.handlePut)
}

var allowedTaskScopeFields = map[string]bool{"scope": true}

type taskScopeRequest struct {
	Scope string `json:"scope"`
}

func (a *taskScopeAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.store.TaskScope(r.Context(), pid)
	if err != nil {
		WriteError(w, r, NewInternalError("task scope could not be read"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": s})
}

func (a *taskScopeAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req taskScopeRequest
	if err := decodeStrictJSON(r, "invalid_task_scope", allowedTaskScopeFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := wbs.ParseTaskScope(req.Scope)
	if err != nil {
		WriteError(w, r, NewValidationCode("invalid_task_scope", "scope must be MVP or FULL"))
		return
	}
	if err := a.store.SetTaskScope(r.Context(), pid, s); err != nil {
		if errors.Is(err, persistence.ErrInvalidReference) {
			WriteError(w, r, NewNotFoundError("project"))
			return
		}
		WriteError(w, r, NewInternalError("task scope could not be saved"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": s})
}
