package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-24 — optimistic concurrency (project version, WBS 3.24).

// T3.24.1 — Matching version updates succeed.
func TestT3241MatchingVersionUpdate(t *testing.T) {
	h := NewHarness(t)
	p := createViaAPI(t, h, "p324-match", "")
	version := versionOf(t, h, p)
	body := `{"name":"renamed","version":` + itoa(version) + `}`
	status, out := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID, body)
	if status != http.StatusOK {
		t.Fatalf("matching-version PATCH = %d, want 200 (body %v)", status, out)
	}
	if out["name"] != "renamed" {
		t.Fatalf("PATCH did not apply: %+v", out)
	}
}

// T3.24.2 — Update increments version.
func TestT3242UpdateIncrementsVersion(t *testing.T) {
	h := NewHarness(t)
	p := createViaAPI(t, h, "p324-increment", "")
	before := versionOf(t, h, p)
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"v2","version":`+itoa(before)+`}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", status)
	}
	after := versionOf(t, h, p)
	if after != before+1 {
		t.Fatalf("version = %d, want %d", after, before+1)
	}
}

// T3.24.3 — Stale version rejected.
func TestT3243StaleVersionRejected(t *testing.T) {
	h := NewHarness(t)
	p := createViaAPI(t, h, "p324-stale", "")
	v := versionOf(t, h, p)
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"first","version":`+itoa(v)+`}`)
	if status != http.StatusOK {
		t.Fatalf("first PATCH = %d, want 200", status)
	}
	// The same (now stale) version is rejected.
	status, out := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"second","version":`+itoa(v)+`}`)
	if status != http.StatusConflict {
		t.Fatalf("stale PATCH = %d, want 409 (body %v)", status, out)
	}
}

// T3.24.4 — Stale update doesn't overwrite current data.
func TestT3244StaleUpdateNoOverwrite(t *testing.T) {
	h := NewHarness(t)
	p := createViaAPI(t, h, "p324-noover", "")
	v := versionOf(t, h, p)
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"winner","version":`+itoa(v)+`}`)
	if status != http.StatusOK {
		t.Fatalf("first PATCH = %d, want 200", status)
	}
	status, _ = doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"loser","version":`+itoa(v)+`}`)
	if status != http.StatusConflict {
		t.Fatalf("stale PATCH = %d, want 409", status)
	}
	var name string
	if err := h.DBScan(context.Background(),
		"SELECT name FROM projects WHERE id = '"+p.ID+"'", &name); err != nil {
		t.Fatal(err)
	}
	if name != "winner" {
		t.Fatalf("stale update overwrote data: name = %q, want winner", name)
	}
}

// T3.24.5 — Conflict returns HTTP 409 (structured body, version present in
// responses so clients can retry).
func TestT3245Conflict409(t *testing.T) {
	h := NewHarness(t)
	p := createViaAPI(t, h, "p324-409", "")
	v := versionOf(t, h, p)
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"a","version":`+itoa(v)+`}`)
	if status != http.StatusOK {
		t.Fatalf("first PATCH = %d, want 200", status)
	}
	status, out := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+p.ID,
		`{"name":"b","version":`+itoa(v)+`}`)
	if status != http.StatusConflict {
		t.Fatalf("stale PATCH = %d, want 409", status)
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("409 body missing error: %v", out)
	}
	// The project GET exposes the current version.
	status, out = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+p.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d, want 200", status)
	}
	if out["version"] == nil {
		t.Fatalf("GET body missing version: %+v", out)
	}
	// The settings response also carries the project version.
	status, out = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+p.ID+"/settings", "")
	if status != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200", status)
	}
	if out["version"] == nil {
		t.Fatalf("settings body missing version: %+v", out)
	}
	// A settings PUT with the stale version is also 409.
	settingsBody := settingsBody(map[string]any{"version": float64(v)})
	status, _ = doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+p.ID+"/settings", settingsBody)
	if status != http.StatusConflict {
		t.Fatalf("stale settings PUT = %d, want 409", status)
	}
}

// projectVersion returns the current stored project version (direct SQL —
// the only way to read it for service-level tests, WBS 3.24).
func projectVersion(t *testing.T, h *Harness, id string) int {
	t.Helper()
	var v int
	if err := h.DBScan(context.Background(),
		"SELECT version FROM projects WHERE id = '"+id+"'", &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// versionOf returns the current project version from the API.
func versionOf(t *testing.T, h *Harness, p projectAPIBody) int {
	t.Helper()
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+p.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET project = %d, want 200", status)
	}
	v, ok := out["version"].(float64)
	if !ok {
		t.Fatalf("GET body missing numeric version: %+v", out)
	}
	return int(v)
}
