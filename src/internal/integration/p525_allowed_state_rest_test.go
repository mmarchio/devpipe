package integration

import (
	"context"
	"net/http"
	"sort"
	"testing"
)

// allowedStatesURL builds the allowed-state configuration REST base for a role.
func allowedStatesURL(h *Harness, scope, roleID string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles/" + roleID + "/states"
}

// fetchAllowedStates reads the role's allowed states from the DB directly.
func fetchAllowedStates(t *testing.T, h *Harness, roleID string) []string {
	t.Helper()
	ctx := context.Background()
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT state_key FROM role_allowed_states WHERE role_id = $1", parseUUID(t, roleID))
	if err != nil {
		t.Fatalf("query states: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// T5.25.1 — States assigned: PUT stores the given states.
func TestP525StatesAssigned(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "st-assign", "S")

	code, got := httpJSON(t, http.MethodPut, allowedStatesURL(h, "global", roleID),
		map[string]any{"states": []any{"implementation", "continuation"}})
	if code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %v", code, got)
	}
	gotStates, _ := got["states"].([]any)
	if len(gotStates) != 2 {
		t.Fatalf("returned states = %d, want 2: %v", len(gotStates), got)
	}
	stored := fetchAllowedStates(t, h, roleID)
	if len(stored) != 2 || stored[0] != "continuation" || stored[1] != "implementation" {
		t.Fatalf("stored states = %v, want [continuation implementation]", stored)
	}
}

// T5.25.2 — Existing assignments replaced atomically: a second PUT overwrites
// the first set entirely (no residue from the prior set).
func TestP525ReplacedAtomically(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "st-replace", "S")

	if code, _ := httpJSON(t, http.MethodPut, allowedStatesURL(h, "global", roleID),
		map[string]any{"states": []any{"implementation", "continuation"}}); code != http.StatusOK {
		t.Fatalf("first put = %d", code)
	}
	// Replace with a disjoint set.
	if code, _ := httpJSON(t, http.MethodPut, allowedStatesURL(h, "global", roleID),
		map[string]any{"states": []any{"review", "done"}}); code != http.StatusOK {
		t.Fatalf("second put = %d", code)
	}
	stored := fetchAllowedStates(t, h, roleID)
	if len(stored) != 2 || stored[0] != "done" || stored[1] != "review" {
		t.Fatalf("stored states after replace = %v, want [done review]", stored)
	}
}

// T5.25.3 — Duplicate states normalized: duplicates in the payload collapse
// to a single assignment (the set is normalized, order-insensitive).
func TestP525DuplicatesNormalized(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "st-dup", "S")

	code, got := httpJSON(t, http.MethodPut, allowedStatesURL(h, "global", roleID),
		map[string]any{"states": []any{"implementation", "implementation", "continuation"}})
	if code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %v", code, got)
	}
	stored := fetchAllowedStates(t, h, roleID)
	if len(stored) != 2 || stored[0] != "continuation" || stored[1] != "implementation" {
		t.Fatalf("stored states = %v, want [continuation implementation] (duplicates normalized)", stored)
	}
}

// T5.25.4 — Empty required assignment rejected: a PUT with an empty states
// list is rejected as invalid input.
func TestP525EmptyRejected(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "st-empty", "S")

	code, got := httpJSON(t, http.MethodPut, allowedStatesURL(h, "global", roleID),
		map[string]any{"states": []any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("empty put = %d, want 400: %v", code, got)
	}
}
