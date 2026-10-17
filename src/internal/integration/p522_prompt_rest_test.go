package integration

import (
	"net/http"
	"testing"
)

func promptURL(h *Harness, id string) string {
	return h.BaseURL + "/api/v1/projects/global/roles/" + id + "/system-prompt"
}
func promptVersionsURL(h *Harness, id string) string {
	return h.BaseURL + "/api/v1/projects/global/roles/" + id + "/system-prompt/versions"
}

// T5.22.1 — Current prompt returned.
func TestP522CurrentPromptReturned(t *testing.T) {
	h := NewHarness(t)
	code, body := httpJSON(t, http.MethodGet, rolesURL(h, "global"), nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	id := body["roles"].([]any)[0].(map[string]any)["id"].(string)
	code, got := httpJSON(t, http.MethodGet, promptURL(h, id), nil)
	if code != http.StatusOK {
		t.Fatalf("get current prompt = %d, want 200: %v", code, got)
	}
	if got["version"] == nil || got["content_hash"] == "" {
		t.Fatalf("current prompt missing version/hash: %v", got)
	}
}

// T5.22.2 — Update creates version.
func TestP522UpdateCreatesVersion(t *testing.T) {
	h := NewHarness(t)
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "prompt-role", "name": "P", "description": "d", "system_prompt": "v1",
	})
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	id := created["id"].(string)
	code, got := httpJSON(t, http.MethodPut, promptURL(h, id), map[string]any{"content": "v2", "expected_version": 1})
	if code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %v", code, got)
	}
	if got["version"].(float64) != 2 {
		t.Fatalf("updated version = %v, want 2", got["version"])
	}
}

// T5.22.3 — Historical versions returned.
func TestP522HistoricalVersionsReturned(t *testing.T) {
	h := NewHarness(t)
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "versions-role", "name": "V", "description": "d", "system_prompt": "v1",
	})
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	id := created["id"].(string)
	if code, _ := httpJSON(t, http.MethodPut, promptURL(h, id), map[string]any{"content": "v2", "expected_version": 1}); code != http.StatusOK {
		t.Fatal(code)
	}
	code, got := httpJSON(t, http.MethodGet, promptVersionsURL(h, id), nil)
	if code != http.StatusOK {
		t.Fatalf("versions = %d", code)
	}
	versions, _ := got["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
}

// T5.22.4 — Stale update returns conflict.
func TestP522StaleUpdateConflict(t *testing.T) {
	h := NewHarness(t)
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "stale-role", "name": "S", "description": "d", "system_prompt": "v1",
	})
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	id := created["id"].(string)
	// Expected version 1 (correct) succeeds → current becomes 2.
	if code, _ := httpJSON(t, http.MethodPut, promptURL(h, id), map[string]any{"content": "v2", "expected_version": 1}); code != http.StatusOK {
		t.Fatal(code)
	}
	// Now expected_version 1 is STALE (current is 2) → conflict.
	code, _ = httpJSON(t, http.MethodPut, promptURL(h, id), map[string]any{"content": "v3", "expected_version": 1})
	if code != http.StatusConflict {
		t.Fatalf("stale update = %d, want 409", code)
	}
}

// T5.22.5 — Prompt hash returned.
func TestP522PromptHashReturned(t *testing.T) {
	h := NewHarness(t)
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "hash-role", "name": "H", "description": "d", "system_prompt": "abc",
	})
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	id := created["id"].(string)
	code, got := httpJSON(t, http.MethodGet, promptURL(h, id), nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if got["content_hash"] == "" || got["content_hash"] == nil {
		t.Fatalf("prompt hash not returned: %v", got)
	}
}
