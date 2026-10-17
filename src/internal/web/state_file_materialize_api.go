// P7-22 (WBS 7.39): the materialization REST API. POST
// /state-files/materialize writes the project's authoritative persisted
// state-file content into the project's working directory and
// GET /state-files/drift reports per-file drift status. Both reuse the
// P7-14/P7-16 materialization/drift services — no parallel drift
// computation path. The destination is the project's CONFIGURED working
// directory (server-resolved from project settings): a client-supplied
// path target in the POST body is rejected by the strict decode (the
// endpoint accepts no fields), and a path-like query parameter is ignored
// (T7.39.3). Writes stay inside the project directory by construction —
// the service only writes into <working-dir>/.overseer/state/
// (T7.39.4).
package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"overseer/internal/service"
)

// stateFileMaterializedEntry is one row of the materialize response: the
// file id, the path it was written to ("" when skipped), the written
// version, the written content hash, and the skip reason.
type stateFileMaterializedEntry struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Version int    `json:"version"`
	Hash    string `json:"hash"`
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
}

// stateFileMaterializeResponse wraps the materialize response; the files
// array is never null (an empty project materializes nothing).
type stateFileMaterializeResponse struct {
	WorkingDirectory string                       `json:"working_directory"`
	Files            []stateFileMaterializedEntry `json:"files"`
}

// stateFileDriftEntry is one row of the drift response: the file id and
// its drift status against the persisted authoritative version.
type stateFileDriftEntry struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int    `json:"version"`
	Hash    string `json:"hash,omitempty"`
	Path    string `json:"path,omitempty"`
}

// stateFileDriftResponse wraps the drift response; files is never null.
type stateFileDriftResponse struct {
	WorkingDirectory string                `json:"working_directory"`
	Files            []stateFileDriftEntry `json:"files"`
}

type stateFileMaterializeAPI struct {
	mat   *service.StateFileMaterializeService
	files *service.StateFileService
	set   *service.SettingsService
}

// RegisterStateFileMaterializeRoutes exposes the project-scoped
// materialization endpoints (WBS 7.39):
//
//	POST /api/v1/projects/{project}/state-files/materialize
//	GET  /api/v1/projects/{project}/state-files/drift
func RegisterStateFileMaterializeRoutes(r *Router, mat *service.StateFileMaterializeService, files *service.StateFileService, set *service.SettingsService) error {
	api := &stateFileMaterializeAPI{mat: mat, files: files, set: set}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodPost, "/api/v1/projects/{project}/state-files/materialize", api.handleMaterialize},
		{http.MethodGet, "/api/v1/projects/{project}/state-files/drift", api.handleDrift},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// workingDirectory resolves the project's configured working directory —
// the ONLY server-resolved destination for materialization. It returns
// (path, err): an unknown/malformed project id is a service error
// (404), a project without settings is also a service error (404 via the
// settings repository's not-found translation).
func (a *stateFileMaterializeAPI) workingDirectory(ctx context.Context, projectID string) (string, error) {
	// A malformed project path segment is a 404, not a 400 — the
	// established project-scoped read contract (an unresolvable project
	// resource is not found).
	if _, err := uuid.Parse(projectID); err != nil {
		return "", NewNotFoundError("project")
	}
	st, err := a.set.GetSettings(ctx, projectID)
	if err != nil {
		return "", err
	}
	return st.WorkingDirectory, nil
}

// handleMaterialize — T7.39.1/T7.39.3/T7.39.4: writes the project's
// authoritative state-file content into <working-dir>/.overseer/state/.
// The strict decode accepts NO fields — any client-supplied body member
// (a path target, a destination, ...) is an unknown field and a 400.
func (a *stateFileMaterializeAPI) handleMaterialize(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	// The endpoint takes no request fields at all (T7.39.3): the
	// destination is server-resolved. An empty object is the only valid
	// body.
	if err := decodeStrictJSON(r, "invalid_materialize_request", map[string]bool{}, nil, &struct{}{}); err != nil {
		WriteError(w, r, err)
		return
	}
	dir, err := a.workingDirectory(r.Context(), project)
	if err != nil {
		WriteError(w, r, matServiceError(err))
		return
	}
	results, err := a.mat.Materialize(r.Context(), dir, project)
	if err != nil {
		WriteError(w, r, matServiceError(err))
		return
	}
	out := stateFileMaterializeResponse{WorkingDirectory: dir, Files: make([]stateFileMaterializedEntry, 0, len(results))}
	for _, res := range results {
		entry := stateFileMaterializedEntry{ID: res.StateFileID, Path: res.Written, Skipped: res.Skipped != "", Reason: res.Skipped}
		if res.Skipped == "" {
			// The response identifies what was written: the WRITTEN
			// version and the content hash of the written bytes (the
			// persisted current version — the write source).
			v, err := a.files.GetCurrentVersion(r.Context(), res.StateFileID)
			if err != nil {
				WriteError(w, r, matServiceError(err))
				return
			}
			if v != nil {
				entry.Version = v.Version
				entry.Hash = v.ContentHash
			}
		}
		out.Files = append(out.Files, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDrift — T7.39.2: the per-file drift status of the project's
// state files against the persisted authoritative versions. Read-only:
// write methods on this path are rejected by the mux (405, plain-text
// body). Per-file drift is delegated to the P7-16 DetectDrift — the
// persisted current version is the comparison source, never the metadata
// record.
func (a *stateFileMaterializeAPI) handleDrift(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	dir, err := a.workingDirectory(r.Context(), project)
	if err != nil {
		WriteError(w, r, matServiceError(err))
		return
	}
	entries, err := a.files.ListProjectFiles(r.Context(), project)
	if err != nil {
		WriteError(w, r, matServiceError(err))
		return
	}
	out := stateFileDriftResponse{WorkingDirectory: dir, Files: make([]stateFileDriftEntry, 0, len(entries))}
	for _, e := range entries {
		res, err := a.mat.DetectDrift(r.Context(), dir, project, e.ID)
		if err != nil {
			WriteError(w, r, matServiceError(err))
			return
		}
		entry := stateFileDriftEntry{ID: e.ID, Status: string(res.Status), Version: res.CurrentVersion, Hash: res.ContentHash, Path: res.Path}
		out.Files = append(out.Files, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// matServiceError maps the materialization surface's errors: the service
// sentinels (the same mapping as the state-file CRUD surface) plus the
// web-layer not-found the malformed project segment produces.
func matServiceError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return NewNotFoundError("project")
	}
	return stateFileServiceError(err)
}
