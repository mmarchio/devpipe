package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/statefile"
)

// P7-21 (WBS 7.38): the state-file CRUD REST API — list, current content,
// managed content update (through the P7-08 UpdateContent path), version
// history reads. Derived files are read-only through the PUT
// (T7.38.5, the P7-08 rejection contract is the source of truth).

// p721FilesURL builds the project-scoped state-file REST base.
func p721FilesURL(h *Harness, projectID string) string {
	return h.BaseURL + "/api/v1/projects/" + projectID + "/state-files"
}

func p721FileURL(h *Harness, projectID, fileID string) string {
	return p721FilesURL(h, projectID) + "/" + fileID
}

// T7.38.1 — Files listed: the project's state files are listed with their
// identity, type key, scope, association, current version, and the type's
// source classification.
func TestP721FilesListed(t *testing.T) {
	h := NewHarness(t)
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, managedFile := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)
	_, derivedFile := p708TypeAndFile(t, h, svc, projectID, "active-task", statefile.SourceDerived)

	code, got := httpJSON(t, http.MethodGet, p721FilesURL(h, projectID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %v", code, got)
	}
	list, _ := got["files"].([]any)
	if len(list) != 2 {
		t.Fatalf("listed files = %d, want 2: %v", len(list), got)
	}
	byID := map[string]map[string]any{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		byID[m["id"].(string)] = m
	}
	managed, ok := byID[managedFile.String()]
	if !ok {
		t.Fatalf("managed file %s missing from list: %v", managedFile, list)
	}
	if managed["type_key"] != "PROJECT-STATE" || managed["scope"] != "PROJECT" ||
		managed["source"] != "MANAGED" || managed["current_version"].(float64) != 0 {
		t.Fatalf("managed listing = %v", managed)
	}
	derived, ok := byID[derivedFile.String()]
	if !ok {
		t.Fatalf("derived file %s missing from list: %v", derivedFile, list)
	}
	if derived["type_key"] != "ACTIVE-TASK" || derived["source"] != "DERIVED" {
		t.Fatalf("derived listing = %v", derived)
	}
}

// T7.38.2 — Current content retrieved: GET returns the file's current
// content, version, hash, and source classification.
func TestP721CurrentContentRetrieved(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)
	if _, err := svc.UpdateContent(ctx, fileID.String(), "v1-content", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := svc.UpdateContent(ctx, fileID.String(), "v2-content", 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}

	code, got := httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), fileID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("get current = %d, want 200: %v", code, got)
	}
	if got["id"] != fileID.String() || got["source"] != "MANAGED" {
		t.Fatalf("current identity = %v", got)
	}
	sec, _ := got["version"].(map[string]any)
	if sec == nil || sec["version"].(float64) != 2 || sec["content"] != "v2-content" || sec["hash"] != v2.ContentHash {
		t.Fatalf("current version section = %v", got["version"])
	}
}

// T7.38.3 — Managed content updated: the PUT goes through the P7-08
// UpdateContent path; a MANAGED update succeeds and appends a version, a
// stale expected_version is a conflict, an unknown file is 404.
func TestP721ManagedContentUpdated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	code, got := httpJSON(t, http.MethodPut, p721FileURL(h, projectID.String(), fileID.String()),
		map[string]any{"content": "first managed edit"})
	if code != http.StatusOK {
		t.Fatalf("managed put = %d, want 200: %v", code, got)
	}
	if got["version"] == nil || got["version"].((map[string]any))["version"].(float64) != 1 {
		t.Fatalf("update response = %v", got)
	}
	var cur int
	if err := h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan pointer: %v", err)
	}
	if cur != 1 {
		t.Fatalf("current_version = %d, want 1", cur)
	}
	// Stale expected_version -> conflict (the P7-08 guard).
	code, got = httpJSON(t, http.MethodPut, p721FileURL(h, projectID.String(), fileID.String()),
		map[string]any{"content": "stale edit", "expected_version": 0})
	if code != http.StatusConflict {
		t.Fatalf("stale put = %d, want 409: %v", code, got)
	}
	// Unknown file -> 404.
	code, _ = httpJSON(t, http.MethodPut, p721FileURL(h, projectID.String(), "00000000-0000-0000-0000-0000000000ff"),
		map[string]any{"content": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown-file put = %d, want 404", code)
	}
}

// T7.38.4 — Historical version retrieved: the version history lists version
// numbers + hashes, and one historical version returns its exact content.
func TestP721HistoricalVersionRetrieved(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)
	if _, err := svc.UpdateContent(ctx, fileID.String(), "v1-content", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := svc.UpdateContent(ctx, fileID.String(), "v2-content", 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}

	code, got := httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), fileID.String())+"/versions", nil)
	if code != http.StatusOK {
		t.Fatalf("list versions = %d, want 200: %v", code, got)
	}
	versions, _ := got["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2: %v", len(versions), got)
	}
	v1entry, _ := versions[0].(map[string]any)
	v2entry, _ := versions[1].(map[string]any)
	if v1entry["version"].(float64) != 1 || v2entry["version"].(float64) != 2 {
		t.Fatalf("version numbers = %v / %v", v1entry, v2entry)
	}
	if v2entry["hash"] != v2.ContentHash {
		t.Fatalf("v2 hash = %v, want %s", v2entry["hash"], v2.ContentHash)
	}

	code, got = httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), fileID.String())+"/versions/1", nil)
	if code != http.StatusOK {
		t.Fatalf("get v1 = %d, want 200: %v", code, got)
	}
	if got["version"] != float64(1) || got["content"] != "v1-content" {
		t.Fatalf("historical v1 = %v", got)
	}
	// A version number that was never stored -> 404.
	code, _ = httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), fileID.String())+"/versions/9", nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing version = %d, want 404", code)
	}
}

// T7.38.5 — Derived direct edit rejected: a DERIVED file rejects the
// ordinary content-update path (the P7-08 contract) and nothing is written.
func TestP721DerivedDirectEditRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "active-task", statefile.SourceDerived)

	code, got := httpJSON(t, http.MethodPut, p721FileURL(h, projectID.String(), fileID.String()),
		map[string]any{"content": "hand-edited derived"})
	if code != http.StatusBadRequest {
		t.Fatalf("derived put = %d, want 400: %v", code, got)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 0 {
		t.Fatalf("version rows after refused derived edit = %d, want 0", n)
	}
}

// Unregistered project-scoped paths 404, and the write surface rejects
// unknown body fields (strict decode).
func TestP721RESTContractEdges(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _ := p705Rig(t, h)

	// Unknown project on the list endpoint -> 404.
	code, _ := httpJSON(t, http.MethodGet, p721FilesURL(h, "00000000-0000-0000-0000-0000000000ff"), nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown project list = %d, want 404", code)
	}
	// Unknown project on a file-scoped GET -> 404.
	code, _ = httpJSON(t, http.MethodGet, p721FileURL(h, "00000000-0000-0000-0000-0000000000ff", "00000000-0000-0000-0000-0000000000ff"), nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown project file get = %d, want 404", code)
	}
	// Non-uuid file path -> 404 (invalid id).
	code, _ = httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), "not-a-uuid"), nil)
	if code != http.StatusNotFound {
		t.Fatalf("bad file id get = %d, want 404", code)
	}
	// A file in ANOTHER project is 404 from this project's URL (project
	// scoping is load-bearing, not just row existence).
	h2 := NewHarness(t)
	svc2 := p708Service(t, h2)
	projectB, _, _ := p705Rig(t, h2)
	_, fileB := p708TypeAndFile(t, h2, svc2, projectB, "project-state", statefile.SourceManaged)
	code, _ = httpJSON(t, http.MethodGet, p721FileURL(h, projectID.String(), fileB.String()), nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-project file get = %d, want 404", code)
	}
}
