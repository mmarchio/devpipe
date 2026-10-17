package integration

import (
	"net/http"
	"testing"
)

// previewURL builds the role preview REST URL (WBS 5.27).
func previewURL(h *Harness, scope, roleID string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles/" + roleID + "/preview"
}

// fetchInstructionIDs returns three global instruction IDs, creating them
// over REST when the fresh database has none (only default roles are seeded).
func fetchInstructionIDs(t *testing.T, h *Harness) []string {
	t.Helper()
	code, body := httpJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/global/instructions", nil)
	if code != http.StatusOK {
		t.Fatalf("list instructions = %d: %v", code, body)
	}
	list, _ := body["instructions"].([]any)
	if len(list) >= 3 {
		out := make([]string, 0, len(list))
		for _, e := range list {
			out = append(out, e.(map[string]any)["id"].(string))
		}
		return out[:3]
	}
	for i, key := range []string{"instr-alpha", "instr-mike", "instr-xray"} {
		payload := map[string]any{
			"key": key, "name": key + " name", "description": "d",
			"content": "instruction content " + key,
		}
		code, got := httpJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/global/instructions", payload)
		if code != http.StatusCreated {
			t.Fatalf("create instruction %d = %d: %v", i, code, got)
		}
		list = append(list, got)
	}
	out := make([]string, 0, 3)
	for _, e := range list[:3] {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

// seedPreviewRole builds a role fully configured over REST (prompt, three
// assigned instructions in position order, allowed states, state files) and
// returns its ID.
func seedPreviewRole(t *testing.T, h *Harness, key string) string {
	t.Helper()
	code, created := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": key, "name": key + " name", "description": "d", "system_prompt": "preview system prompt",
	})
	if code != http.StatusCreated {
		t.Fatalf("create role = %d: %v", code, created)
	}
	id := created["id"].(string)
	ids := fetchInstructionIDs(t, h)
	if len(ids) < 3 {
		t.Fatalf("global instructions = %d, want >= 3", len(ids))
	}
	for _, instID := range ids[:3] {
		code, got := httpJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/global/roles/"+id+"/instructions",
			map[string]any{"instruction": instID})
		if code != http.StatusCreated {
			t.Fatalf("assign = %d: %v", code, got)
		}
	}
	code, _ = httpJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/global/roles/"+id+"/states",
		map[string]any{"states": []any{"implementation", "continuation"}})
	if code != http.StatusOK {
		t.Fatalf("set states = %d", code)
	}
	code, _ = httpJSON(t, http.MethodPut, h.BaseURL+"/api/v1/projects/global/roles/"+id+"/state-files",
		map[string]any{"state_files": []any{"ACTIVE_TASK", "PROJECT_STATE"}})
	if code != http.StatusOK {
		t.Fatalf("set state files = %d", code)
	}
	return id
}

// previewListStrings extracts a []string from a preview body field.
func previewListStrings(t *testing.T, body map[string]any, field string) []string {
	t.Helper()
	raw, _ := body[field].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(string))
	}
	return out
}

// T5.27.1 — Preview contains the role's current system-prompt content.
func TestP527PreviewContainsSystemPrompt(t *testing.T) {
	h := NewHarness(t)
	id := seedPreviewRole(t, h, "preview-prompt")

	code, got := httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("preview = %d: %v", code, got)
	}
	sys, _ := got["system_prompt"].(map[string]any)
	if sys == nil {
		t.Fatalf("preview body missing system_prompt: %v", got)
	}
	if sys["content"] != "preview system prompt" {
		t.Fatalf("system_prompt.content = %v, want seeded content", sys["content"])
	}
	if v, _ := sys["version"].(float64); v != 1 {
		t.Fatalf("system_prompt.version = %v, want 1", sys["version"])
	}
}

// T5.27.2 — Preview lists the instructions in assignment (position) order.
func TestP527PreviewInstructionsOrdered(t *testing.T) {
	h := NewHarness(t)
	id := seedPreviewRole(t, h, "preview-order")

	code, got := httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("preview = %d: %v", code, got)
	}
	list, _ := got["instructions"].([]any)
	if len(list) != 3 {
		t.Fatalf("preview instructions = %d, want 3", len(list))
	}
	keys := []string{}
	for _, e := range list {
		keys = append(keys, e.(map[string]any)["key"].(string))
	}
	assignCode, assignBody := httpJSON(t, http.MethodGet,
		h.BaseURL+"/api/v1/projects/global/roles/"+id+"/instructions", nil)
	if assignCode != http.StatusOK {
		t.Fatal(assignCode)
	}
	assignList, _ := assignBody["instructions"].([]any)
	want := []string{}
	for _, e := range assignList {
		want = append(want, e.(map[string]any)["instruction_key"].(string))
	}
	if len(want) != 3 || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Fatalf("preview order = %v, want assignment order %v", keys, want)
	}
}

// T5.27.3 — Preview includes the role's allowed states (state eligibility).
func TestP527PreviewIncludesAllowedStates(t *testing.T) {
	h := NewHarness(t)
	id := seedPreviewRole(t, h, "preview-states")

	code, got := httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("preview = %d: %v", code, got)
	}
	states := previewListStrings(t, got, "allowed_states")
	// Allowed states are a set (storage order is not the input order):
	// compare as a set.
	if len(states) != 2 || !hasAll(states, "implementation", "continuation") {
		t.Fatalf("allowed_states = %v, want set {implementation continuation}", states)
	}
}

// T5.27.4 — Preview includes the role's state-file requirements, in order.
func TestP527PreviewIncludesStateFiles(t *testing.T) {
	h := NewHarness(t)
	id := seedPreviewRole(t, h, "preview-files")

	code, got := httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("preview = %d: %v", code, got)
	}
	files := previewListStrings(t, got, "state_files")
	if len(files) != 2 || files[0] != "ACTIVE_TASK" || files[1] != "PROJECT_STATE" {
		t.Fatalf("state_files = %v, want [ACTIVE_TASK PROJECT_STATE]", files)
	}
}

// T5.27.5 — Preview includes the configuration hash, and the hash tracks
// configuration changes: it is present and well-formed, and changes when a
// component (the system prompt) changes.
func TestP527PreviewIncludesConfigurationHash(t *testing.T) {
	h := NewHarness(t)
	id := seedPreviewRole(t, h, "preview-hash")

	code, got := httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("preview = %d: %v", code, got)
	}
	hash, _ := got["configuration_hash"].(string)
	if len(hash) != 64 || !isHex(hash) {
		t.Fatalf("configuration_hash = %q, want 64-hex", hash)
	}

	// Change the system prompt (expected_version = 1, the seeded one).
	code, putBody := httpJSON(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/global/roles/"+id+"/system-prompt",
		map[string]any{"content": "changed prompt", "expected_version": 1})
	if code != http.StatusOK {
		t.Fatalf("put prompt = %d: %v", code, putBody)
	}

	code, got = httpJSON(t, http.MethodGet, previewURL(h, "global", id), nil)
	if code != http.StatusOK {
		t.Fatalf("re-preview = %d: %v", code, got)
	}
	newHash, _ := got["configuration_hash"].(string)
	if newHash == hash {
		t.Fatalf("configuration hash unchanged after prompt change: %s", hash)
	}
}

// T5.27.6 — Preview for an unknown role returns 404 (missing role
// ErrNotFound).
func TestP527PreviewUnknownRole404(t *testing.T) {
	h := NewHarness(t)
	code, _ := httpJSON(t, http.MethodGet, previewURL(h, "global", "00000000-0000-0000-0000-000000000000"), nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown role preview = %d, want 404", code)
	}
}

func hasAll(list []string, want ...string) bool {
	for _, w := range want {
		ok := false
		for _, v := range list {
			if v == w {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
