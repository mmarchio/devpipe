package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func httpJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(rb) > 0 {
		_ = json.Unmarshal(rb, &out)
	}
	return resp.StatusCode, out
}

func rolesURL(h *Harness, scope string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles"
}
func roleURL(h *Harness, scope, id string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles/" + id
}

// T5.21.1 — List roles.
func TestP521ListRoles(t *testing.T) {
	h := NewHarness(t)
	code, body := httpJSON(t, http.MethodGet, rolesURL(h, "global"), nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %v", code, body)
	}
	roles, ok := body["roles"].([]any)
	if !ok {
		t.Fatalf("list body missing roles: %v", body)
	}
	if len(roles) < 5 {
		t.Fatalf("global roles = %d, want >= 5 (defaults)", len(roles))
	}
}

// T5.21.2 — Get role.
func TestP521GetRole(t *testing.T) {
	h := NewHarness(t)
	code, body := httpJSON(t, http.MethodGet, rolesURL(h, "global"), nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	id := body["roles"].([]any)[0].(map[string]any)["id"].(string)
	code, got := httpJSON(t, http.MethodGet, roleURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d, want 200", code)
	}
	if got["id"] != id {
		t.Fatalf("get id = %v, want %s", got["id"], id)
	}
}

// T5.21.3 — Create custom role.
func TestP521CreateRole(t *testing.T) {
	h := NewHarness(t)
	code, got := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "custom-role", "name": "Custom", "description": "d", "system_prompt": "sp",
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %v", code, got)
	}
	if got["key"] == "" || got["name"] != "Custom" {
		t.Fatalf("create response = %v", got)
	}
}

// T5.21.4 — Update role.
func TestP521UpdateRole(t *testing.T) {
	h := NewHarness(t)
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "updatable-role", "name": "Before", "description": "d", "system_prompt": "sp",
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	id := created["id"].(string)
	code, got := httpJSON(t, http.MethodPatch, roleURL(h, "global", id), map[string]any{"name": "After"})
	if code != http.StatusOK {
		t.Fatalf("patch = %d, want 200: %v", code, got)
	}
	if got["name"] != "After" {
		t.Fatalf("patched name = %v, want After", got["name"])
	}
}

// T5.21.5 — Unknown role returns 404.
func TestP521UnknownRole404(t *testing.T) {
	h := NewHarness(t)
	code, _ := httpJSON(t, http.MethodGet, roleURL(h, "global", "00000000-0000-0000-0000-000000000000"), nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown role = %d, want 404", code)
	}
}

// T5.21.6 — Historically referenced role cannot be destructively deleted.
func TestP521HistoricallyReferencedRoleProtected(t *testing.T) {
	h := NewHarness(t)
	// The seeded global defaults all have prompt history → protected.
	code, body := httpJSON(t, http.MethodGet, rolesURL(h, "global"), nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	id := body["roles"].([]any)[0].(map[string]any)["id"].(string)
	code, _ = httpJSON(t, http.MethodDelete, roleURL(h, "global", id), nil)
	if code != http.StatusConflict {
		t.Fatalf("delete of historically-referenced role = %d, want 409 (disable preferred)", code)
	}
}
