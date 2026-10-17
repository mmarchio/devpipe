package integration

import (
	"net/http"
	"testing"
)

// roleInstrURL builds the role-instruction assignment REST base for a role.
func roleInstrURL(h *Harness, scope, roleID string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles/" + roleID + "/instructions"
}

// T5.24.1 — Assignment created via POST.
func TestP524AssignmentCreated(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "eng-assign", "E")
	instr := createInstruction(t, h, "a1-instr", "A1", "c")
	instrID := instr["id"].(string)

	code, got := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": instrID})
	if code != http.StatusCreated {
		t.Fatalf("assign = %d, want 201: %v", code, got)
	}
	if got["position"].(float64) != 1 {
		t.Fatalf("first assignment position = %v, want 1", got["position"])
	}
}

// T5.24.2 — Assignment removed via DELETE.
func TestP524AssignmentRemoved(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "eng-unassign", "E")
	instr := createInstruction(t, h, "a2-instr", "A2", "c")
	instrID := instr["id"].(string)

	if code, got := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": instrID}); code != http.StatusCreated {
		t.Fatalf("assign = %d: %v", code, got)
	}
	code, got := httpJSON(t, http.MethodDelete, roleInstrURL(h, "global", roleID)+"/"+instrID, nil)
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("unassign = %d, want 204/200: %v", code, got)
	}
	// Now the list is empty.
	code, body := httpJSON(t, http.MethodGet, roleInstrURL(h, "global", roleID), nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	list, _ := body["instructions"].([]any)
	if len(list) != 0 {
		t.Fatalf("assignments after unassign = %d, want 0", len(list))
	}
}

// T5.24.3 — Order preserved (GET returns assignment position order).
func TestP524OrderPreserved(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "eng-order", "E")
	var ids []string
	for i, key := range []string{"o-a", "o-b", "o-c"} {
		instr := createInstruction(t, h, key, "O"+itoaSafe(i), "c")
		id := instr["id"].(string)
		ids = append(ids, id)
		if code, got := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": id}); code != http.StatusCreated {
			t.Fatalf("assign %s = %d: %v", key, code, got)
		}
	}
	code, body := httpJSON(t, http.MethodGet, roleInstrURL(h, "global", roleID), nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	list, _ := body["instructions"].([]any)
	if len(list) != 3 {
		t.Fatalf("assignments = %d, want 3", len(list))
	}
	// Positions must be the exact insertion order 1,2,3 and the instruction IDs in order.
	for i, item := range list {
		m := item.(map[string]any)
		if m["position"].(float64) != float64(i+1) {
			t.Fatalf("position at index %d = %v, want %d", i, m["position"], i+1)
		}
		if m["instruction_id"].(string) != ids[i] {
			t.Fatalf("instruction at index %d = %v, want %s", i, m["instruction_id"], ids[i])
		}
	}
}

// T5.24.4 — Duplicate assignment rejected.
func TestP524DuplicateRejected(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "eng-dup", "E")
	instr := createInstruction(t, h, "dup-instr", "D", "c")
	instrID := instr["id"].(string)
	if code, _ := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": instrID}); code != http.StatusCreated {
		t.Fatalf("first assign = %d", code)
	}
	code, got := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": instrID})
	if code != http.StatusConflict {
		t.Fatalf("duplicate assign = %d, want 409: %v", code, got)
	}
}

// T5.24.5 — Cross-project assignment rejected.
func TestP524CrossProjectRejected(t *testing.T) {
	h := NewHarness(t)
	// A global role and a project-scoped instruction (different scope).
	roleID := seedRole(t, h, nil, "eng-cross", "E")
	pid := roleProjectID(t, createPhaseProject(t, h))
	instrID := seedProjectInstruction(t, h, &pid, "cross-instr", "C", "c")
	code, got := httpJSON(t, http.MethodPost, roleInstrURL(h, "global", roleID), map[string]any{"instruction": instrID})
	if code != http.StatusBadRequest {
		t.Fatalf("cross-project assign = %d, want 400: %v", code, got)
	}
}

// itoaSafe is a small local int->string helper for the order test's seed keys.
func itoaSafe(i int) string {
	return "X" + string(rune('0'+i%10))
}
