package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// P3-18 WBS API (WBS 3.18, T3.18.1-T3.18.7): GET/PUT
// /api/v1/projects/{projectID}/phases/{phaseID}/wbs with version information,
// markdown round-trip, version increment, cross-project rejection, and
// protected executed WBS.

func wbsURL(t *testing.T, h *Harness, projectID, phaseID string) string {
	t.Helper()
	return h.BaseURL + "/api/v1/projects/" + projectID + "/phases/" + phaseID + "/wbs"
}

// wbsPutBody wraps content in the strict WBS PUT payload.
func wbsPutBody(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return `{"content":` + string(raw) + `}`
}

// wbsSetup creates a project + a phase via the API and returns their ids.
func wbsSetup(t *testing.T, h *Harness, name string) (string, string) {
	t.Helper()
	created := createViaAPI(t, h, name, "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Phase 1"}`)
	return created.ID, out["id"].(string)
}

// T3.18.1 — WBS created: PUT stores the document at version 1.
func TestT3181WBSCreated(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p318 create")
	status, out := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "# Phase 1\n\n- [ ] do things"))
	if status != http.StatusOK {
		t.Fatalf("PUT WBS = %d, want 200", status)
	}
	if out["version"] != float64(1) {
		t.Fatalf("response version = %v, want 1", out["version"])
	}
	var version int
	var content string
	if err := h.DBScan(context.Background(),
		"SELECT version, content FROM phase_documents WHERE phase_id = '"+phaseID+"' AND doc_type = 'WBS'",
		&version, &content); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("stored version = %d, want 1", version)
	}
	if content != "# Phase 1\n\n- [ ] do things" {
		t.Fatalf("stored content = %q, want the PUT payload", content)
	}
}

// T3.18.2 — WBS retrieved: GET returns phase_id, version, content.
func TestT3182WBSRetrieved(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p318 retrieve")
	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "# WBS body")); status != http.StatusOK {
		t.Fatal("setup PUT failed")
	}
	status, out := doJSON(t, http.MethodGet, wbsURL(t, h, projectID, phaseID), "")
	if status != http.StatusOK {
		t.Fatalf("GET WBS = %d, want 200", status)
	}
	if out["phase_id"] != phaseID {
		t.Fatalf("phase_id = %v, want %s", out["phase_id"], phaseID)
	}
	if out["version"] != float64(1) {
		t.Fatalf("version = %v, want 1", out["version"])
	}
	if out["content"] != "# WBS body" {
		t.Fatalf("content = %v, want the stored body", out["content"])
	}
}

// T3.18.3 — Markdown round-trips unchanged (exact byte equality).
func TestT3183MarkdownRoundTrip(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p318 roundtrip")
	content := "# Phase 3\n\n## Work unit\n- `code` with **bold**, \"quotes\" and \\ backslash\n\n\ttab-indented\n"
	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, content)); status != http.StatusOK {
		t.Fatal("setup PUT failed")
	}
	_, out := doJSON(t, http.MethodGet, wbsURL(t, h, projectID, phaseID), "")
	if out["content"] != content {
		t.Fatalf("round-tripped content differs:\n got %q\nwant %q", out["content"], content)
	}
}

// T3.18.4 — Update increments version.
func TestT3184UpdateIncrementsVersion(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p318 version")
	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "v1")); status != http.StatusOK {
		t.Fatal("setup PUT failed")
	}
	status, out := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "v2"))
	if status != http.StatusOK {
		t.Fatalf("PUT update = %d, want 200", status)
	}
	if out["version"] != float64(2) {
		t.Fatalf("version after update = %v, want 2", out["version"])
	}
	var version int
	var content string
	if err := h.DBScan(context.Background(),
		"SELECT version, content FROM phase_documents WHERE phase_id = '"+phaseID+"' AND doc_type = 'WBS'",
		&version, &content); err != nil {
		t.Fatal(err)
	}
	if version != 2 || content != "v2" {
		t.Fatalf("stored version/content = %d/%q, want 2/v2", version, content)
	}
}

// T3.18.5 — Unknown phase returns 404.
func TestT3185UnknownPhase404(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p318 unknown", "")
	status, _ := doJSON(t, http.MethodGet,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/phases/00000000-0000-0000-0000-000000000000/wbs", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown phase WBS = %d, want 404", status)
	}
}

// T3.18.6 — Phase belonging to another project rejected.
func TestT3186CrossProjectRejected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p318 owner", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Phase 1"}`)
	phaseID := out["id"].(string)
	other := createViaAPI(t, h, "p318 other", "")
	status, _ := doJSON(t, http.MethodGet, wbsURL(t, h, other.ID, phaseID), "")
	if status != http.StatusNotFound {
		t.Fatalf("GET cross-project WBS = %d, want 404", status)
	}
	status, _ = doJSON(t, http.MethodPut, wbsURL(t, h, other.ID, phaseID), wbsPutBody(t, "x"))
	if status != http.StatusNotFound {
		t.Fatalf("PUT cross-project WBS = %d, want 404", status)
	}
}

// T3.18.7 — Protected executed WBS cannot be silently rewritten.
func TestT3187ProtectedExecutedWBS(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p318 protected")
	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "original")); status != http.StatusOK {
		t.Fatal("setup PUT failed")
	}
	if _, err := h.DBExec(context.Background(),
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID), wbsPutBody(t, "rewritten"))
	if status != http.StatusConflict {
		t.Fatalf("PUT executed-phase WBS = %d, want 409", status)
	}
	var content string
	if err := h.DBScan(context.Background(),
		"SELECT content FROM phase_documents WHERE phase_id = '"+phaseID+"' AND doc_type = 'WBS'",
		&content); err != nil {
		t.Fatal(err)
	}
	if content != "original" {
		t.Fatalf("protected WBS content = %q, want original (unmutated)", content)
	}
}
