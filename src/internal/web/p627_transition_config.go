// P6-27 (WBS 6.37): the transition-configuration REST surface. Transition
// management within an editable state-machine definition: create transition,
// update transition (incl. disable), configure condition, configure
// priority.
//
// Routes live under /api/v1/projects/{project}/state-machine/transitions
// (the P6-25 parent surface); unregistered API paths 404. Stable JSON DTOs,
// strict decode with per-resource allow-lists, the standard serviceError
// mapping.
package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
	"overseer/internal/state"
)

// stateOp is the web-to-domain operator conversion for condition payloads.
func stateOp(o string) state.ConditionOperator { return state.ConditionOperator(o) }

// transitionConditionResource is the stable JSON view of a transition's
// structured condition: the value is exactly one of literal / context_field
// (the other is null).
type transitionConditionResource struct {
	Field        string `json:"field"`
	Operator     string `json:"operator"`
	Literal      any    `json:"literal"`
	ContextField string `json:"context_field"`
}

// transitionConfigResource is the stable JSON view of a configured
// transition, addressed by id and state keys (never state row ids).
type transitionConfigResource struct {
	ID        string                       `json:"id"`
	From      string                       `json:"from"`
	To        string                       `json:"to"`
	Outcome   string                       `json:"outcome"`
	Priority  int                          `json:"priority"`
	Enabled   bool                         `json:"enabled"`
	Condition *transitionConditionResource `json:"condition"`
}

func toTransitionConfigResource(v *service.TransitionConfigView) transitionConfigResource {
	out := transitionConfigResource{
		ID: v.ID, From: v.From, To: v.To, Outcome: v.Outcome,
		Priority: v.Priority, Enabled: v.Enabled,
	}
	if v.Condition != nil {
		out.Condition = &transitionConditionResource{
			Field: v.Condition.Field, Operator: string(v.Condition.Operator),
			Literal: v.Condition.Literal, ContextField: v.Condition.ContextField,
		}
	}
	return out
}

// transitionCreateRequest is the POST transitions payload: the condition is
// the P6-04 shape (field, operator, exactly one of literal / context_field).
type transitionCreateRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Outcome   string `json:"outcome"`
	Priority  int    `json:"priority"`
	Enabled   *bool  `json:"enabled"`
	Condition *struct {
		Field        string `json:"field"`
		Operator     string `json:"operator"`
		Literal      any    `json:"literal"`
		ContextField string `json:"context_field"`
	} `json:"condition"`
}

var allowedTransitionCreateFields = map[string]bool{
	"from": true, "to": true, "outcome": true, "priority": true,
	"enabled": true, "condition": true,
}

// transitionUpdateRequest is the PUT transitions/{id} payload: every field
// is optional (pointer = "not supplied, keep stored value").
type transitionUpdateRequest struct {
	Outcome  *string `json:"outcome"`
	Priority *int    `json:"priority"`
	Enabled  *bool   `json:"enabled"`
}

var allowedTransitionUpdateFields = map[string]bool{
	"outcome": true, "priority": true, "enabled": true,
}

// transitionConditionRequest is the PUT condition payload: field + operator
// are required; the value is exactly one of literal / context_field (a
// body with neither clears the condition).
type transitionConditionRequest struct {
	Field        string `json:"field"`
	Operator     string `json:"operator"`
	Literal      any    `json:"literal"`
	ContextField string `json:"context_field"`
}

var allowedTransitionConditionFields = map[string]bool{
	"field": true, "operator": true, "literal": true, "context_field": true,
}

type transitionPriorityRequest struct {
	Priority int `json:"priority"`
}

var allowedTransitionPriorityFields = map[string]bool{"priority": true}

func transitionConfigServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_transition", "Transition configuration is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("transition")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("ambiguous transition configuration (same source, outcome and priority)")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("transition configuration operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type transitionConfigAPI struct {
	svc *service.TransitionConfigService
}

// RegisterTransitionConfigRoutes exposes the transition-configuration
// endpoints (WBS 6.37):
//
//	POST   /api/v1/projects/{project}/state-machine/transitions
//	GET    /api/v1/projects/{project}/state-machine/transitions
//	PUT    /api/v1/projects/{project}/state-machine/transitions/{id}
//	POST   /api/v1/projects/{project}/state-machine/transitions/{id}/disable
//	PUT    /api/v1/projects/{project}/state-machine/transitions/{id}/condition
//	PUT    /api/v1/projects/{project}/state-machine/transitions/{id}/priority
func RegisterTransitionConfigRoutes(r *Router, svc *service.TransitionConfigService) error {
	api := &transitionConfigAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodPost, "/api/v1/projects/{project}/state-machine/transitions", api.handleCreate},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/transitions", api.handleList},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/transitions/{id}", api.handleUpdate},
		{http.MethodPost, "/api/v1/projects/{project}/state-machine/transitions/{id}/disable", api.handleDisable},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/transitions/{id}/condition", api.handleCondition},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine/transitions/{id}/priority", api.handlePriority},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// handleCreate — T6.37.1/T6.37.2: create a transition (with its condition,
// which is persisted).
func (a *transitionConfigAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req transitionCreateRequest
	if err := decodeStrictJSON(r, "invalid_transition", allowedTransitionCreateFields,
		[]string{"from", "to", "outcome"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := &service.CreateTransitionInput{
		FromKey: req.From, ToKey: req.To, Outcome: req.Outcome,
		Priority: req.Priority, Enabled: req.Enabled,
	}
	if req.Condition != nil {
		in.Condition = &service.TransitionConditionInput{
			Field: req.Condition.Field, Operator: stateOp(req.Condition.Operator),
			Literal: req.Condition.Literal, ContextField: req.Condition.ContextField,
		}
	}
	view, err := a.svc.CreateTransition(r.Context(), r.PathValue("project"), in)
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toTransitionConfigResource(view))
}

// handleList: the current machine version's transitions.
func (a *transitionConfigAPI) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := a.svc.ListTransitions(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	out := make([]transitionConfigResource, 0, len(list))
	for _, v := range list {
		out = append(out, toTransitionConfigResource(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": out})
}

// handleUpdate — T6.37.3: update the transition's editable configuration.
func (a *transitionConfigAPI) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req transitionUpdateRequest
	if err := decodeStrictJSON(r, "invalid_transition", allowedTransitionUpdateFields,
		nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.UpdateTransition(r.Context(), r.PathValue("project"), r.PathValue("id"), &service.UpdateTransitionInput{
		Outcome: req.Outcome, Priority: req.Priority, Enabled: req.Enabled,
	})
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransitionConfigResource(view))
}

// handleDisable: disable the transition.
func (a *transitionConfigAPI) handleDisable(w http.ResponseWriter, r *http.Request) {
	view, err := a.svc.DisableTransition(r.Context(), r.PathValue("project"), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransitionConfigResource(view))
}

// handleCondition — T6.37.2: set (or clear) the transition's condition; it
// is persisted, not just returned.
func (a *transitionConfigAPI) handleCondition(w http.ResponseWriter, r *http.Request) {
	var req transitionConditionRequest
	if err := decodeStrictJSON(r, "invalid_transition", allowedTransitionConditionFields,
		[]string{"field", "operator"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := &service.TransitionConditionInput{
		Field: req.Field, Operator: stateOp(req.Operator),
		Literal: req.Literal, ContextField: req.ContextField,
	}
	view, err := a.svc.ConfigureCondition(r.Context(), r.PathValue("project"), r.PathValue("id"), in)
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransitionConfigResource(view))
}

// handlePriority — T6.37.3: set the transition's priority.
func (a *transitionConfigAPI) handlePriority(w http.ResponseWriter, r *http.Request) {
	var req transitionPriorityRequest
	if err := decodeStrictJSON(r, "invalid_transition", allowedTransitionPriorityFields,
		[]string{"priority"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	view, err := a.svc.ConfigurePriority(r.Context(), r.PathValue("project"), r.PathValue("id"), req.Priority)
	if err != nil {
		WriteError(w, r, transitionConfigServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransitionConfigResource(view))
}
