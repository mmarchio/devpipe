package integration

import (
	"net/http"
	"testing"
)

// P4-21 WBS History API (WBS 4.16, T4.16.1-T4.16.4):
// GET /api/v1/projects/{project}/phases/{phase}/wbs/versions (ascending
// history) and .../versions/{version} (one stored version). Unknown
// version and cross-project/unknown phase are 404.

func wbsVersionsURL(t *testing.T, h *Harness, projectID, phaseID string) string {
	t.Helper()
	return h.BaseURL + "/api/v1/projects/" + projectID + "/phases/" + phaseID + "/wbs/versions"
}

// wbsVersionSetup creates a project + phase via the API and stores three
// WBS versions through the REST surface.
func wbsVersionSetup(t *testing.T, h *Harness, name string) (string, string) {
	t.Helper()
	projectID, phaseID := wbsSetup(t, h, name)
	contents := []string{"v1 content", "v2 content", "v3 content"}
	for _, c := range contents {
		if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
			wbsPutBody(t, c)); status != http.StatusOK {
			t.Fatalf("PUT WBS %q = %d, want 200", c, status)
		}
	}
	return projectID, phaseID
}

// T4.16.1 — The history endpoint returns all stored versions in ascending
// version order (the documented ordering of the service/repository), each
// carrying its version, content hash, and created_at.
func TestT4161VersionsReturnedAscending(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsVersionSetup(t, h, "p416 list")

	status, out := doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectID, phaseID), "")
	if status != http.StatusOK {
		t.Fatalf("GET versions = %d, want 200", status)
	}
	items, ok := out["versions"].([]any)
	if !ok {
		t.Fatalf("list response has no versions array: %v", out)
	}
	if len(items) != 3 {
		t.Fatalf("listed %d versions, want 3", len(items))
	}
	for i, it := range items {
		row, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("version %d is not an object: %v", i+1, it)
		}
		if row["version"] != float64(i+1) {
			t.Fatalf("position %d has version %v, want %d (ascending order)", i+1, row["version"], i+1)
		}
		hash, _ := row["content_hash"].(string)
		if hash == "" {
			t.Fatalf("version %d has an empty content_hash", i+1)
		}
		createdAt, _ := row["created_at"].(string)
		if createdAt == "" {
			t.Fatalf("version %d has an empty created_at", i+1)
		}
	}
}

// T4.16.2 — A specific historical version is retrievable by version
// number: version, content hash, and the exact stored content of that
// version (not the current one).
func TestT4162SpecificHistoricalVersionRetrievable(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsVersionSetup(t, h, "p416 get")

	status, out := doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectID, phaseID)+"/2", "")
	if status != http.StatusOK {
		t.Fatalf("GET versions/2 = %d, want 200", status)
	}
	if out["version"] != float64(2) {
		t.Fatalf("returned version = %v, want 2", out["version"])
	}
	if out["content"] != "v2 content" {
		t.Fatalf("returned content = %v, want the stored v2 content (not the current v3)", out["content"])
	}
	hash, _ := out["content_hash"].(string)
	if hash == "" {
		t.Fatal("returned version has an empty content_hash")
	}
	// The hash must match the one reported for the same version by the
	// list endpoint (deterministic content hash).
	_, ls := doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectID, phaseID), "")
	var listed string
	for _, it := range ls["versions"].([]any) {
		row := it.(map[string]any)
		if row["version"] == float64(2) {
			listed, _ = row["content_hash"].(string)
		}
	}
	if listed != hash {
		t.Fatalf("get hash %q != list hash %q for version 2", hash, listed)
	}
}

// T4.16.3 — Unknown version returns 404 (the phase exists and has WBS,
// but the requested version was never stored).
func TestT4163UnknownVersion404(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsVersionSetup(t, h, "p416 unknown")

	status, _ := doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectID, phaseID)+"/99", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET versions/99 = %d, want 404", status)
	}
}

// T4.16.4 — Cross-project access is rejected with 404 on both history
// endpoints: a phase that belongs to another project is out of scope.
func TestT4164CrossProjectAccessRejected(t *testing.T) {
	h := NewHarness(t)
	projectA, phaseA := wbsVersionSetup(t, h, "p416 cross a")
	projectB, phaseB := wbsVersionSetup(t, h, "p416 cross b")

	// Phase B's history under project A.
	status, _ := doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectA, phaseB), "")
	if status != http.StatusNotFound {
		t.Fatalf("cross-project list = %d, want 404", status)
	}
	status, _ = doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectA, phaseB)+"/1", "")
	if status != http.StatusNotFound {
		t.Fatalf("cross-project get = %d, want 404", status)
	}
	// And the reverse direction.
	status, _ = doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectB, phaseA), "")
	if status != http.StatusNotFound {
		t.Fatalf("reverse cross-project list = %d, want 404", status)
	}
	// The legitimate access still works (the 404s are scoping, not a
	// broken endpoint).
	status, _ = doJSON(t, http.MethodGet, wbsVersionsURL(t, h, projectB, phaseB), "")
	if status != http.StatusOK {
		t.Fatalf("legitimate list = %d, want 200", status)
	}
}
