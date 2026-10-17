// P7-21 (WBS 7.38): the state-file CRUD REST surface. List the project's
// state files, read the current content of one, the managed content update
// (through the P7-08 UpdateContent path), and the version-history reads.
// Derived files are read-only through the PUT (the P7-08 rejection
// contract). Project-scoped; stable JSON DTOs, strict decode on the PUT,
// the standard serviceError mapping.
package web

import (
	"errors"
	"net/http"
	"strconv"

	"overseer/internal/service"
)

// stateFileListEntry is the stable JSON view of one state file on the
// project list (WBS 7.38): id, type key, scope, association, current
// version, and the type's source classification.
type stateFileListEntry struct {
	ID             string  `json:"id"`
	TypeID         string  `json:"type_id"`
	TypeKey        string  `json:"type_key"`
	Source         string  `json:"source"`
	Scope          string  `json:"scope"`
	PhaseID        *string `json:"phase_id"`
	WorkUnitID     *string `json:"work_unit_id"`
	CurrentVersion int     `json:"current_version"`
}

// stateFileVersionSection is the current/historical version section:
// version number, exact content, deterministic content hash.
type stateFileVersionSection struct {
	Version int    `json:"version"`
	Content string `json:"content"`
	Hash    string `json:"hash"`
	// Size (bytes) and TokenEstimate are backend metadata (WBS 17.43).
	Size          int `json:"size"`
	TokenEstimate int `json:"token_estimate"`
}

// stateFileCurrent is the GET-file response: identity + source + the
// current version section (null when the file has no content).
type stateFileCurrent struct {
	ID      string                   `json:"id"`
	Source  string                   `json:"source"`
	Version *stateFileVersionSection `json:"version"`
}

// stateFileContentPutRequest is the WBS 7.38 PUT payload: the new content
// plus the optional expected-version the client edited against (absent = 0).
type stateFileContentPutRequest struct {
	Content         string `json:"content"`
	ExpectedVersion *int   `json:"expected_version"`
}

var allowedStateFileContentFields = map[string]bool{"content": true, "expected_version": true}

// stateFileVersionSummary is one row of the version-history list (version
// number + deterministic hash).
type stateFileVersionSummary struct {
	Version int    `json:"version"`
	Hash    string `json:"hash"`
}

// stateFileVersionList wraps the history-list response; an empty history is
// an empty array, never null.
type stateFileVersionList struct {
	Versions []stateFileVersionSummary `json:"versions"`
}

// stateFileServiceError maps the P7-08 state-file service sentinels onto the
// standard HTTP responses (400 / 404 / 409 / 500).
func stateFileServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_state_file", "State-file request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("state file")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("state-file version conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("state-file operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

func toStateFileVersionSection(v *service.StateFileVersionView) *stateFileVersionSection {
	if v == nil {
		return nil
	}
	return &stateFileVersionSection{Version: v.Version, Content: v.Content, Hash: v.ContentHash, Size: len(v.Content), TokenEstimate: v.TokenEstimate}
}

type stateFileCrudAPI struct {
	svc *service.StateFileService
}

// RegisterStateFileCRUDRoutes exposes the project-scoped state-file CRUD
// endpoints (WBS 7.38):
//
//	GET /api/v1/projects/{project}/state-files
//	GET /api/v1/projects/{project}/state-files/{file}
//	PUT /api/v1/projects/{project}/state-files/{file}
//	GET /api/v1/projects/{project}/state-files/{file}/versions
//	GET /api/v1/projects/{project}/state-files/{file}/versions/{version}
func RegisterStateFileCRUDRoutes(r *Router, svc *service.StateFileService) error {
	api := &stateFileCrudAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/state-files", api.handleList},
		{http.MethodGet, "/api/v1/projects/{project}/state-files/{file}", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{project}/state-files/{file}", api.handlePut},
		{http.MethodGet, "/api/v1/projects/{project}/state-files/{file}/versions", api.handleListVersions},
		{http.MethodGet, "/api/v1/projects/{project}/state-files/{file}/versions/{version}", api.handleGetVersion},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// handleList — T7.38.1: list the project's state files with their type key,
// scope, association, current version, and source classification.
func (a *stateFileCrudAPI) handleList(w http.ResponseWriter, r *http.Request) {
	entries, err := a.svc.ListProjectFiles(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, stateFileServiceError(err))
		return
	}
	out := make([]stateFileListEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, stateFileListEntry{
			ID: e.ID, TypeID: e.TypeID, TypeKey: e.TypeKey, Source: e.Source,
			Scope: e.Scope, PhaseID: e.PhaseID, WorkUnitID: e.WorkUnitID,
			CurrentVersion: e.CurrentVersion,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// handleGet — T7.38.2: the current content of one file (content, version,
// hash) plus its source classification.
func (a *stateFileCrudAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	project, file := r.PathValue("project"), r.PathValue("file")
	rec, typeKey, source, err := a.svc.GetFile(r.Context(), project, file)
	if err != nil {
		WriteError(w, r, stateFileServiceError(err))
		return
	}
	var cur *service.StateFileVersionView
	if rec.CurrentVersion > 0 {
		cur, err = a.svc.GetCurrentVersion(r.Context(), file)
		if err != nil {
			WriteError(w, r, stateFileServiceError(err))
			return
		}
	}
	_ = typeKey
	writeJSON(w, http.StatusOK, stateFileCurrent{
		ID: rec.ID.String(), Source: source, Version: toStateFileVersionSection(cur),
	})
}

// handlePut — T7.38.3/T7.38.5: the managed content update through the
// P7-08 UpdateContent path (expected-version guard; DERIVED direct edit
// rejected by the P7-08 contract before any write).
func (a *stateFileCrudAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req stateFileContentPutRequest
	if err := decodeStrictJSON(r, "invalid_state_file", allowedStateFileContentFields,
		[]string{"content"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	expected := 0
	if req.ExpectedVersion != nil {
		expected = *req.ExpectedVersion
	}
	v, err := a.svc.UpdateContent(r.Context(), r.PathValue("file"), req.Content, expected)
	if err != nil {
		WriteError(w, r, stateFileServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("file"), "version": toStateFileVersionSection(v)})
}

// handleListVersions — T7.38.4: the file's version history (version numbers
// + hashes).
func (a *stateFileCrudAPI) handleListVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := a.svc.ListFileVersions(r.Context(), r.PathValue("project"), r.PathValue("file"))
	if err != nil {
		WriteError(w, r, stateFileServiceError(err))
		return
	}
	out := stateFileVersionList{Versions: make([]stateFileVersionSummary, 0, len(versions))}
	for _, v := range versions {
		out.Versions = append(out.Versions, stateFileVersionSummary{Version: v.Version, Hash: v.ContentHash})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetVersion — T7.38.4: one historical version (version, content,
// hash); a missing version number is 404.
func (a *stateFileCrudAPI) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		WriteError(w, r, NewValidationError("version must be a positive integer"))
		return
	}
	v, err := a.svc.GetFileVersion(r.Context(), r.PathValue("project"), r.PathValue("file"), version)
	if err != nil {
		WriteError(w, r, stateFileServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": v.Version, "content": v.Content, "hash": v.ContentHash,
	})
}
