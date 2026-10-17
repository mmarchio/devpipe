package web

import (
	"errors"
	"net/http"

	"overseer/internal/persistence"
	"overseer/internal/service"
)

// roleInstructionAssignmentResource is the JSON wire format for one role →
// instruction assignment (WBS 5.24).
type roleInstructionAssignmentResource struct {
	InstructionID  string `json:"instruction_id"`
	Position       int    `json:"position"`
	InstructionKey string `json:"instruction_key"`
}

func toAssignmentResource(a service.RoleInstructionAssignment) roleInstructionAssignmentResource {
	return roleInstructionAssignmentResource{InstructionID: a.InstructionID, Position: a.Position, InstructionKey: a.InstructionKey}
}

type assignInstructionRequest struct {
	Instruction string `json:"instruction"`
}

var allowedAssignmentFields = map[string]bool{"instruction": true}

// RegisterAssignmentRoutes wires the role → instruction assignment REST
// endpoints (WBS 5.24):
//
//	GET    /api/v1/projects/{project}/roles/{role}/instructions
//	POST   /api/v1/projects/{project}/roles/{role}/instructions
//	DELETE /api/v1/projects/{project}/roles/{role}/instructions/{instruction}
//
// Ordering is the assignment's position (deterministic MAX+1 on create).
func RegisterAssignmentRoutes(r *Router, svc *service.RoleService) error {
	api := &assignmentAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/roles/{role}/instructions", api.handleList},
		{http.MethodPost, "/api/v1/projects/{project}/roles/{role}/instructions", api.handleAssign},
		{http.MethodDelete, "/api/v1/projects/{project}/roles/{role}/instructions/{instruction}", api.handleUnassign},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

type assignmentAPI struct{ svc *service.RoleService }

func (a *assignmentAPI) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.ListRoleInstructions(r.Context(), r.PathValue("role"))
	if err != nil {
		WriteError(w, r, assignmentServiceError(err))
		return
	}
	list := make([]roleInstructionAssignmentResource, 0, len(out))
	for _, rec := range out {
		list = append(list, toAssignmentResource(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instructions": list})
}

func (a *assignmentAPI) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req assignInstructionRequest
	if err := decodeStrictJSON(r, "invalid_assignment", allowedAssignmentFields, []string{"instruction"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.svc.AssignInstruction(r.Context(), r.PathValue("role"), req.Instruction); err != nil {
		WriteError(w, r, assignmentServiceError(err))
		return
	}
	// Return the created assignment: the new position is the current max+1,
	// which is the length of the role's (now-updated) assignment list.
	list, err := a.svc.ListRoleInstructions(r.Context(), r.PathValue("role"))
	if err != nil {
		WriteError(w, r, assignmentServiceError(err))
		return
	}
	var created roleInstructionAssignmentResource
	for _, rec := range list {
		if rec.InstructionID == req.Instruction {
			created = toAssignmentResource(rec)
			break
		}
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *assignmentAPI) handleUnassign(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.UnassignInstruction(r.Context(), r.PathValue("role"), r.PathValue("instruction")); err != nil {
		WriteError(w, r, assignmentServiceError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func assignmentServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, persistence.ErrInvalidReference):
		return NewValidationCode("invalid_assignment", "Assignment input is invalid.")
	case errors.Is(err, service.ErrNotFound), errors.Is(err, persistence.ErrNotFound):
		return NewNotFoundError("assignment")
	case errors.Is(err, service.ErrConflict), errors.Is(err, persistence.ErrConflict):
		return NewConflictError("assignment conflict (duplicate (role, instruction))")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("assignment operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}
