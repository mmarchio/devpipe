// P6-30 (WBS 6.40): runtime-state REST. GET
// /api/v1/projects/{project}/runtime-state returns the read-only runtime
// state view (current state, phase order + name, work unit id + name,
// runtime version). Read-only: the route is registered GET-only — no
// surface accepts an arbitrary runtime state (T6.40.5).
package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
)

// runtimeStatePhase is the phase section of the runtime-state response
// (WBS 6.40): order + name.
type runtimeStatePhase struct {
	Order int    `json:"order"`
	Name  string `json:"name"`
}

// runtimeStateWorkUnit is the work-unit section of the runtime-state
// response (WBS 6.40): id + name.
type runtimeStateWorkUnit struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// runtimeStateResource is the stable JSON view of the runtime state
// (WBS 6.40) — the response shape from the WBS example: state /
// phase{order,name} / work_unit{id,name} / runtime_version. The phase and
// work_unit sections are null when the runtime row carries no association.
type runtimeStateResource struct {
	State          string                `json:"state"`
	Phase          *runtimeStatePhase    `json:"phase"`
	WorkUnit       *runtimeStateWorkUnit `json:"work_unit"`
	RuntimeVersion int                   `json:"runtime_version"`
}

func runtimeStateServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_runtime_state", "Runtime state request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("runtime state")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("runtime state conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("runtime state operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type runtimeStateAPI struct {
	svc *service.RuntimeStateAPIService
}

// RegisterRuntimeStateRoutes exposes the read-only runtime-state endpoint
// (WBS 6.40):
//
//	GET /api/v1/projects/{project}/runtime-state
//
// No write methods are registered: clients cannot set the runtime state
// arbitrarily (T6.40.5).
func RegisterRuntimeStateRoutes(r *Router, svc *service.RuntimeStateAPIService) error {
	api := &runtimeStateAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodGet, "/api/v1/projects/{project}/runtime-state", api.handleGet); err != nil {
		return err
	}
	return nil
}

// handleGet — T6.40.1–T6.40.4: the current state, its phase (order +
// name), the current work unit (id + name), and the runtime version.
func (a *runtimeStateAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Get(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, runtimeStateServiceError(err))
		return
	}
	out := runtimeStateResource{State: v.State, RuntimeVersion: v.RuntimeVersion}
	if v.Phase != nil {
		out.Phase = &runtimeStatePhase{Order: v.Phase.Order, Name: v.Phase.Name}
	}
	if v.WorkUnit != nil {
		out.WorkUnit = &runtimeStateWorkUnit{ID: v.WorkUnit.ID, Name: v.WorkUnit.Name}
	}
	writeJSON(w, http.StatusOK, out)
}
