// P7-23 (WBS 7.37/7.40): the context-preview + snapshot-read REST surface.
//
// GET /api/v1/projects/{project}/context-preview reports what context WOULD
// be captured (current state, role, required files with their current
// versions, the missing required files, and the estimated token count)
// WITHOUT persisting a snapshot (WBS 7.37).
// GET /api/v1/projects/{project}/context-snapshots lists the persisted
// historical snapshots and GET .../context-snapshots/{snapshot} retrieves
// one (WBS 7.40) — a read-only surface (no mutation endpoint).
package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"overseer/internal/persistence"
	"overseer/internal/role"
	"overseer/internal/service"
)

// contextPreviewFileEntry is one required state file the preview would
// capture: its type key, scope, and — when present — its current version,
// content hash and approximate token estimate (WBS 7.37).
type contextPreviewFileEntry struct {
	TypeKey       string `json:"type_key"`
	Scope         string `json:"scope"`
	Present       bool   `json:"present"`
	Version       int    `json:"version"`
	Hash          string `json:"hash"`
	TokenEstimate int    `json:"token_estimate"`
}

// contextPreviewResource is the stable JSON view of the context preview
// (WBS 7.37): the current state, the resolved role, the required files with
// their current versions, the missing required files, and the estimated
// total context token count.
type contextPreviewResource struct {
	State           string                    `json:"state"`
	Role            string                    `json:"role"`
	RequiredFiles   []contextPreviewFileEntry `json:"required_files"`
	MissingFiles    []string                  `json:"missing_files"`
	EstimatedTokens int                       `json:"estimated_tokens"`
}

// snapshotRoleSection is the role section of a persisted-snapshot read (WBS
// 7.40): the resolved role's key + version and its system-prompt version —
// the "historical versions represented" surface.
type snapshotRoleSection struct {
	Key             string `json:"key"`
	Version         int    `json:"version"`
	SystemPromptVer int    `json:"system_prompt_version"`
}

// snapshotFileEntry is one state-file pin in a persisted snapshot: the exact
// version, hash, classification and token estimate captured (WBS 7.30/7.35).
type snapshotFileEntry struct {
	TypeKey        string `json:"type_key"`
	Scope          string `json:"scope"`
	Version        int    `json:"version"`
	Hash           string `json:"hash"`
	TokenEstimate  int    `json:"token_estimate"`
	Classification string `json:"classification"`
}

// contextSnapshotEntry is the stable JSON view of one persisted snapshot
// (WBS 7.40): its position fields (state key, runtime + machine versions),
// its role, its state-file pins, and its budget + creation time.
type contextSnapshotEntry struct {
	ID                  string               `json:"id"`
	StateKey            string               `json:"state_key"`
	RuntimeStateVersion int                  `json:"runtime_state_version"`
	StateMachineVersion int                  `json:"state_machine_version"`
	Role                *snapshotRoleSection `json:"role"`
	Files               []snapshotFileEntry  `json:"files"`
	EstimatedTotal      int                  `json:"estimated_total"`
	CreatedAt           string               `json:"created_at"`
}

// contextSnapshotList wraps the list response; an empty history is an empty
// array, never null.
type contextSnapshotList struct {
	Snapshots []contextSnapshotEntry `json:"snapshots"`
}

func contextSnapshotServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_context_snapshot", "Context-snapshot request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("context snapshot")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("context snapshot conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("context snapshot operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

// toContextSnapshotEntry maps a persisted snapshot record to its stable JSON
// view, decoding the stored role snapshot into the role section.
func toContextSnapshotEntry(rec *persistence.ContextSnapshotRecord) contextSnapshotEntry {
	out := contextSnapshotEntry{
		ID:                  rec.ID.String(),
		StateKey:            rec.StateKey,
		RuntimeStateVersion: rec.RuntimeStateVersion,
		StateMachineVersion: rec.StateMachineVersion,
		Files:               make([]snapshotFileEntry, 0, len(rec.Files)),
		EstimatedTotal:      rec.EstimatedTotal,
		CreatedAt:           rec.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if len(rec.RoleSnapshotJSON) > 0 {
		var rs role.RoleSnapshot
		if err := json.Unmarshal(rec.RoleSnapshotJSON, &rs); err == nil {
			out.Role = &snapshotRoleSection{
				Key:             rs.RoleKey,
				Version:         rs.RoleVersion,
				SystemPromptVer: rs.SystemPrompt.Version,
			}
		}
	}
	for _, f := range rec.Files {
		out.Files = append(out.Files, snapshotFileEntry{
			TypeKey: f.TypeKey, Scope: f.Scope, Version: f.Version,
			Hash: f.Hash, TokenEstimate: f.TokenEstimate, Classification: f.Classification,
		})
	}
	return out
}

type contextSnapshotAPI struct {
	preview *service.ContextPreviewService
}

// RegisterContextSnapshotRoutes exposes the context-preview + snapshot-read
// endpoints (WBS 7.37/7.40):
//
//	GET /api/v1/projects/{project}/context-preview
//	GET /api/v1/projects/{project}/context-snapshots
//	GET /api/v1/projects/{project}/context-snapshots/{snapshot}
//
// The read-only surface registers no write methods: clients cannot mutate
// snapshots (T7.40.4) — the mux 405s any write on the snapshot paths.
func RegisterContextSnapshotRoutes(r *Router, svc *service.ContextPreviewService) error {
	api := &contextSnapshotAPI{preview: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/context-preview", api.handlePreview},
		{http.MethodGet, "/api/v1/projects/{project}/context-snapshots", api.handleList},
		{http.MethodGet, "/api/v1/projects/{project}/context-snapshots/{snapshot}", api.handleGet},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// handlePreview — T7.37.1-T7.37.3: the correct requirements, their current
// versions, the missing required files, and the estimated token count are
// returned; T7.37.4/T7.37.5 are guaranteed by the read-only service op (no
// snapshot persisted, no runtime write).
func (a *contextSnapshotAPI) handlePreview(w http.ResponseWriter, r *http.Request) {
	v, err := a.preview.Preview(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, contextSnapshotServiceError(err))
		return
	}
	files := make([]contextPreviewFileEntry, 0, len(v.Files))
	for _, f := range v.Files {
		files = append(files, contextPreviewFileEntry{
			TypeKey: f.TypeKey, Scope: string(f.Scope),
			Present: f.Present, Version: f.Version,
			Hash: f.Hash, TokenEstimate: f.TokenEstimate,
		})
	}
	missing := v.MissingFiles
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, contextPreviewResource{
		State: v.StateKey, Role: v.RoleKey,
		RequiredFiles: files, MissingFiles: missing,
		EstimatedTokens: v.EstimatedTokens,
	})
}

// handleList — T7.40: list the project's persisted snapshots (oldest first).
func (a *contextSnapshotAPI) handleList(w http.ResponseWriter, r *http.Request) {
	recs, err := a.preview.ListSnapshots(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, contextSnapshotServiceError(err))
		return
	}
	entries := make([]contextSnapshotEntry, 0, len(recs))
	for _, rec := range recs {
		entries = append(entries, toContextSnapshotEntry(rec))
	}
	writeJSON(w, http.StatusOK, contextSnapshotList{Snapshots: entries})
}

// handleGet — T7.40.1/T7.40.3: retrieve one persisted snapshot by id; an
// unknown (or cross-project) snapshot is 404.
func (a *contextSnapshotAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	rec, err := a.preview.GetSnapshot(r.Context(), r.PathValue("project"), r.PathValue("snapshot"))
	if err != nil {
		WriteError(w, r, contextSnapshotServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toContextSnapshotEntry(rec))
}
