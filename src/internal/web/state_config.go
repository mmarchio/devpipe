// P6-26 (WBS 6.36): the state-configuration REST surface. State
// management within an editable state-machine definition: create state,
// update state (incl. disable), assign role, configure instructions,
// configure state files, configure prompt template, and destructive delete
// with the historically-referenced-state protection (T6.36.5 → 409).
//
// Routes live under /api/v1/projects/{project}/state-machine/states
// (the P6-25 parent surface); unregistered API paths 404. Stable JSON
// DTOs, strict decode with per-resource allow-lists, the standard
// serviceError mapping.
package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
)

// stateConfigResource is the stable JSON view of a configured state.
type stateConfigResource struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	PromptTemplate  string   `json:"prompt_template"`
	Kind            string   `json:"kind"`
	Terminal        bool     `json:"terminal"`
	Enabled         bool     `json:"enabled"`
	Roles           []string `json:"roles"`
	StateFiles      []string `json:"state_files"`
	InstructionVer  int      `json:"instruction_version"`
	InstructionHash string   `json:"instruction_hash"`
}

func toStateConfigResource(v *service.StateConfigView) stateConfigResource {
	return stateConfigResource{
		Key: v.Key, Name: v.Name, Description: v.Description,
		PromptTemplate: v.PromptTemplate, Kind: v.Kind,
		Terminal: v.Terminal, Enabled: v.Enabled,
		Roles: v.Roles, StateFiles: v.StateFiles,
		InstructionVer: v.InstructionVer, InstructionHash: v.InstructionHash,
	}
}

// stateCreateRequest is the POST states payload.
type stateCreateRequest struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	PromptTemplate string `json:"prompt_template"`
	Kind           string `json:"kind"`
	Terminal       bool   `json:"terminal"`
	Enabled        *bool  `json:"enabled"`
}

var allowedStateCreateFields = map[string]bool{
	"key": true, "name": true, "description": true,
	"prompt_template": true, "kind": true, "terminal": true, "enabled": true,
}

// stateUpdateRequest is the PUT states/{key} payload: every field is
// optional (pointer = "not supplied, keep stored value").
type stateUpdateRequest struct {
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	PromptTemplate *string `json:"prompt_template"`
	Kind           *string `json:"kind"`
	Terminal       *bool   `json:"terminal"`
	Enabled        *bool   `json:"enabled"`
}

var allowedStateUpdateFields = map[string]bool{
	"name": true, "description": true, "prompt_template": true,
	"kind": true, "terminal": true, "enabled": true,
}

type stateInstructionsRequest struct {
	Content string `json:"content"`
}

var allowedStateInstructionsFields = map[string]bool{"content": true}

type stateFilesRequest struct {
	Categories []string `json:"categories"`
}

var allowedStateFilesFields = map[string]bool{"categories": true}

type stateTemplateRequest struct {
	Template string `json:"template"`
}

var allowedStateTemplateFields = map[string]bool{"template": true}

func stateConfigServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_state", "State configuration is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("state")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("state is historically referenced or duplicate")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("state configuration operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type stateConfigAPI struct {
	svc *service.StateConfigService
}

// RegisterStateConfigRoutes exposes the state-configuration endpoints
// (WBS 6.36):
//
//	POST   /api/v1/projects/{project}/state-machine/states
//	PUT    /api/v1/projects/{project}/state-machine/states/{key}
//	DELETE /api/v1/projects/{project}/state-machine/states/{key}
//	POST   /api/v1/projects/{project}/state-machine/states/{key}/roles/{role}
//	PUT    /api/v1/projects/{project}/state-machine/states/{key}/instructions
//	PUT    /api/v1/projects/{project}/state-machine/states/{key}/state-files
//	PUT    /api/v1/projects/{project}/state-machine/states/{key}/prompt-template
func RegisterStateConfigRoutes(r *Router, svc *service.StateConfigService) error {
	api := &stateConfigAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodPost, "/api/v1/projects/{project}/state-machine/states", api.handleCreate},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/states", api.handleList},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/states/{key}", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/states/{key}", api.handleUpdate},
		{http.MethodDelete, "/api/v1/projects/{project}/state-machine/states/{key}", api.handleDelete},
		{http.MethodPost, "/api/v1/projects/{project}/state-machine/states/{key}/roles/{role}", api.handleAssignRole},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/states/{key}/instructions", api.handleInstructions},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/states/{key}/state-files", api.handleStateFiles},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/states/{key}/prompt-template", api.handleTemplate},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// handleCreate — T6.36.1: create a new state in the current machine.
func (a *stateConfigAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req stateCreateRequest
	if err := decodeStrictJSON(r, "invalid_state", allowedStateCreateFields,
		[]string{"key", "name", "kind"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.CreateState(r.Context(), r.PathValue("project"), &service.CreateStateInput{
		Key: req.Key, Name: req.Name, Description: req.Description,
		PromptTemplate: req.PromptTemplate, Kind: req.Kind,
		Terminal: req.Terminal, Enabled: req.Enabled,
	})
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toStateConfigResource(view))
}

// handleUpdate — T6.36.2: update (incl. disable) an existing state.
func (a *stateConfigAPI) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req stateUpdateRequest
	if err := decodeStrictJSON(r, "invalid_state", allowedStateUpdateFields,
		nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.UpdateState(r.Context(), r.PathValue("project"), r.PathValue("key"), &service.UpdateStateInput{
		Name: req.Name, Description: req.Description,
		PromptTemplate: req.PromptTemplate, Kind: req.Kind,
		Terminal: req.Terminal, Enabled: req.Enabled,
	})
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toStateConfigResource(view))
}

// handleDelete — T6.36.5: destructive delete; historically referenced
// states are rejected with 409 BEFORE any write.
func (a *stateConfigAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := a.svc.DeleteState(r.Context(), r.PathValue("project"), r.PathValue("key"))
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAssignRole — T6.36.3: assign a role to the state's ordered list.
func (a *stateConfigAPI) handleAssignRole(w http.ResponseWriter, r *http.Request) {
	view, err := a.svc.AssignRole(r.Context(), r.PathValue("project"),
		r.PathValue("key"), r.PathValue("role"))
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toStateConfigResource(view))
}

// handleInstructions — T6.36.4: set the state's instruction (one new
// append-only version, T6.5.x semantics).
func (a *stateConfigAPI) handleInstructions(w http.ResponseWriter, r *http.Request) {
	var req stateInstructionsRequest
	if err := decodeStrictJSON(r, "invalid_state", allowedStateInstructionsFields,
		[]string{"content"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.SetStateInstructions(r.Context(), r.PathValue("project"),
		r.PathValue("key"), req.Content)
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toStateConfigResource(view))
}

// handleStateFiles — T6.36.4: replace the state's required state-file
// categories (order significant, duplicates/empty rejected).
func (a *stateConfigAPI) handleStateFiles(w http.ResponseWriter, r *http.Request) {
	var req stateFilesRequest
	if err := decodeStrictJSON(r, "invalid_state", allowedStateFilesFields,
		[]string{"categories"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.SetStateFiles(r.Context(), r.PathValue("project"),
		r.PathValue("key"), req.Categories)
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toStateConfigResource(view))
}

// handleTemplate — T6.36.4: set the state's referenced prompt-template key.
func (a *stateConfigAPI) handleTemplate(w http.ResponseWriter, r *http.Request) {
	var req stateTemplateRequest
	if err := decodeStrictJSON(r, "invalid_state", allowedStateTemplateFields,
		[]string{"template"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.SetPromptTemplate(r.Context(), r.PathValue("project"),
		r.PathValue("key"), req.Template)
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toStateConfigResource(view))
}

// handleList — read-only list of the current states with roles / files
// (WBS 17.36).
func (a *stateConfigAPI) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := a.svc.ListStates(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	out := make([]stateConfigResource, 0, len(list))
	for _, v := range list {
		out = append(out, toStateConfigResource(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"states": out})
}

// handleGet — one state with its instruction content (WBS 17.38).
func (a *stateConfigAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	v, content, err := a.svc.GetState(r.Context(), r.PathValue("project"), r.PathValue("key"))
	if err != nil {
		WriteError(w, r, stateConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": toStateConfigResource(v), "instruction": content})
}
