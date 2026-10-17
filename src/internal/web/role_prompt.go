package web

import (
	"net/http"

	"overseer/internal/service"
)

type rolePromptResource struct {
	Version     int    `json:"version"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
}

func toRolePromptResource(v *service.SystemPromptView) rolePromptResource {
	return rolePromptResource{Version: v.Version, Content: v.Content, ContentHash: v.ContentHash}
}

// rolePromptServiceError maps service/persistence errors for the prompt
// surface. It reuses the role mapper (which handles both sentinel sets).
func rolePromptServiceError(err error) error { return roleServiceError(err) }

type rolePromptAPI struct {
	svc *service.RoleService
}

// RegisterRolePromptRoutes exposes the system-prompt endpoints (WBS 5.22):
//
//	GET /api/v1/projects/{project}/roles/{role}/system-prompt
//	PUT /api/v1/projects/{project}/roles/{role}/system-prompt
//	GET /api/v1/projects/{project}/roles/{role}/system-prompt/versions
func RegisterRolePromptRoutes(r *Router, svc *service.RoleService) error {
	api := &rolePromptAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/roles/{roleID}/system-prompt", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{project}/roles/{roleID}/system-prompt", api.handlePut},
		{http.MethodGet, "/api/v1/projects/{project}/roles/{roleID}/system-prompt/versions", api.handleVersions},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

type rolePromptPutRequest struct {
	Content         string `json:"content"`
	ExpectedVersion int    `json:"expected_version"`
}

func (a *rolePromptAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.GetCurrentSystemPrompt(r.Context(), r.PathValue("roleID"))
	if err != nil {
		WriteError(w, r, rolePromptServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toRolePromptResource(v))
}

func (a *rolePromptAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req rolePromptPutRequest
	if err := decodeStrictJSON(r, "invalid_system_prompt", map[string]bool{"content": true, "expected_version": true}, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.svc.UpdateSystemPrompt(r.Context(), r.PathValue("roleID"), req.Content, req.ExpectedVersion)
	if err != nil {
		WriteError(w, r, rolePromptServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toRolePromptResource(v))
}

func (a *rolePromptAPI) handleVersions(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.ListSystemPromptVersions(r.Context(), r.PathValue("roleID"))
	if err != nil {
		WriteError(w, r, rolePromptServiceError(err))
		return
	}
	list := make([]rolePromptResource, 0, len(out))
	for _, v := range out {
		list = append(list, toRolePromptResource(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": list})
}
