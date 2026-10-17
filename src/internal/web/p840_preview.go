// P8-40 (WBS 8.35): the prompt-preview REST surface.
//
// POST /api/v1/projects/{project}/prompts/preview renders the canonical
// prompt for the project's current position WITHOUT persisting an execution
// and WITHOUT calling an LLM (WBS 8.35). The request body is optional (an
// empty object previews the default position); the response carries the
// rendered content + the WBS 8.35 metadata (state, role, template, estimated
// tokens, component sizes, snapshot hash, prompt hash) + the missing-required
// report (T8.35.4).
package web

import (
	"errors"
	"net/http"
	"strings"

	"overseer/internal/service"
)

// promptPreviewRequest is the preview POST payload: every field is optional.
// The template identity pins the preview to a specific template version; when
// absent the state's referenced template (at its current version) is used.
type promptPreviewRequest struct {
	TemplateID             string `json:"template_id"`
	TemplateVersion        int    `json:"template_version"`
	ContinuationStatusID   string `json:"continuation_status_id"`
	ContinuationWorkUnitID string `json:"continuation_work_unit_id"`
}

var promptPreviewFields = map[string]bool{
	"template_id":               true,
	"template_version":          true,
	"continuation_status_id":    true,
	"continuation_work_unit_id": true,
}

// promptPreviewComponentEntry is one component-size metadata row (WBS 8.35).
type promptPreviewComponentEntry struct {
	Type          string `json:"type"`
	SourceID      string `json:"source_id"`
	SourceVersion int    `json:"source_version"`
	TokenEstimate int    `json:"token_estimate"`
}

// promptPreviewResource is the stable JSON view of a preview (WBS 8.35).
type promptPreviewResource struct {
	StateKey        string                        `json:"state"`
	RoleKey         string                        `json:"role"`
	TemplateID      string                        `json:"template_id"`
	TemplateVersion int                           `json:"template_version"`
	Content         string                        `json:"content"`
	Hash            string                        `json:"hash"`
	EstimatedTokens int                           `json:"estimated_tokens"`
	SnapshotHash    string                        `json:"snapshot_hash"`
	Components      []promptPreviewComponentEntry `json:"components"`
	MissingRequired []string                      `json:"missing_required"`
}

func promptPreviewServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_prompt_preview", "Prompt preview request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("prompt preview position")
	case errors.Is(err, service.ErrConflict):
		return NewConflictCode("prompt_not_composable", strings.TrimPrefix(err.Error(), "service: conflict: "))
	case errors.Is(err, service.ErrMissingStateFile):
		return NewValidationCode("missing_required", "A required prompt component is absent at this position.")
	default:
		return NewInternalError("prompt preview failed")
	}
}

type promptPreviewAPI struct {
	svc *service.PromptPreviewService
}

// RegisterPromptPreviewRoutes exposes the prompt-preview endpoint (WBS
// 8.35):
//
//	POST /api/v1/projects/{project}/prompts/preview
func RegisterPromptPreviewRoutes(r *Router, svc *service.PromptPreviewService) error {
	api := &promptPreviewAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodPost, "/api/v1/projects/{project}/prompts/preview", api.handlePreview); err != nil {
		return err
	}
	return nil
}

// handlePreview — T8.35.1-T8.35.5: the preview renders the canonical prompt
// with metadata, persists nothing, and is deterministic over the pinned
// position (the no-persist / no-LLM / runtime-unchanged proofs are pinned in
// the integration tests over the live HTTP surface).
func (a *promptPreviewAPI) handlePreview(w http.ResponseWriter, r *http.Request) {
	var body promptPreviewRequest
	// The body is optional: an empty/absent body previews the default
	// position; a present body must be a JSON object from the closed set.
	if err := decodeStrictOptionalJSON(r, "invalid_prompt_preview", promptPreviewFields, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	req, err := service.NewPromptPreviewRequest(service.PromptPreviewRequestInput{
		TemplateID:             body.TemplateID,
		TemplateVersion:        body.TemplateVersion,
		ContinuationStatusID:   body.ContinuationStatusID,
		ContinuationWorkUnitID: body.ContinuationWorkUnitID,
	})
	if err != nil {
		WriteError(w, r, promptPreviewServiceError(err))
		return
	}
	out, err := a.svc.Preview(r.Context(), r.PathValue("project"), req)
	if err != nil {
		WriteError(w, r, promptPreviewServiceError(err))
		return
	}
	res := promptPreviewResource{
		StateKey:        out.StateKey,
		RoleKey:         out.RoleKey,
		TemplateID:      out.TemplateID.String(),
		TemplateVersion: out.TemplateVersion,
		Content:         out.Content,
		Hash:            out.Hash,
		EstimatedTokens: out.EstimatedTokens,
		SnapshotHash:    out.SnapshotHash,
		Components:      make([]promptPreviewComponentEntry, 0, len(out.Components)),
		MissingRequired: out.MissingRequired,
	}
	if res.MissingRequired == nil {
		res.MissingRequired = []string{}
	}
	for _, c := range out.Components {
		res.Components = append(res.Components, promptPreviewComponentEntry{
			Type:          string(c.Type),
			SourceID:      c.SourceID.String(),
			SourceVersion: c.SourceVersion,
			TokenEstimate: c.TokenEstimate,
		})
	}
	writeJSON(w, http.StatusOK, res)
}
