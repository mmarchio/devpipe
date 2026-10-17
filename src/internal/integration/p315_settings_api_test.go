package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// settingsBody builds a full settings payload. The base is the create-time
// defaults (the service inserts empty providers, 200000 window, 0.8
// threshold, 5m timeout on project creation) so that a PUT changing only the
// overridden field leaves every protected field equal to the stored value.
func settingsBody(overrides ...map[string]any) string {
	base := map[string]any{
		"working_directory":    "",
		"worker_provider":      "prov",
		"worker_model":         "model",
		"compression_provider": "",
		"compression_model":    "",
		"context_window":       200000,
		"context_threshold":    0.8,
		"auto_continue":        false,
		"max_retries":          0,
		"worker_timeout":       "5m",
		"version":              1,
	}
	if len(overrides) > 0 {
		for k, v := range overrides[0] {
			base[k] = v
		}
	}
	b, _ := json.Marshal(base)
	return string(b)
}

// T3.15.1 — GET settings returns 200 with stored values.
func TestT3151GetSettings(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "settings get", "")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings", "")
	if status != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200", status)
	}
	var dir string
	if err := h.DBScan(context.Background(),
		"SELECT working_directory FROM project_settings WHERE project_id = '"+created.ID+"'", &dir); err != nil {
		t.Fatal(err)
	}
	// The create defaults (empty dir) must round-trip through the API.
	if out["working_directory"] != dir {
		t.Fatalf("GET body dir = %v, stored %q", out["working_directory"], dir)
	}
}

// T3.15.2 — PUT settings persists new state and returns 200.
func TestT3152PutSettings(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "settings put", "")
	status, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		settingsBody(map[string]any{"worker_provider": "newprov", "working_directory": "/tmp/work"}))
	if status != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", status)
	}
	var prov, dir string
	if err := h.DBScan(context.Background(),
		"SELECT worker_provider, working_directory FROM project_settings WHERE project_id = '"+created.ID+"'",
		&prov, &dir); err != nil {
		t.Fatal(err)
	}
	if prov != "newprov" || dir != "/tmp/work" {
		t.Fatalf("persisted prov=%q dir=%q", prov, dir)
	}
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings", "")
	if status != http.StatusOK || out["worker_provider"] != "newprov" {
		t.Fatalf("GET after PUT = %d %+v", status, out)
	}
}

// T3.15.3 — Unknown project returns 404 on both GET and PUT.
func TestT3153UnknownProject404(t *testing.T) {
	h := NewHarness(t)
	id := "00000000-0000-0000-0000-000000000000"
	status, _ := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+id+"/settings", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown = %d, want 404", status)
	}
	status, _ = doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+id+"/settings", settingsBody())
	if status != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", status)
	}
}

// T3.15.4 — Invalid settings payloads return 400; nothing persisted.
func TestT3154InvalidSettings400(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "settings invalid", "")
	cases := map[string]string{
		"missing field": `{"working_directory":"/x"}`,
		"unknown field": settingsBody(map[string]any{"bogus": "1"}),
		"bad threshold": settingsBody(map[string]any{"context_threshold": 1.5}),
		"neg retries":   settingsBody(map[string]any{"max_retries": -1}),
		"zero window":   settingsBody(map[string]any{"context_window": 0}),
		"bad timeout":   settingsBody(map[string]any{"worker_timeout": "nope"}),
		"malformed":     `{not json`,
		"non-object":    `[1,2,3]`,
	}
	for name, body := range cases {
		status, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings", body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: PUT = %d, want 400", name, status)
		}
	}
	var dir string
	if err := h.DBScan(context.Background(),
		"SELECT working_directory FROM project_settings WHERE project_id = '"+created.ID+"'", &dir); err != nil {
		t.Fatal(err)
	}
	// T3.15.4: the T3.15.2 PUT already set dir to /tmp/work in this test's
	// harness... actually this is a fresh harness, so the create default
	// (empty) must be unchanged by the rejected PUTs.
	if dir != "" {
		t.Fatalf("rejected PUTs altered row: %q", dir)
	}
}

// T3.15.5 — Protected changes on RUNNING return 409; non-protected allowed.
func TestT3155RunningProtectedConflict(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "settings running", "")
	if _, err := h.DBExec(context.Background(),
		"UPDATE projects SET status = 'RUNNING' WHERE id = '"+created.ID+"'"); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		settingsBody(map[string]any{"context_window": 8192}))
	if status != http.StatusConflict {
		t.Fatalf("protected PUT (RUNNING) = %d, want 409", status)
	}
	var cw int
	if err := h.DBScan(context.Background(),
		"SELECT context_window FROM project_settings WHERE project_id = '"+created.ID+"'", &cw); err != nil {
		t.Fatal(err)
	}
	if cw != 200000 {
		t.Fatalf("protected change persisted: window=%d, want 200000 (create default)", cw)
	}
	status, _ = doJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		settingsBody(map[string]any{"worker_provider": "liveprov"}))
	if status != http.StatusOK {
		t.Fatalf("non-protected PUT (RUNNING) = %d, want 200", status)
	}
}

// T3.15.6 — Settings response never exposes secrets. The WBS states "Future
// provider API keys should not be returned in plaintext." The response must
// not contain any field whose name suggests a secret.
func TestT3156SecretsNotExposed(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "settings secrets", "")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID+"/settings", "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d, want 200", status)
	}
	// No secret-named fields in the response.
	for _, key := range []string{"api_key", "provider_api_key", "password", "secret_key", "token"} {
		if _, exists := out[key]; exists {
			t.Fatalf("settings response contains secret field %q: %+v", key, out)
		}
	}
	// The known fields are present (non-secret).
	for _, key := range []string{"working_directory", "worker_provider", "worker_model", "context_window"} {
		if _, exists := out[key]; !exists {
			t.Fatalf("settings response missing expected field %q: %+v", key, out)
		}
	}
}
