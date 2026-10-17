package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/service"
)

// projectResource is the stable JSON representation of a project (WBS 3.14).
// It is deliberately separate from the domain struct so the API contract can
// evolve independently of internal state.
type projectResource struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toResource(p *project.Project) projectResource {
	return projectResource{
		ID:          p.ID.String(),
		Name:        p.Name,
		Description: p.Description,
		Status:      p.Status.String(),
		Version:     p.Version,
		CreatedAt:   p.CreatedAt.UTC(),
		UpdatedAt:   p.UpdatedAt.UTC(),
	}
}

// projectCreateRequest / projectPatchRequest are the stable request shapes.
type projectCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// projectPatchRequest carries optional fields; absent keys leave the stored
// value untouched.
// projectPatchRequest carries optional fields; absent keys leave the stored
// value untouched. Version is required (WBS 3.24 optimistic concurrency).
type projectPatchRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Version     *int    `json:"version"`
}

// allowedProjectFields is the closed field set accepted on project mutation
// payloads. Unknown fields are rejected with 400 (WBS 3.14: stable
// representation; P3-23 will centralize this rule across the API).
var allowedProjectFields = map[string]bool{"name": true, "description": true, "version": true}

// serviceError maps the service-level error set onto the centralized
// structured error responses (invalid input 400, not found 404, conflict
// 409, everything else 500) with resource-specific codes.
func serviceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_project", "Project input is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("project state conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("project operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type projectAPI struct {
	svc *service.ProjectService
}

// RegisterProjectRoutes exposes the project REST endpoints (WBS 3.14) on the
// /api/v1 namespace, backed by the project service.
func RegisterProjectRoutes(r *Router, svc *service.ProjectService) error {
	api := &projectAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodPost, "/api/v1/projects", api.handleCreate},
		{http.MethodGet, "/api/v1/projects", api.handleList},
		{http.MethodGet, "/api/v1/projects/{id}", api.handleGet},
		{http.MethodPatch, "/api/v1/projects/{id}", api.handlePatch},
		{http.MethodDelete, "/api/v1/projects/{id}", api.handleDelete},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func (a *projectAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req projectCreateRequest
	if err := decodeStrictJSON(r, "invalid_project", allowedProjectFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, _, err := a.svc.CreateProject(r.Context(), req.Name, req.Description)
	if err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+p.ID.String())
	writeJSON(w, http.StatusCreated, toResource(p))
}

func (a *projectAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResource(p))
}

// parseListQuery converts ?status= / ?limit= / ?offset= into a repository
// filter, rejecting unknown statuses and malformed numbers with 400
// (WBS 3.22).
func parseListQuery(q url.Values) (persistence.ProjectListFilter, error) {
	var f persistence.ProjectListFilter
	if status := q.Get("status"); status != "" {
		if _, err := project.ParseStatus(status); err != nil {
			return f, NewValidationError("unknown status " + status)
		}
		f.Status = project.ProjectStatus(status)
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return f, NewValidationError("limit must be a positive integer")
		}
		f.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return f, NewValidationError("offset must be a non-negative integer")
		}
		f.Offset = n
	}
	return f, nil
}

func (a *projectAPI) handleList(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListQuery(r.URL.Query())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.ListProjects(r.Context(), filter)
	if err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	list := make([]projectResource, 0, len(out))
	for _, p := range out {
		list = append(list, toResource(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list})
}

// handlePatch is the optimistic-concurrency project update (WBS 3.24): the
// client must echo the version it read, and a stale version is a 409 that
// leaves the current data untouched.
func (a *projectAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	var req projectPatchRequest
	if err := decodeStrictJSON(r, "invalid_project", allowedProjectFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Version == nil {
		WriteError(w, r, NewValidationFields("invalid_project", "Project input is invalid.",
			map[string]string{"version": "version is required"}))
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		WriteError(w, r, NewValidationFields("invalid_project", "Project input is invalid.",
			map[string]string{"name": "must not be empty"}))
		return
	}
	p, err := a.svc.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Description != nil {
		p.Description = *req.Description
	}
	updated, err := a.svc.UpdateProjectWithVersion(r.Context(), p, *req.Version)
	if err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResource(updated))
}

func (a *projectAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := a.svc.DeleteProject(r.Context(), r.PathValue("id")); err != nil {
		WriteError(w, r, serviceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
