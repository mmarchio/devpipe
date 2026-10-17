package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"overseer/internal/service"
)

// P4-11 (WBS 4.12): optimistic concurrency on WBS updates. An update
// declares the version it was made against; a match against the current
// version proceeds to the next version, while a stale declaration is
// rejected with ErrConflict (HTTP 409) and the stale content is never
// persisted.

// wbsPutVersionedBody is the strict WBS PUT payload carrying the
// expected-version declaration (WBS 4.12).
func wbsPutVersionedBody(t *testing.T, content string, expectedVersion int) string {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return `{"content":` + string(raw) + `,"expected_version":` + itoa(expectedVersion) + `}`
}

// wbsBumpToVersion drives the service from a freshly created WBS (v1) up
// to the target version via versioned updates.
func wbsBumpToVersion(t *testing.T, h *Harness, svc *service.WBSDocumentService, projectID, phaseID string, target int) string {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	content := "v1"
	for v := 2; v <= target; v++ {
		content = "v" + itoa(v)
		if _, err := svc.UpdateWBS(ctx, projectID, phaseID, content, v-1); err != nil {
			t.Fatalf("UpdateWBS to v%d: %v", v, err)
		}
	}
	return content
}

// T4.12.1 — Update against current version succeeds: the WBS is at v4, an
// update declaring v4 is accepted with no error.
func TestT4121UpdateAgainstCurrentVersionSucceeds(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)
	wbsBumpToVersion(t, h, svc, projectID, phaseID, 4)

	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "v5", 4); err != nil {
		t.Fatalf("UpdateWBS declaring current version 4: %v, want success", err)
	}
}

// T4.12.2 — Update creates next version: the accepted update appends the
// next version (v4 -> v5) and the current version advances.
func TestT4122UpdateCreatesNextVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)
	wbsBumpToVersion(t, h, svc, projectID, phaseID, 4)

	rec, err := svc.UpdateWBS(ctx, projectID, phaseID, "v5", 4)
	if err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	if rec.Version != 5 {
		t.Fatalf("update result version = %d, want 5", rec.Version)
	}
	cur, err := svc.GetWBS(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	if cur.Version != 5 || cur.Content != "v5" {
		t.Fatalf("current = v%d %q, want v5 v5", cur.Version, cur.Content)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("stored versions = %d, want 5 (append-only)", count)
	}
}

// T4.12.3 — Stale update rejected: client B declares v4 after the WBS is
// at v5 and receives ErrConflict.
func TestT4123StaleUpdateRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)
	wbsBumpToVersion(t, h, svc, projectID, phaseID, 5)

	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "stale edit", 4); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("stale update: got %v, want ErrConflict", err)
	}
}

// T4.12.4 — Stale content not persisted: after the rejected update the
// version history and current content are exactly the pre-attempt state.
func TestT4124StaleContentNotPersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)
	wbsBumpToVersion(t, h, svc, projectID, phaseID, 5)

	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "stale edit", 4); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("stale update: got %v, want ErrConflict", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("stale update persisted %d versions, want 5 (unmutated)", count)
	}
	var content string
	if err := h.DBScan(ctx, "SELECT content FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND version = 5", &content); err != nil {
		t.Fatal(err)
	}
	if content != "v5" {
		t.Fatalf("v5 content = %q, want the pre-attempt content v5", content)
	}
	cur, err := svc.GetWBS(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	if cur.Version != 5 || cur.Content != "v5" {
		t.Fatalf("current = v%d %q, want v5 v5 (stale content never stored)", cur.Version, cur.Content)
	}
}

// T4.12.5 — HTTP returns 409 conflict: a stale expected_version on the WBS
// PUT maps to 409, while a matching version succeeds with 200.
func TestT4125HTTPReturns409Conflict(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsSetup(t, h, "p412 http")
	svc := newVersionedWBSDocService(t, h)
	wbsBumpToVersion(t, h, svc, projectID, phaseID, 2)

	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutVersionedBody(t, "stale http edit", 1)); status != http.StatusConflict {
		t.Fatalf("PUT with stale expected_version = %d, want 409", status)
	}
	status, out := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutVersionedBody(t, "fresh http edit", 2))
	if status != http.StatusOK {
		t.Fatalf("PUT with current expected_version = %d, want 200", status)
	}
	if out["version"] != float64(3) {
		t.Fatalf("response version = %v, want 3", out["version"])
	}
	var staleCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND content = 'stale http edit'", &staleCount); err != nil {
		t.Fatal(err)
	}
	if staleCount != 0 {
		t.Fatalf("stale HTTP edit persisted %d rows, want 0", staleCount)
	}
}
