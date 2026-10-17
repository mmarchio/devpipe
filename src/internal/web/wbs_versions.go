package web

import (
	"net/http"
	"strconv"

	"overseer/internal/persistence"
)

// wbsVersionResource is the stable JSON representation of one stored WBS
// version of the history surface (WBS 4.16): version number, deterministic
// content hash, the exact stored content, and the storage timestamp.
type wbsVersionResource struct {
	Version     int    `json:"version"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	// ContentOmitted marks a list entry whose content exceeded the list's
	// content budget (WBS 18.96): fetch /wbs/versions/{version}.
	ContentOmitted bool `json:"content_omitted,omitempty"`
}

// wbsListContentBudget bounds the inline content of a version list
// (WBS 18.96 / 18.165): newest versions first get their content; the rest
// are listed with hash and number only.
const wbsListContentBudget = 256 << 10

func toWBSVersionResource(r *persistence.WBSVersionRecord) wbsVersionResource {
	return wbsVersionResource{
		Version:     r.Version,
		ContentHash: r.ContentHash,
		Content:     r.Content,
		CreatedAt:   r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

// wbsVersionList wraps the history list response (WBS 4.16); an empty
// history is an empty array, never null.
type wbsVersionList struct {
	Versions []wbsVersionResource `json:"versions"`
}

// handleListVersions returns the phase's stored WBS versions in ascending
// order (WBS 4.16). Cross-project/unknown phase and unknown project are
// 404 via the service's scoped-phase contract.
func (a *wbsAPI) handleListVersions(w http.ResponseWriter, r *http.Request) {
	recs, err := a.svc.ListWBSVersionHistory(r.Context(), r.PathValue("id"), r.PathValue("phaseID"))
	if err != nil {
		WriteError(w, r, wbsServiceError(err))
		return
	}
	out := wbsVersionList{Versions: make([]wbsVersionResource, len(recs))}
	budget := wbsListContentBudget
	for i := len(recs) - 1; i >= 0; i-- { // newest first keep their content
		v := toWBSVersionResource(recs[i])
		if len(v.Content) > budget {
			v.Content, v.ContentOmitted = "", true
		} else {
			budget -= len(v.Content)
		}
		out.Versions[i] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetVersion returns one stored WBS version by number (WBS 4.16);
// unknown versions are 404 (service ErrNotFound).
func (a *wbsAPI) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		WriteError(w, r, NewValidationError("version must be a positive integer"))
		return
	}
	rec, err := a.svc.GetWBSVersionHistory(r.Context(), r.PathValue("id"), r.PathValue("phaseID"), version)
	if err != nil {
		WriteError(w, r, wbsServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toWBSVersionResource(rec))
}
