// P8-42 (WBS 8.37): the prompt provenance REST surface.
//
//	GET /api/v1/projects/{project}/prompts/{prompt}/provenance
//
// returns every source the canonical prompt was composed from, with the
// PINNED versions and hashes, in the WBS 8.9 canonical order (stable and
// deterministic). Read-only: write methods on the path are a 405 via the
// mux.
package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
)

// promptProvenanceEntryDTO is the stable JSON view of one provenance entry
// (WBS 8.37: source identity, captured version, captured hash).
type promptProvenanceEntryDTO struct {
	ComponentType string `json:"component_type"`
	SourceID      string `json:"source_id"`
	SourceVersion int    `json:"source_version"`
	SourceHash    string `json:"source_hash"`
}

// promptProvenanceResource is the stable JSON view of one prompt's
// provenance (WBS 8.37).
type promptProvenanceResource struct {
	PromptID string                     `json:"prompt_id"`
	StateKey string                     `json:"state_key"`
	Entries  []promptProvenanceEntryDTO `json:"entries"`
	Count    int                        `json:"count"`
}

type promptProvenanceAPI struct {
	svc *service.PromptProvenanceService
}

// RegisterPromptProvenanceRoutes exposes the prompt-provenance endpoint
// (WBS 8.37).
func RegisterPromptProvenanceRoutes(r *Router, svc *service.PromptProvenanceService) error {
	api := &promptProvenanceAPI{svc: svc}
	return r.RegisterAPI(http.MethodGet, "/api/v1/projects/{project}/prompts/{prompt}/provenance", api.handle)
}

// handle — T8.37.1-T8.37.4: the prompt's source set, pinned versions/hashes,
// deterministic order, project-scoped (a foreign id is a 404).
func (a *promptProvenanceAPI) handle(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.Get(r.Context(), r.PathValue("project"), r.PathValue("prompt"))
	if err != nil {
		WriteError(w, r, provenanceServiceError(err))
		return
	}
	res := promptProvenanceResource{
		PromptID: out.PromptID.String(),
		StateKey: out.StateKey,
		Entries:  make([]promptProvenanceEntryDTO, 0, len(out.Entries)),
		Count:    len(out.Entries),
	}
	for _, e := range out.Entries {
		res.Entries = append(res.Entries, promptProvenanceEntryDTO{
			ComponentType: string(e.ComponentType),
			SourceID:      e.SourceID.String(),
			SourceVersion: e.SourceVersion,
			SourceHash:    e.SourceHash,
		})
	}
	writeJSON(w, http.StatusOK, res)
}

func provenanceServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_prompt_provenance", "Prompt provenance request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("canonical prompt")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("prompt provenance conflict")
	default:
		return NewInternalError("prompt provenance read failed")
	}
}
