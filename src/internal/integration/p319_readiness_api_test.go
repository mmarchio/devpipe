package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// P3-19 readiness endpoints (WBS 3.19, T3.19.1-T3.19.5):
// GET /api/v1/projects/{id}/readiness (structured report, non-mutating) and
// POST /api/v1/projects/{id}/ready (DRAFT→READY transition).

func readinessURL(t *testing.T, h *Harness, projectID string, ready bool) string {
	t.Helper()
	u := h.BaseURL + "/api/v1/projects/" + projectID + "/readiness"
	if ready {
		u = h.BaseURL + "/api/v1/projects/" + projectID + "/ready"
	}
	return u
}

func p319Settings(overrides ...map[string]any) string {
	base := map[string]any{
		"working_directory":    "",
		"worker_provider":      "hermes",
		"worker_model":         "default",
		"compression_provider": "llm",
		"compression_model":    "default",
		"context_window":       200000,
		"context_threshold":    0.8,
		"auto_continue":        false,
		"max_retries":          3,
		"worker_timeout":       "5m",
		"version":              1,
	}
	for _, o := range overrides {
		for k, v := range o {
			base[k] = v
		}
	}
	b, _ := json.Marshal(base)
	return string(b)
}

// makeReadyProject drives a project to readiness-complete through the API:
// full settings (real temp working dir) + one phase with a WBS document.
func makeReadyProject(t *testing.T, h *Harness, dir string) string {
	t.Helper()
	created := createViaAPI(t, h, "p319 ready", "")
	if st, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{"working_directory": dir})); st != http.StatusOK {
		t.Fatalf("setup PUT settings = %d", st)
	}
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"p1"}`)
	phaseID := out["id"].(string)
	if st, _ := doJSON(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/phases/"+phaseID+"/wbs",
		`{"content":"# WBS"}`); st != http.StatusOK {
		t.Fatalf("setup PUT wbs = %d", st)
	}
	return created.ID
}

// T3.19.1 — Ready project reports true.
func TestT3191ReadyReportsTrue(t *testing.T) {
	h := NewHarness(t)
	id := makeReadyProject(t, h, t.TempDir())
	status, out := doJSON(t, http.MethodGet, readinessURL(t, h, id, false), "")
	if status != http.StatusOK {
		t.Fatalf("GET readiness = %d, want 200", status)
	}
	if out["ready"] != true {
		t.Fatalf("ready = %v, want true", out["ready"])
	}
}

// T3.19.2 — Invalid project reports individual failed checks.
func TestT3192InvalidReportsFailedChecks(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p319 invalid", "")
	// Leave worker_model empty: the project is not readiness-complete.
	if st, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{"worker_model": ""})); st != http.StatusOK {
		t.Fatalf("setup PUT settings = %d", st)
	}
	status, out := doJSON(t, http.MethodGet, readinessURL(t, h, created.ID, false), "")
	if status != http.StatusOK {
		t.Fatalf("GET readiness = %d, want 200", status)
	}
	if out["ready"] != false {
		t.Fatalf("ready = %v, want false", out["ready"])
	}
	checks, _ := out["checks"].(map[string]any)
	if checks == nil {
		t.Fatalf("checks missing from response: %+v", out)
	}
	if checks["worker_model"] == "ok" {
		t.Fatalf("worker_model check = ok, want a failure indication: %v", checks)
	}
}

// T3.19.3 — Ready action transitions valid project.
func TestT3193ReadyActionTransitions(t *testing.T) {
	h := NewHarness(t)
	id := makeReadyProject(t, h, t.TempDir())
	status, _ := doJSON(t, http.MethodPost, readinessURL(t, h, id, true), "")
	if status != http.StatusOK {
		t.Fatalf("POST ready = %d, want 200", status)
	}
	var statusStr string
	if err := h.DBScan(context.Background(),
		"SELECT status FROM projects WHERE id = '"+id+"'", &statusStr); err != nil {
		t.Fatal(err)
	}
	if statusStr != "READY" {
		t.Fatalf("stored status = %q, want READY", statusStr)
	}
}

// T3.19.4 — Ready action doesn't transition invalid project.
func TestT3194ReadyActionNoTransitionOnInvalid(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p319 invalid-post", "")
	if st, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{"worker_model": ""})); st != http.StatusOK {
		t.Fatalf("setup PUT settings = %d", st)
	}
	status, _ := doJSON(t, http.MethodPost, readinessURL(t, h, created.ID, true), "")
	if status != http.StatusBadRequest {
		t.Fatalf("POST ready on invalid project = %d, want 400", status)
	}
	var statusStr string
	if err := h.DBScan(context.Background(),
		"SELECT status FROM projects WHERE id = '"+created.ID+"'", &statusStr); err != nil {
		t.Fatal(err)
	}
	if statusStr != "DRAFT" {
		t.Fatalf("stored status = %q, want DRAFT (untransitioned)", statusStr)
	}
}

// T3.19.5 — Readiness endpoint doesn't mutate project.
func TestT3195ReadinessEndpointNonMutating(t *testing.T) {
	h := NewHarness(t)
	id := makeReadyProject(t, h, t.TempDir())
	var updatedAt string
	var events int
	ctx := context.Background()
	if err := h.DBScan(ctx, "SELECT updated_at::text FROM projects WHERE id = '"+id+"'", &updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+id+"'", &events); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodGet, readinessURL(t, h, id, false), "")
	if status != http.StatusOK {
		t.Fatalf("GET readiness = %d, want 200", status)
	}
	var afterUpdated string
	var afterEvents int
	if err := h.DBScan(ctx, "SELECT updated_at::text FROM projects WHERE id = '"+id+"'", &afterUpdated); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+id+"'", &afterEvents); err != nil {
		t.Fatal(err)
	}
	if afterUpdated != updatedAt {
		t.Fatalf("GET readiness mutated updated_at: %q -> %q", updatedAt, afterUpdated)
	}
	if afterEvents != events {
		t.Fatalf("GET readiness wrote events: %d -> %d", events, afterEvents)
	}
	var statusStr string
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+id+"'", &statusStr); err != nil {
		t.Fatal(err)
	}
	if statusStr != "DRAFT" {
		t.Fatalf("stored status = %q, want DRAFT (GET readiness must not transition)", statusStr)
	}
}
