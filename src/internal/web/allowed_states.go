package web

import (
	"net/http"

	"overseer/internal/service"
)

// allowedStatePutRequest is the WBS 5.25 PUT payload: the complete replacement
// set of allowed states for the role.
type allowedStatePutRequest struct {
	States []string `json:"states"`
}

var allowedStateFields = map[string]bool{"states": true}

type allowedStateAPI struct {
	svc *service.RoleService
}

// RegisterAllowedStateRoutes exposes the allowed-state configuration endpoint
// (WBS 5.25):
//
//	PUT /api/v1/projects/{project}/roles/{role}/states
//
// The put replaces the role's allowed states atomically.
func RegisterAllowedStateRoutes(r *Router, svc *service.RoleService) error {
	api := &allowedStateAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodPut, "/api/v1/projects/{project}/roles/{role}/states", api.handlePut); err != nil {
		return err
	}
	return nil
}

func (a *allowedStateAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req allowedStatePutRequest
	if err := decodeStrictJSON(r, "invalid_allowed_states", allowedStateFields, []string{"states"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.SetAllowedStates(r.Context(), r.PathValue("role"), req.States)
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"states": out})
}
