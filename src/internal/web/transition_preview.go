// P6-31 (WBS 6.41): transition-preview REST. POST
// /api/v1/projects/{project}/transitions/preview evaluates an outcome
// over the project's current runtime state WITHOUT applying it — the
// debugging surface the WBS calls out for workflow configuration.
// Request: {"outcome", "context_usage", "context_threshold"} (the WBS
// 6.41 example shape). Response: the current state, the evaluated
// outcome, and the selected transition (null when no selection).
// POST-only: the route has no other methods (the Go 1.22 mux 405s
// them) and the op is read-only.
package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
)

// transitionPreviewRequest is the closed request payload (WBS 6.41):
// the outcome token + the caller-supplied context values. All three
// fields are required — the context values are the declared condition
// inputs, and a preview without them would evaluate conditions
// against zeros (the strict-decode presence rule, P3-23).
type transitionPreviewRequest struct {
	Outcome          string  `json:"outcome"`
	ContextUsage     float64 `json:"context_usage"`
	ContextThreshold float64 `json:"context_threshold"`
}

// transitionPreviewResource is the stable JSON view of a preview
// (WBS 6.41): current_state (the evaluation's source), the evaluated
// outcome, and to_state — the selected transition's destination
// (null when the selection is empty: no match or condition-gated
// out).
type transitionPreviewResource struct {
	CurrentState string  `json:"current_state"`
	Outcome      string  `json:"outcome"`
	ToState      *string `json:"to_state"`
}

func transitionPreviewServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_transition_preview", "Transition preview request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("runtime state")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("ambiguous transition selection")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("transition preview operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type transitionPreviewAPI struct {
	svc *service.TransitionPreviewService
}

// RegisterTransitionPreviewRoutes exposes the transition-preview
// endpoint (WBS 6.41):
//
//	POST /api/v1/projects/{project}/transitions/preview
//
// POST-only — the op evaluates without applying, and no other method
// is registered on the path.
func RegisterTransitionPreviewRoutes(r *Router, svc *service.TransitionPreviewService) error {
	api := &transitionPreviewAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodPost, "/api/v1/projects/{project}/transitions/preview", api.handlePreview); err != nil {
		return err
	}
	return nil
}

// handlePreview — T6.41.1: the selected transition is returned;
// T6.41.2–T6.41.4 are guaranteed by the read-only service op (no
// writes on the path).
func (a *transitionPreviewAPI) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req transitionPreviewRequest
	if err := decodeStrictJSON(r, "invalid_transition_preview",
		map[string]bool{"outcome": true, "context_usage": true, "context_threshold": true},
		[]string{"outcome", "context_usage", "context_threshold"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.Preview(r.Context(), r.PathValue("project"), &service.PreviewTransitionInput{
		Outcome:          req.Outcome,
		ContextUsage:     req.ContextUsage,
		ContextThreshold: req.ContextThreshold,
	})
	if err != nil {
		WriteError(w, r, transitionPreviewServiceError(err))
		return
	}
	res := transitionPreviewResource{CurrentState: out.CurrentState, Outcome: out.Outcome}
	if out.Selected != nil {
		res.ToState = &out.Selected.ToStateID
	}
	writeJSON(w, http.StatusOK, res)
}
