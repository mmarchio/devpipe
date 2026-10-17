package integration

import (
	"context"
	"net/http"
	"testing"
)

// stateFilesURL builds the state-file requirement REST base for a role.
func stateFilesURL(h *Harness, scope, roleID string) string {
	return h.BaseURL + "/api/v1/projects/" + scope + "/roles/" + roleID + "/state-files"
}

// fetchStateFiles reads the role's required state files from the DB directly,
// in position order (order is meaningful — unlike allowed states).
func fetchStateFiles(t *testing.T, h *Harness, roleID string) []string {
	t.Helper()
	ctx := context.Background()
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx,
		"SELECT state_file_key FROM role_state_files WHERE role_id = $1 ORDER BY position ASC", parseUUID(t, roleID))
	if err != nil {
		t.Fatalf("query state files: %v", err)
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
	return out
}

// T5.26.1 — Requirements stored: PUT stores the given state files.
func TestP526RequirementsStored(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "sf-store", "S")

	code, got := httpJSON(t, http.MethodPut, stateFilesURL(h, "global", roleID),
		map[string]any{"state_files": []any{"ACTIVE_TASK", "PROJECT_STATE"}})
	if code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %v", code, got)
	}
	gotFiles, _ := got["state_files"].([]any)
	if len(gotFiles) != 2 {
		t.Fatalf("returned state_files = %d, want 2: %v", len(gotFiles), got)
	}
	stored := fetchStateFiles(t, h, roleID)
	if len(stored) != 2 || stored[0] != "ACTIVE_TASK" || stored[1] != "PROJECT_STATE" {
		t.Fatalf("stored = %v, want [ACTIVE_TASK PROJECT_STATE]", stored)
	}
}

// T5.26.2 — Ordering preserved: the stored order matches the input order
// (a non-alphabetical permutation is retained exactly).
func TestP526OrderingPreserved(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "sf-order", "S")

	code, _ := httpJSON(t, http.MethodPut, stateFilesURL(h, "global", roleID),
		map[string]any{"state_files": []any{"ZULU", "ALPHA", "MIKE"}})
	if code != http.StatusOK {
		t.Fatalf("put = %d, want 200", code)
	}
	stored := fetchStateFiles(t, h, roleID)
	if len(stored) != 3 || stored[0] != "ZULU" || stored[1] != "ALPHA" || stored[2] != "MIKE" {
		t.Fatalf("stored = %v, want [ZULU ALPHA MIKE] (input order)", stored)
	}
}

// T5.26.3 — Duplicate requirements rejected: a duplicate key in the input is
// rejected as invalid input. Unlike allowed states (normalized), state-file
// order is meaningful, so duplicates are rejected, not collapsed.
func TestP526DuplicatesRejected(t *testing.T) {
	h := NewHarness(t)
	roleID := seedRole(t, h, nil, "sf-dup", "S")

	code, got := httpJSON(t, http.MethodPut, stateFilesURL(h, "global", roleID),
		map[string]any{"state_files": []any{"ACTIVE_TASK", "PROJECT_STATE", "ACTIVE_TASK"}})
	if code != http.StatusBadRequest {
		t.Fatalf("dup put = %d, want 400: %v", code, got)
	}
	stored := fetchStateFiles(t, h, roleID)
	if len(stored) != 0 {
		t.Fatalf("stored after rejected dup = %v, want empty", stored)
	}
}

// T5.26.4 — Unknown role rejected: a PUT for an unknown role is 404.
func TestP526UnknownRoleRejected(t *testing.T) {
	h := NewHarness(t)

	code, _ := httpJSON(t, http.MethodPut, stateFilesURL(h, "global", "00000000-0000-0000-0000-000000000001"),
		map[string]any{"state_files": []any{"ACTIVE_TASK"}})
	if code != http.StatusNotFound {
		t.Fatalf("unknown-role put = %d, want 404", code)
	}
}
