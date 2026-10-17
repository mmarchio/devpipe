package web

import (
	"net/http"

	"overseer/internal/service"
)

// stateFilePutRequest is the WBS 5.26 PUT payload: the ordered list of state
// files the role requires.
type stateFilePutRequest struct {
	StateFiles []string `json:"state_files"`
}

var stateFileFields = map[string]bool{"state_files": true}

type stateFileAPI struct {
	svc *service.RoleService
}

// RegisterStateFileRoutes exposes the state-file requirement endpoint
// (WBS 5.26):
//
//	PUT /api/v1/projects/{project}/roles/{role}/state-files
//
// The put replaces the role's required state files atomically (ordered);
// duplicates are rejected.
func RegisterStateFileRoutes(r *Router, svc *service.RoleService) error {
	api := &stateFileAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodPut, "/api/v1/projects/{project}/roles/{role}/state-files", api.handlePut); err != nil {
		return err
	}
	return nil
}

func (a *stateFileAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req stateFilePutRequest
	if err := decodeStrictJSON(r, "invalid_state_files", stateFileFields, []string{"state_files"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.SetStateFiles(r.Context(), r.PathValue("role"), req.StateFiles)
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state_files": out})
}
