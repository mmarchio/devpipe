package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// P4-24 WBS Diff endpoint (WBS 4.17, endpoint half of T4.17.1-T4.17.5):
// GET /api/v1/projects/{project}/phases/{phase}/wbs/diff?from=N&to=M
// returns the deterministic P4-23 Diff between two stored WBS versions.
// Invalid version pairs and malformed query params are rejected.

func wbsDiffURL(t *testing.T, h *Harness, projectID, phaseID, from, to string) string {
	t.Helper()
	return wbsURL(t, h, projectID, phaseID) + "/diff?from=" + from + "&to=" + to
}

// diffHunk is the web-layer diff hunk shape as decoded from JSON.
type diffHunk struct {
	Operation string   `json:"operation"`
	FromLines []string `json:"from_lines"`
	ToLines   []string `json:"to_lines"`
}

// diffHunksFrom decodes the response's hunks field into the stable
// web-layer shape; the field must be an array (identical pairs are an
// empty array, never null).
func diffHunksFrom(out map[string]any) ([]diffHunk, error) {
	raw, err := json.Marshal(out["hunks"])
	if err != nil {
		return nil, err
	}
	var hunks []diffHunk
	if err := json.Unmarshal(raw, &hunks); err != nil {
		return nil, err
	}
	return hunks, nil
}

// findHunk returns the hunk with the given operation, or nil.
func findHunk(hunks []diffHunk, op string) *diffHunk {
	for i := range hunks {
		if hunks[i].Operation == op {
			return &hunks[i]
		}
	}
	return nil
}

// T4.17.1 — Added content identified: a version pair that appends a line
// yields an "added" hunk whose to_lines carry exactly the added line and
// whose from_lines are empty.
func TestT4171AddedContentIdentified(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p424 added")
	for _, c := range []string{"alpha\nbeta", "alpha\nbeta\ngamma"} {
		if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
			wbsPutBody(t, c)); status != http.StatusOK {
			t.Fatalf("PUT WBS = %d, want 200", status)
		}
	}

	status, out := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseID, "1", "2"), "")
	if status != http.StatusOK {
		t.Fatalf("GET diff = %d, want 200", status)
	}
	if out["from"] != float64(1) || out["to"] != float64(2) {
		t.Fatalf("response from/to = %v/%v, want 1/2", out["from"], out["to"])
	}
	hunks, err := diffHunksFrom(out)
	if err != nil {
		t.Fatalf("response hunks: %v", err)
	}
	added := findHunk(hunks, "added")
	if added == nil {
		t.Fatalf("no added hunk in %v, want one carrying gamma", hunks)
	}
	if len(added.ToLines) != 1 || added.ToLines[0] != "gamma" {
		t.Fatalf("added hunk to_lines = %v, want [gamma]", added.ToLines)
	}
	if len(added.FromLines) != 0 {
		t.Fatalf("added hunk from_lines = %v, want empty", added.FromLines)
	}
}

// T4.17.2 — Removed content identified: the pair that removes a line
// yields a "removed" hunk whose from_lines carry exactly the removed line.
func TestT4172RemovedContentIdentified(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p424 removed")
	for _, c := range []string{"alpha\nbeta", "alpha"} {
		if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
			wbsPutBody(t, c)); status != http.StatusOK {
			t.Fatalf("PUT WBS = %d, want 200", status)
		}
	}

	status, out := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseID, "1", "2"), "")
	if status != http.StatusOK {
		t.Fatalf("GET diff = %d, want 200", status)
	}
	hunks, err := diffHunksFrom(out)
	if err != nil {
		t.Fatalf("response hunks: %v", err)
	}
	removed := findHunk(hunks, "removed")
	if removed == nil {
		t.Fatalf("no removed hunk in %v, want one carrying beta", hunks)
	}
	if len(removed.FromLines) != 1 || removed.FromLines[0] != "beta" {
		t.Fatalf("removed hunk from_lines = %v, want [beta]", removed.FromLines)
	}
	if len(removed.ToLines) != 0 {
		t.Fatalf("removed hunk to_lines = %v, want empty", removed.ToLines)
	}
}

// T4.17.3 — Modified content represented: a replaced line pair yields a
// "modified" hunk carrying the old line on from_lines and the new line on
// to_lines (adjacent removed+added collapse into one hunk, P4-23 primitive).
func TestT4173ModifiedContentRepresented(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p424 modified")
	for _, c := range []string{"one\ntwo\nthree", "one\ntwo\nthree-b"} {
		if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
			wbsPutBody(t, c)); status != http.StatusOK {
			t.Fatalf("PUT WBS = %d, want 200", status)
		}
	}

	status, out := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseID, "1", "2"), "")
	if status != http.StatusOK {
		t.Fatalf("GET diff = %d, want 200", status)
	}
	hunks, err := diffHunksFrom(out)
	if err != nil {
		t.Fatalf("response hunks: %v", err)
	}
	modified := findHunk(hunks, "modified")
	if modified == nil {
		t.Fatalf("no modified hunk in %v, want one pairing three/three-b", hunks)
	}
	if len(modified.FromLines) != 1 || modified.FromLines[0] != "three" {
		t.Fatalf("modified hunk from_lines = %v, want [three]", modified.FromLines)
	}
	if len(modified.ToLines) != 1 || modified.ToLines[0] != "three-b" {
		t.Fatalf("modified hunk to_lines = %v, want [three-b]", modified.ToLines)
	}
}

// T4.17.4 — Same versions produce an empty diff: from == to yields an
// empty hunks array (present, never null).
func TestT4174SameVersionsEmptyDiff(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsVersionSetup(t, h, "p424 same")

	status, out := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseID, "2", "2"), "")
	if status != http.StatusOK {
		t.Fatalf("GET diff 2..2 = %d, want 200", status)
	}
	hunks, err := diffHunksFrom(out)
	if err != nil {
		t.Fatalf("response hunks: %v", err)
	}
	if len(hunks) != 0 {
		t.Fatalf("identical pair produced %d hunks, want 0", len(hunks))
	}
}

// T4.17.5 (endpoint half) — Invalid version pairs and malformed query
// params are rejected: unknown version 404; malformed/missing/non-positive
// params 400; cross-project pair 404; unknown project 404.
func TestT4175InvalidVersionRejected(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsVersionSetup(t, h, "p424 invalid")

	cases := []struct {
		name string
		from string
		to   string
		want int
	}{
		{"unknown from", "99", "3", http.StatusNotFound},
		{"unknown to", "1", "99", http.StatusNotFound},
		{"malformed from", "abc", "3", http.StatusBadRequest},
		{"malformed to", "1", "xy", http.StatusBadRequest},
		{"missing from", "", "3", http.StatusBadRequest},
		{"missing to", "1", "", http.StatusBadRequest},
		{"non-positive from", "0", "3", http.StatusBadRequest},
		{"negative to", "1", "-1", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseID, tc.from, tc.to), "")
			if status != tc.want {
				t.Fatalf("from=%q to=%q = %d, want %d", tc.from, tc.to, status, tc.want)
			}
		})
	}

	// Cross-project version pair: phase belongs to another project.
	projectB, phaseB := wbsVersionSetup(t, h, "p424 invalid cross")
	status, _ := doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectB, phaseID, "1", "2"), "")
	if status != http.StatusNotFound {
		t.Fatalf("cross-project diff (projectB/phaseA) = %d, want 404", status)
	}
	status, _ = doJSON(t, http.MethodGet, wbsDiffURL(t, h, projectID, phaseB, "1", "2"), "")
	if status != http.StatusNotFound {
		t.Fatalf("cross-project diff (projectA/phaseB) = %d, want 404", status)
	}

	// Unknown project (valid uuid, no such row).
	unknownProject := "00000000-0000-0000-0000-000000000000"
	status, _ = doJSON(t, http.MethodGet, wbsDiffURL(t, h, unknownProject, phaseID, "1", "2"), "")
	if status != http.StatusNotFound {
		t.Fatalf("unknown project diff = %d, want 404", status)
	}
}
