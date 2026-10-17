package web

import (
	"errors"
	"net/http"

	"overseer/internal/persistence"
	"overseer/internal/service"
)

// roleResource is the stable JSON representation of a role (WBS 5.21).
type roleResource struct {
	ID          string  `json:"id"`
	ProjectID   *string `json:"project_id"`
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Enabled     bool    `json:"enabled"`
	Version     int     `json:"version"`
}

func toRoleResource(r *persistence.RoleRecord) roleResource {
	var pid *string
	if r.ProjectID != nil {
		s := r.ProjectID.String()
		pid = &s
	}
	return roleResource{
		ID:          r.ID.String(),
		ProjectID:   pid,
		Key:         r.Key,
		Name:        r.Name,
		Description: r.Description,
		Enabled:     r.Enabled,
		Version:     r.Version,
	}
}

// allowedRoleFields is the closed field set for role create/patch payloads.
var allowedRoleFields = map[string]bool{"key": true, "name": true, "description": true, "system_prompt": true}

type roleCreateRequest struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"`
}

type rolePatchRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

func roleServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, persistence.ErrInvalidReference):
		return NewValidationCode("invalid_role", "Role input is invalid.")
	case errors.Is(err, service.ErrNotFound), errors.Is(err, persistence.ErrNotFound):
		return NewNotFoundError("role")
	case errors.Is(err, service.ErrConflict), errors.Is(err, persistence.ErrConflict):
		return NewConflictError("role conflict (key taken or historically referenced)")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("role operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type roleAPI struct {
	svc *service.RoleService
}

// RegisterRoleRoutes exposes the role CRUD endpoints (WBS 5.21):
// GET/POST /api/v1/projects/{project}/roles and
// GET/PATCH/DELETE /api/v1/projects/{project}/roles/{roleID}.
// project == "global" addresses the global scope.
func RegisterRoleRoutes(r *Router, svc *service.RoleService) error {
	api := &roleAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/roles", api.handleList},
		{http.MethodPost, "/api/v1/projects/{project}/roles", api.handleCreate},
		{http.MethodGet, "/api/v1/projects/{project}/roles/{roleID}", api.handleGet},
		{http.MethodPatch, "/api/v1/projects/{project}/roles/{roleID}", api.handlePatch},
		{http.MethodDelete, "/api/v1/projects/{project}/roles/{roleID}", api.handleDelete},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// scopePID converts the {project} path value to a *string scope (nil = global).
func scopePID(project string) *string {
	if project == "global" {
		return nil
	}
	return &project
}

func (a *roleAPI) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.ListRoles(r.Context(), scopePID(r.PathValue("project")))
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	list := make([]roleResource, 0, len(out))
	for _, rec := range out {
		list = append(list, toRoleResource(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": list})
}

func (a *roleAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req roleCreateRequest
	if err := decodeStrictJSON(r, "invalid_role", allowedRoleFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	rec, err := a.svc.CreateRole(r.Context(), scopePID(r.PathValue("project")), req.Key, req.Name, req.Description, req.SystemPrompt)
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toRoleResource(rec))
}

func (a *roleAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	rec, err := a.svc.GetRole(r.Context(), r.PathValue("roleID"))
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toRoleResource(rec))
}

func (a *roleAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	var req rolePatchRequest
	if err := decodeStrictJSON(r, "invalid_role", map[string]bool{"name": true, "description": true, "enabled": true}, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	roleID := r.PathValue("roleID")
	rec, err := a.svc.GetRole(ctx, roleID)
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	name, desc := rec.Name, rec.Description
	if req.Name != nil {
		name = *req.Name
	}
	if req.Description != nil {
		desc = *req.Description
	}
	updated, err := a.svc.UpdateRole(ctx, roleID, name, desc)
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	if req.Enabled != nil {
		if *req.Enabled {
			err = a.svc.Enable(ctx, roleID)
		} else {
			err = a.svc.Disable(ctx, roleID)
		}
		if err != nil {
			WriteError(w, r, roleServiceError(err))
			return
		}
		updated, _ = a.svc.GetRole(ctx, roleID)
	}
	writeJSON(w, http.StatusOK, toRoleResource(updated))
}

func (a *roleAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteRole(r.Context(), r.PathValue("project"), r.PathValue("roleID")); err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
