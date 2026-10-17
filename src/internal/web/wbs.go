package web

import (
	"errors"
	"net/http"
	"strconv"

	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// wbsResource is the stable JSON representation of a phase WBS document
// (WBS 3.18), deliberately separate from the domain record.
type wbsResource struct {
	PhaseID string `json:"phase_id"`
	Version int    `json:"version"`
	Content string `json:"content"`
}

func toWBSResource(d *persistence.WBSDocumentRecord) wbsResource {
	return wbsResource{PhaseID: d.PhaseID, Version: d.Version, Content: d.Content}
}

// wbsDiffHunk is the stable JSON representation of one P4-23 diff hunk;
// from_lines/to_lines are arrays (empty, never null, for the absent side).
type wbsDiffHunk struct {
	Operation string   `json:"operation"`
	FromLines []string `json:"from_lines"`
	ToLines   []string `json:"to_lines"`
}

// wbsDiffResponse is the stable JSON response of the WBS diff endpoint
// (WBS 4.17): the requested version pair plus the deterministic hunks.
type wbsDiffResponse struct {
	From  int           `json:"from"`
	To    int           `json:"to"`
	Hunks []wbsDiffHunk `json:"hunks"`
}

func toWBSDiffResponse(from, to int, d wbs.Diff) wbsDiffResponse {
	out := wbsDiffResponse{From: from, To: to, Hunks: make([]wbsDiffHunk, 0, len(d.Hunks))}
	for _, h := range d.Hunks {
		fh := wbsDiffHunk{
			Operation: string(h.Operation),
			FromLines: h.FromLines,
			ToLines:   h.ToLines,
		}
		if fh.FromLines == nil {
			fh.FromLines = []string{}
		}
		if fh.ToLines == nil {
			fh.ToLines = []string{}
		}
		out.Hunks = append(out.Hunks, fh)
	}
	return out
}

// allowedWBSFields is the closed field set accepted on WBS mutation
// payloads (WBS 3.18 + 4.12: content plus the optional expected-version
// declaration; phase_id/version are server-owned).
var allowedWBSFields = map[string]bool{"content": true, "expected_version": true}

type wbsPutRequest struct {
	Content string `json:"content"`
	// ExpectedVersion is the version the client edited against (WBS 4.12);
	// absent = undeclared (legacy, unchecked), present and stale = 409.
	ExpectedVersion *int `json:"expected_version"`
}

// wbsServiceError maps service-level errors onto web error responses for
// the WBS surface (400 / 404 / 409 / 500).
func wbsServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_wbs", "WBS document is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("WBS document")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("WBS version conflict or protected phase")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("WBS operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type wbsAPI struct {
	svc *service.WBSDocumentService
}

// RegisterWBSRoutes exposes the WBS REST endpoints (WBS 3.18):
// GET/PUT /api/v1/projects/{id}/phases/{phaseID}/wbs.
func RegisterWBSRoutes(r *Router, svc *service.WBSDocumentService) error {
	api := &wbsAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{id}/phases/{phaseID}/wbs", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{id}/phases/{phaseID}/wbs", api.handlePut},
		// WBS history surface (WBS 4.16): stored version list + one version.
		{http.MethodGet, "/api/v1/projects/{id}/phases/{phaseID}/wbs/versions", api.handleListVersions},
		{http.MethodGet, "/api/v1/projects/{id}/phases/{phaseID}/wbs/versions/{version}", api.handleGetVersion},
		// WBS diff (WBS 4.17 endpoint half): deterministic diff of two stored versions.
		{http.MethodGet, "/api/v1/projects/{id}/phases/{phaseID}/wbs/diff", api.handleDiff},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func (a *wbsAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	doc, err := a.svc.GetWBS(r.Context(), r.PathValue("id"), r.PathValue("phaseID"))
	if err != nil {
		WriteError(w, r, wbsServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toWBSResource(doc))
}

func (a *wbsAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req wbsPutRequest
	if err := decodeStrictJSON(r, "invalid_wbs", allowedWBSFields, nil, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	expected := 0
	if req.ExpectedVersion != nil {
		expected = *req.ExpectedVersion
	}
	doc, err := a.svc.UpdateWBS(r.Context(), r.PathValue("id"), r.PathValue("phaseID"), req.Content, expected)
	if err != nil {
		WriteError(w, r, wbsServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toWBSResource(doc))
}

// handleDiff returns the deterministic diff between two stored WBS versions
// (WBS 4.17 endpoint half). Malformed/missing/non-positive from/to are 400;
// unknown versions, cross-project/unknown phase, and unknown project are
// 404 via the service's scoped-phase and version contract.
func (a *wbsAPI) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := strconv.Atoi(q.Get("from"))
	if err != nil || from < 1 {
		WriteError(w, r, NewValidationCode("invalid_wbs", "from and to must be positive integers."))
		return
	}
	to, err := strconv.Atoi(q.Get("to"))
	if err != nil || to < 1 {
		WriteError(w, r, NewValidationCode("invalid_wbs", "from and to must be positive integers."))
		return
	}
	diff, err := a.svc.DiffWBS(r.Context(), r.PathValue("id"), r.PathValue("phaseID"), from, to)
	if err != nil {
		WriteError(w, r, wbsServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toWBSDiffResponse(from, to, diff))
}
