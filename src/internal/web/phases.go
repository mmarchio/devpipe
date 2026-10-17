package web

import (
	"errors"
	"net/http"
	"time"

	"overseer/internal/phase"
	"overseer/internal/service"
)

// phaseResource is the stable JSON representation of a phase (WBS 3.16).
// It is deliberately separate from the domain struct.
type phaseResource struct {
	ID          string     `json:"id"`
	Order       int        `json:"order"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func toPhaseResource(p *phase.Phase) phaseResource {
	var started, completed *time.Time
	if p.StartedAt != nil {
		ts := p.StartedAt.UTC()
		started = &ts
	}
	if p.CompletedAt != nil {
		ts := p.CompletedAt.UTC()
		completed = &ts
	}
	return phaseResource{
		ID:          p.ID.String(),
		Order:       p.Order,
		Name:        p.Name,
		Description: p.Description,
		Status:      p.Status.String(),
		CreatedAt:   p.CreatedAt.UTC(),
		UpdatedAt:   p.UpdatedAt.UTC(),
		StartedAt:   started,
		CompletedAt: completed,
	}
}

// allowedPhaseFields is the closed field set accepted on phase mutation
// payloads (WBS 3.16: order is server-assigned, so a user-supplied order is
// an unknown field and 400 — T3.16.4).
var allowedPhaseFields = map[string]bool{"name": true, "description": true}

// allowedPhaseCreateFields adds the optional initial WBS on create: when
// present the phase and its WBS v1 are stored in one transaction (WBS 4.28).
var allowedPhaseCreateFields = map[string]bool{"name": true, "description": true, "wbs": true}

// phaseCreateRequest is the stable POST body.
type phaseCreateRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	WBS         *string `json:"wbs"`
}

// phasePatchRequest carries optional fields; absent keys leave the stored
// value untouched.
type phasePatchRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// phaseServiceError maps service-level errors onto web error responses for
// the phase surface (400 / 404 / 409 / 500).
func phaseServiceError(err error) error {
	var oe *service.PhaseOrderError
	switch {
	case errors.As(err, &oe) && errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_phase_order", oe.Message)
	case errors.As(err, &oe):
		return NewConflictError(oe.Message)
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_phase", "Phase input is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("phase")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("phase state conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("phase operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type phaseAPI struct {
	svc *service.PhaseService
}

// RegisterPhaseRoutes exposes the phase REST endpoints (WBS 3.16):
// POST/GET /api/v1/projects/{id}/phases and
// GET/PATCH/DELETE /api/v1/projects/{id}/phases/{phaseID}.
func RegisterPhaseRoutes(r *Router, svc *service.PhaseService) error {
	api := &phaseAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodPost, "/api/v1/projects/{id}/phases", api.handleCreate},
		{http.MethodGet, "/api/v1/projects/{id}/phases", api.handleList},
		{http.MethodGet, "/api/v1/projects/{id}/phases/{phaseID}", api.handleGet},
		{http.MethodPatch, "/api/v1/projects/{id}/phases/{phaseID}", api.handlePatch},
		{http.MethodDelete, "/api/v1/projects/{id}/phases/{phaseID}", api.handleDelete},
		{http.MethodPut, "/api/v1/projects/{id}/phases/order", api.handleReorder},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func (a *phaseAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req phaseCreateRequest
	if err := decodeStrictJSON(r, "invalid_phase", allowedPhaseCreateFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var p *phase.Phase
	var err error
	if req.WBS != nil {
		p, _, err = a.svc.CreatePhaseWithWBS(r.Context(), r.PathValue("id"), req.Name, req.Description, *req.WBS)
	} else {
		p, err = a.svc.CreatePhase(r.Context(), r.PathValue("id"), req.Name, req.Description)
	}
	if err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toPhaseResource(p))
}

func (a *phaseAPI) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.ListPhases(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	list := make([]phaseResource, 0, len(out))
	for _, p := range out {
		list = append(list, toPhaseResource(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"phases": list})
}

func (a *phaseAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.GetPhase(r.Context(), r.PathValue("id"), r.PathValue("phaseID"))
	if err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toPhaseResource(p))
}

func (a *phaseAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	var req phasePatchRequest
	if err := decodeStrictJSON(r, "invalid_phase", allowedPhaseFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	updated, err := a.svc.UpdatePhase(r.Context(), r.PathValue("id"), r.PathValue("phaseID"), req.Name, req.Description)
	if err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toPhaseResource(updated))
}

// allowedPhaseOrderFields is the closed field set of a reorder.
var allowedPhaseOrderFields = map[string]bool{"phase_ids": true}

type phaseOrderRequest struct {
	PhaseIDs []string `json:"phase_ids"`
}

// handleReorder — PUT /api/v1/projects/{id}/phases/order: every phase id,
// first to last. Only phases that have not started can move.
func (a *phaseAPI) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req phaseOrderRequest
	if err := decodeStrictJSON(r, "invalid_phase_order", allowedPhaseOrderFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(req.PhaseIDs) == 0 {
		WriteError(w, r, NewValidationCode("invalid_phase_order", "phase_ids must list every phase of the project"))
		return
	}
	out, err := a.svc.ReorderPhases(r.Context(), r.PathValue("id"), req.PhaseIDs)
	if err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	list := make([]phaseResource, 0, len(out))
	for _, p := range out {
		list = append(list, toPhaseResource(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"phases": list})
}

func (a *phaseAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeletePhase(r.Context(), r.PathValue("id"), r.PathValue("phaseID")); err != nil {
		WriteError(w, r, phaseServiceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
