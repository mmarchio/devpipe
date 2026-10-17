package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/dbrepo"
)

func instrURL(h *Harness, id string) string {
	return h.BaseURL + "/api/v1/projects/global/instructions/" + id
}

func createInstruction(t *testing.T, h *Harness, key, name, content string) map[string]any {
	t.Helper()
	code, body := httpJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/global/instructions", map[string]any{
		"key": key, "name": name, "content": content,
	})
	if code != http.StatusCreated {
		t.Fatalf("create instruction = %d, want 201: %v", code, body)
	}
	return body
}

// T5.23.1 — List instructions.
func TestP523ListInstructions(t *testing.T) {
	h := NewHarness(t)
	createInstruction(t, h, "list-instr", "L", "c")
	code, got := httpJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/global/instructions", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	list, _ := got["instructions"].([]any)
	if len(list) < 1 {
		t.Fatalf("instructions = %d, want >= 1", len(list))
	}
}

// T5.23.2 — Create instruction.
func TestP523CreateInstruction(t *testing.T) {
	h := NewHarness(t)
	body := createInstruction(t, h, "create-instr", "C", "content")
	if body["version"].(float64) != 1 {
		t.Fatalf("new version = %v, want 1", body["version"])
	}
	if body["key"] == "" {
		t.Fatal("missing key")
	}
}

// T5.23.3 — Update creates new version.
func TestP523UpdateNewVersion(t *testing.T) {
	h := NewHarness(t)
	created := createInstruction(t, h, "update-instr", "U", "v1")
	id := created["id"].(string)
	code, got := httpJSON(t, http.MethodPut, instrURL(h, id), map[string]any{"content": "v2", "expected_version": 1})
	if code != http.StatusOK {
		t.Fatalf("update = %d, want 200: %v", code, got)
	}
	if got["version"].(float64) != 2 {
		t.Fatalf("version after update = %v, want 2", got["version"])
	}
}

// T5.23.4 — Delete un-referenced instruction succeeds.
func TestP523DeleteUnreferenced(t *testing.T) {
	h := NewHarness(t)
	created := createInstruction(t, h, "delete-instr", "D", "c")
	id := created["id"].(string)
	code, got := httpJSON(t, http.MethodDelete, instrURL(h, id), nil)
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("delete = %d, want 204/200: %v", code, got)
	}
	// Now it is gone.
	code, _ = httpJSON(t, http.MethodGet, instrURL(h, id), nil)
	if code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}
}

// T5.23.5 — Delete referenced instruction is protected.
func TestP523DeleteReferencedProtected(t *testing.T) {
	h := NewHarness(t)
	created := createInstruction(t, h, "ref-instr", "R", "c")
	id := created["id"].(string)
	// Create a project role and assign the instruction to it.
	code, role := httpJSON(t, http.MethodPost, rolesURL(h, "global"), map[string]any{
		"key": "ref-role", "name": "R", "description": "d", "system_prompt": "sp",
	})
	if code != http.StatusCreated {
		t.Fatalf("create role = %d", code)
	}
	roleID := role["id"].(string)
	assignRepo := dbrepo.NewRoleInstructionAssignmentRepository(h.App.DB().Raw())
	if _, err := assignRepo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, id)); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// Now deleting the referenced instruction must be protected.
	code, got := httpJSON(t, http.MethodDelete, instrURL(h, id), nil)
	if code != http.StatusConflict {
		t.Fatalf("delete referenced = %d, want 409: %v", code, got)
	}
}
