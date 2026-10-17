// P8-41 (WBS 8.36): the prompt history REST surface.
//
// Two read-only endpoints for debugging persisted canonical prompts:
//
//	GET /api/v1/projects/{project}/prompts[?phase&work_unit&state&role&template]
//	GET /api/v1/projects/{project}/prompts/{prompt}
//
// The list supports conjunctive query-string filters; the by-id endpoint is
// scoped to the project (a foreign id is a 404 — T8.36.4).
package web

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"overseer/internal/service"
)

// promptHistoryItem is the stable JSON view of one persisted canonical prompt
// (WBS 8.36 metadata: every provenance field + the exact content + hash +
// estimated tokens).
type promptHistoryItem struct {
	ID                  string  `json:"id"`
	PhaseID             *string `json:"phase_id"`
	WorkUnitID          *string `json:"work_unit_id"`
	ContextSnapshotID   string  `json:"context_snapshot_id"`
	TemplateID          string  `json:"template_id"`
	TemplateVersion     int     `json:"template_version"`
	RoleID              string  `json:"role_id"`
	RoleVersion         int     `json:"role_version"`
	StateMachineID      string  `json:"state_machine_id"`
	StateMachineVersion int     `json:"state_machine_version"`
	StateKey            string  `json:"state_key"`
	Content             string  `json:"content"`
	Hash                string  `json:"hash"`
	EstimatedTokens     int     `json:"estimated_tokens"`
	CreatedAt           string  `json:"created_at"`
}

type promptHistoryListResource struct {
	Prompts []promptHistoryItem `json:"prompts"`
	Count   int                 `json:"count"`
}

func historyServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_prompt_history", "Prompt history request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("canonical prompt")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("prompt history conflict")
	default:
		return NewInternalError("prompt history read failed")
	}
}

type promptHistoryAPI struct {
	svc *service.PromptHistoryService
}

// RegisterPromptHistoryRoutes exposes the prompt-history endpoints (WBS 8.36).
func RegisterPromptHistoryRoutes(r *Router, svc *service.PromptHistoryService) error {
	api := &promptHistoryAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodGet, "/api/v1/projects/{project}/prompts", api.handleList); err != nil {
		return err
	}
	return r.RegisterAPI(http.MethodGet, "/api/v1/projects/{project}/prompts/{prompt}", api.handleGet)
}

// handleList — T8.36.1/T8.36.5: the list with conjunctive filters (unknown
// filter values yield an empty list, not an error; a malformed id in a
// filter param is a 400).
func (a *promptHistoryAPI) handleList(w http.ResponseWriter, r *http.Request) {
	filter, err := parseHistoryFilter(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.List(r.Context(), r.PathValue("project"), filter)
	if err != nil {
		WriteError(w, r, historyServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toHistoryList(out))
}

// handleGet — T8.36.1/T8.36.2/T8.36.4: one prompt by id, project-scoped,
// content exact.
func (a *promptHistoryAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.Get(r.Context(), r.PathValue("project"), r.PathValue("prompt"))
	if err != nil {
		WriteError(w, r, historyServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toHistoryItem(out))
}

// toHistoryItem projects one service record into the wire DTO.
func toHistoryItem(rec *service.PromptHistoryRecord) promptHistoryItem {
	return promptHistoryItem{
		ID:                  rec.ID.String(),
		PhaseID:             uuidPtr(rec.PhaseID),
		WorkUnitID:          uuidPtr(rec.WorkUnitID),
		ContextSnapshotID:   rec.ContextSnapshotID.String(),
		TemplateID:          rec.TemplateID.String(),
		TemplateVersion:     rec.TemplateVersion,
		RoleID:              rec.RoleID.String(),
		RoleVersion:         rec.RoleVersion,
		StateMachineID:      rec.StateMachineID.String(),
		StateMachineVersion: rec.StateMachineVersion,
		StateKey:            rec.StateKey,
		Content:             rec.Content,
		Hash:                rec.Hash,
		EstimatedTokens:     rec.EstimatedTokens,
		CreatedAt:           rec.CreatedAt,
	}
}

func toHistoryList(recs []*service.PromptHistoryRecord) promptHistoryListResource {
	items := make([]promptHistoryItem, 0, len(recs))
	for _, rec := range recs {
		items = append(items, toHistoryItem(rec))
	}
	return promptHistoryListResource{Prompts: items, Count: len(items)}
}

// parseHistoryFilter decodes the conjunctive query-string filters (WBS 8.36).
// Absent params are no-ops; a present-but-malformed id param is a 400 (the
// caller gave a value in a form the API cannot match).
func parseHistoryFilter(r *http.Request) (service.PromptHistoryFilter, error) {
	var f service.PromptHistoryFilter
	q := r.URL.Query()
	if v := q.Get("phase"); v != "" {
		id, err := parseFilterUUID("phase", v)
		if err != nil {
			return f, err
		}
		f.PhaseID = &id
	}
	if v := q.Get("work_unit"); v != "" {
		id, err := parseFilterUUID("work_unit", v)
		if err != nil {
			return f, err
		}
		f.WorkUnitID = &id
	}
	if v := q.Get("state"); v != "" {
		f.StateKey = &v
	}
	if v := q.Get("role"); v != "" {
		id, err := parseFilterUUID("role", v)
		if err != nil {
			return f, err
		}
		f.RoleID = &id
	}
	if v := q.Get("template"); v != "" {
		id, err := parseFilterUUID("template", v)
		if err != nil {
			return f, err
		}
		f.TemplateID = &id
	}
	return f, nil
}

func parseFilterUUID(param, v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.UUID{}, NewValidationFields("invalid_filter", "Filter "+param+" is not a valid id.", map[string]string{param: "must be a UUID"})
	}
	return id, nil
}

// uuidPtr converts a *uuid.UUID to a *string (nil stays nil) for the wire DTO.
func uuidPtr(v *uuid.UUID) *string {
	if v == nil {
		return nil
	}
	s := v.String()
	return &s
}
