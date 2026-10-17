package web

import (
	"errors"
	"net/http"

	"overseer/internal/persistence"
	"overseer/internal/service"
)

// instructionResource is the JSON wire format for a reusable instruction
// document (WBS 5.23).
type instructionResource struct {
	ID        string  `json:"id"`
	ProjectID *string `json:"project_id,omitempty"`
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Content   string  `json:"content"`
	Version   int     `json:"version"`
	Enabled   bool    `json:"enabled"`
}

func toInstructionResource(v service.InstructionView) instructionResource {
	return instructionResource{ID: v.ID, ProjectID: v.ProjectID, Key: v.Key, Name: v.Name, Content: v.Content, Version: v.Version, Enabled: v.Enabled}
}

type createInstructionRequest struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

type updateInstructionRequest struct {
	Content         string `json:"content"`
	ExpectedVersion int    `json:"expected_version"`
}

var allowedInstructionFields = map[string]bool{"key": true, "name": true, "description": true, "content": true}
var allowedInstructionUpdateFields = map[string]bool{"content": true, "expected_version": true}

// RegisterInstructionRoutes wires the instruction CRUD REST endpoints (WBS
// 5.23) on top of the existing role routes.
func RegisterInstructionRoutes(r *Router, svc *service.RoleService) error {
	api := &instructionAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/instructions", api.handleList},
		{http.MethodPost, "/api/v1/projects/{project}/instructions", api.handleCreate},
		{http.MethodGet, "/api/v1/projects/{project}/instructions/{instructionID}", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{project}/instructions/{instructionID}", api.handleUpdate},
		{http.MethodDelete, "/api/v1/projects/{project}/instructions/{instructionID}", api.handleDelete},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

type instructionAPI struct{ svc *service.RoleService }

func (a *instructionAPI) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.ListInstructions(r.Context(), scopePID(r.PathValue("project")))
	if err != nil {
		WriteError(w, r, instructionServiceError(err))
		return
	}
	list := make([]instructionResource, 0, len(out))
	for _, rec := range out {
		list = append(list, toInstructionResource(*rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instructions": list})
}

func (a *instructionAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createInstructionRequest
	if err := decodeStrictJSON(r, "invalid_instruction", allowedInstructionFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	created, err := a.svc.CreateInstruction(r.Context(), scopePID(r.PathValue("project")), req.Key, req.Name, req.Description, req.Content)
	if err != nil {
		WriteError(w, r, instructionServiceError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toInstructionResource(*created))
}

func (a *instructionAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	got, err := a.svc.GetInstruction(r.Context(), r.PathValue("instructionID"))
	if err != nil {
		WriteError(w, r, instructionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toInstructionResource(*got))
}

func (a *instructionAPI) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateInstructionRequest
	if err := decodeStrictJSON(r, "invalid_instruction", allowedInstructionUpdateFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	updated, err := a.svc.UpdateInstruction(r.Context(), r.PathValue("instructionID"), req.Content, req.ExpectedVersion)
	if err != nil {
		WriteError(w, r, instructionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toInstructionResource(*updated))
}

func (a *instructionAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteInstruction(r.Context(), r.PathValue("instructionID")); err != nil {
		WriteError(w, r, instructionServiceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func instructionServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, persistence.ErrInvalidReference):
		return NewValidationCode("invalid_instruction", "Instruction input is invalid.")
	case errors.Is(err, service.ErrNotFound), errors.Is(err, persistence.ErrNotFound):
		return NewNotFoundError("instruction")
	case errors.Is(err, service.ErrConflict), errors.Is(err, persistence.ErrConflict):
		return NewConflictError("instruction conflict (key taken or referenced by a role)")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("instruction operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}
