package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-22 — project listing filters + deterministic ordering (WBS 3.22).

func createNProjects(t *testing.T, h *Harness, n int, prefix string) []projectAPIBody {
	t.Helper()
	var out []projectAPIBody
	for i := 0; i < n; i++ {
		out = append(out, createViaAPI(t, h, prefix+itoa(i), "desc "+prefix+itoa(i)))
	}
	return out
}

// T3.22.1 — Filter by status: only matching projects are returned.
func TestP322_1FilterByStatus(t *testing.T) {
	h := NewHarness(t)
	running := createViaAPI(t, h, "p322-run", "")
	draft := createViaAPI(t, h, "p322-draft", "")
	if _, err := h.DBExec(context.Background(), `UPDATE projects SET status = 'RUNNING' WHERE id = '`+running.ID+`'`); err != nil {
		t.Fatal(err)
	}
	_ = draft

	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects?status=RUNNING", "")
	if status != http.StatusOK {
		t.Fatalf("GET ?status=RUNNING = %d, want 200", status)
	}
	projects := out["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("filtered list length = %d, want 1", len(projects))
	}
	if id, _ := projects[0].(map[string]any)["id"].(string); id != running.ID {
		t.Fatalf("filtered list id = %q, want %q", id, running.ID)
	}

	status, out = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects?status=DRAFT", "")
	if status != http.StatusOK {
		t.Fatalf("GET ?status=DRAFT = %d, want 200", status)
	}
	projects = out["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("draft filter length = %d, want 1", len(projects))
	}
	if id, _ := projects[0].(map[string]any)["id"].(string); id != draft.ID {
		t.Fatalf("draft filter id = %q, want %q", id, draft.ID)
	}
}

// T3.22.2 — Unknown status rejected: 400, empty collection, not 500.
func TestP322_2UnknownStatusRejected(t *testing.T) {
	h := NewHarness(t)
	createViaAPI(t, h, "p322-x", "")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects?status=NOT_A_STATUS", "")
	if status != http.StatusBadRequest {
		t.Fatalf("GET ?status=NOT_A_STATUS = %d, want 400", status)
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("400 body missing error field: %v", out)
	}
}

// T3.22.3 — Pagination stable: limit caps the page, repeated identical
// requests return identical results, and pages are disjoint and exhaustive.
func TestP322_3PaginationStable(t *testing.T) {
	h := NewHarness(t)
	createNProjects(t, h, 5, "p322-stable")

	fetchPage := func(qs string) []string {
		status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects"+qs, "")
		if status != http.StatusOK {
			t.Fatalf("GET /api/v1/projects%s = %d, want 200", qs, status)
		}
		projects := out["projects"].([]any)
		ids := make([]string, 0, len(projects))
		for _, p := range projects {
			ids = append(ids, p.(map[string]any)["id"].(string))
		}
		return ids
	}

	page1a := fetchPage("?limit=3")
	page1b := fetchPage("?limit=3")
	if len(page1a) != 3 {
		t.Fatalf("limit=3 returned %d items, want 3", len(page1a))
	}
	for i := range page1a {
		if page1a[i] != page1b[i] {
			t.Fatalf("repeated page differs at %d: %q vs %q — not stable", i, page1a[i], page1b[i])
		}
	}
	page2 := fetchPage("?limit=3&offset=3")
	if len(page2) != 2 {
		t.Fatalf("offset=3&limit=3 returned %d items, want 2", len(page2))
	}
	for _, id := range page1a {
		for _, id2 := range page2 {
			if id == id2 {
				t.Fatalf("pages overlap at %q — pagination not stable", id)
			}
		}
	}
}

// T3.22.4 — Empty result returns empty collection (200 with [] not null).
func TestP322_4EmptyResult(t *testing.T) {
	h := NewHarness(t)
	createViaAPI(t, h, "p322-empty", "")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects?status=CANCELLED", "")
	if status != http.StatusOK {
		t.Fatalf("GET ?status=CANCELLED (no matches) = %d, want 200", status)
	}
	projects, ok := out["projects"].([]any)
	if !ok {
		t.Fatalf("body missing projects array: %v", out)
	}
	if len(projects) != 0 {
		t.Fatalf("empty filter returned %d projects, want 0", len(projects))
	}
}

// T3.22.5 — Default ordering deterministic: updated_at DESC (most recently
// updated first) with a stable tiebreak.
func TestP322_5DefaultOrdering(t *testing.T) {
	h := NewHarness(t)
	a := createViaAPI(t, h, "p322-old", "")
	b := createViaAPI(t, h, "p322-new", "")
	// Touch the newer project so it becomes the most recently updated —
	// under the old created_at ASC ordering the older project would lead,
	// so this pins the updated_at DESC contract.
	if _, err := h.DBExec(context.Background(), `UPDATE projects SET description = 'touched' WHERE id = '`+b.ID+`'`); err != nil {
		t.Fatal(err)
	}
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects", "")
	if status != http.StatusOK {
		t.Fatalf("GET list = %d, want 200", status)
	}
	projects := out["projects"].([]any)
	if len(projects) < 2 {
		t.Fatalf("list length = %d, want >= 2", len(projects))
	}
	firstID, _ := projects[0].(map[string]any)["id"].(string)
	if firstID != b.ID {
		t.Fatalf("first listed id = %q, want most-recently-updated %q (updated_at DESC)", firstID, b.ID)
	}
	if secondID, _ := projects[1].(map[string]any)["id"].(string); secondID != a.ID {
		t.Fatalf("second listed id = %q, want %q", secondID, a.ID)
	}
}
