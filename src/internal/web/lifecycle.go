package web

import (
	"errors"
	"net/http"

	"overseer/internal/project"
	"overseer/internal/service"
)

type lifecycleAPI struct {
	svc *service.LifecycleService
}

// RegisterLifecycleRoutes exposes the lifecycle command endpoints (WBS
// 3.20): POST /api/v1/projects/{id}/start|pause|resume|cancel.
func RegisterLifecycleRoutes(r *Router, svc *service.LifecycleService) error {
	api := &lifecycleAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		op      string
		h       func(w http.ResponseWriter, r *http.Request)
	}{
		{http.MethodPost, "/api/v1/projects/{id}/start", "start", api.handleStart},
		{http.MethodPost, "/api/v1/projects/{id}/pause", "pause", api.handlePause},
		{http.MethodPost, "/api/v1/projects/{id}/resume", "resume", api.handleResume},
		{http.MethodPost, "/api/v1/projects/{id}/cancel", "cancel", api.handleCancel},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, http.HandlerFunc(rt.h)); err != nil {
			return err
		}
	}
	return nil
}

func (a *lifecycleAPI) handleStart(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Start(r.Context(), r.PathValue("id"))
	a.writeLifecycle(w, r, p, err)
}

func (a *lifecycleAPI) handlePause(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Pause(r.Context(), r.PathValue("id"))
	a.writeLifecycle(w, r, p, err)
}

func (a *lifecycleAPI) handleResume(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Resume(r.Context(), r.PathValue("id"))
	a.writeLifecycle(w, r, p, err)
}

func (a *lifecycleAPI) handleCancel(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Cancel(r.Context(), r.PathValue("id"))
	a.writeLifecycle(w, r, p, err)
}

// writeLifecycle maps the lifecycle operation result (WBS 3.20): an
// invalid transition is 409 (references/lifecycle-ops.md), an unknown
// project is 404, and a successful transition returns the project.
func (a *lifecycleAPI) writeLifecycle(w http.ResponseWriter, r *http.Request, p *project.Project, err error) {
	if err != nil {
		WriteError(w, r, lifecycleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResource(p))
}

// lifecycleServiceError maps the lifecycle service error set to HTTP codes
// for the command surface: an invalid transition (ErrInvalidInput) is 409
// per T3.20.5, distinct from plain input validation (400, P3-23).
func lifecycleServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewConflictError("invalid lifecycle transition")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrTaskOpRunning):
		return NewConflictCode("task_op_running", "A task analysis or fix is running; resume when it finishes.")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("project state conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("lifecycle operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}
