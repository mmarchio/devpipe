package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-28 project API integration test (WBS 3.28): the complete user
// configuration workflow driven end-to-end through HTTP only —
// create project → configure settings → add phase → add WBS → check
// readiness → mark READY → START → PAUSE → RESUME → CANCEL — verifying
// each response and the persisted transition history, which must match
// exactly DRAFT → READY → RUNNING → PAUSED → RUNNING → CANCELLED.

// p328ExpectStatus verifies one HTTP step's status code and, for the
// project-shaped responses, the body status field.
func p328ExpectStatus(t *testing.T, step string, status int, out map[string]any, wantStatus int, wantBodyStatus string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("%s: HTTP %d, want %d (body %v)", step, status, wantStatus, out)
	}
	if wantBodyStatus != "" {
		if got, _ := out["status"].(string); got != wantBodyStatus {
			t.Fatalf("%s: body status = %q, want %q", step, got, wantBodyStatus)
		}
	}
}

func TestP328ProjectWorkflow(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	// 1. Create project.
	created := createViaAPI(t, h, "p328 workflow", "end-to-end configuration workflow")
	if created.Status != "DRAFT" {
		t.Fatalf("create: body status = %q, want DRAFT", created.Status)
	}

	// 2. Configure settings (full-state PUT).
	st, out := doJSON(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{"working_directory": t.TempDir()}))
	if st != http.StatusOK {
		t.Fatalf("settings: HTTP %d, want 200 (body %v)", st, out)
	}

	// 3. Add Phase 1.
	st, out = doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Phase 1"}`)
	if st != http.StatusCreated {
		t.Fatalf("phase: HTTP %d, want 201 (body %v)", st, out)
	}
	phaseID, _ := out["id"].(string)
	if phaseID == "" {
		t.Fatalf("phase: 201 body missing id: %v", out)
	}

	// 4. Add WBS document.
	st, out = doJSON(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/phases/"+phaseID+"/wbs", `{"content":"# WBS"}`)
	if st != http.StatusOK {
		t.Fatalf("wbs: HTTP %d, want 200 (body %v)", st, out)
	}
	if out["content"] != "# WBS" {
		t.Fatalf("wbs: body content = %v, want # WBS", out["content"])
	}

	// 5. Check readiness (non-mutating).
	st, out = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID+"/readiness", "")
	if st != http.StatusOK {
		t.Fatalf("readiness: HTTP %d, want 200 (body %v)", st, out)
	}
	if out["ready"] != true {
		t.Fatalf("readiness: ready = %v, want true (checks %v)", out["ready"], out["checks"])
	}

	// 6. Mark READY (response is the readiness report; verify the
	// transition via the stored status).
	st, out = doJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/"+created.ID+"/ready", "")
	if st != http.StatusOK {
		t.Fatalf("ready: HTTP %d, want 200 (body %v)", st, out)
	}
	if out["ready"] != true {
		t.Fatalf("ready: report ready = %v, want true (checks %v)", out["ready"], out["checks"])
	}
	if got := statusOf(t, h, created.ID); got != "READY" {
		t.Fatalf("ready: stored status = %q, want READY", got)
	}

	// 7. START.
	st, out = doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "start"), "")
	p328ExpectStatus(t, "start", st, out, http.StatusOK, "RUNNING")

	// 8. PAUSE.
	st, out = doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "pause"), "")
	p328ExpectStatus(t, "pause", st, out, http.StatusOK, "PAUSED")

	// 9. RESUME.
	st, out = doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "resume"), "")
	p328ExpectStatus(t, "resume", st, out, http.StatusOK, "RUNNING")

	// 10. CANCEL.
	st, out = doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "cancel"), "")
	p328ExpectStatus(t, "cancel", st, out, http.StatusOK, "CANCELLED")

	// Final stored status.
	if got := statusOf(t, h, created.ID); got != "CANCELLED" {
		t.Fatalf("stored status = %q, want CANCELLED", got)
	}

	// Persisted transition history: the exact expected chain, in order,
	// with no extra or missing events (WBS 3.28).
	var got []string
	for {
		var pair string
		if err := h.DBScan(ctx,
			"SELECT from_status || '->' || to_status FROM project_status_events WHERE project_id = '"+created.ID+"' ORDER BY ctid LIMIT 1 OFFSET "+itoa(len(got)),
			&pair); err != nil {
			break
		}
		got = append(got, pair)
	}
	want := []string{"DRAFT->READY", "READY->RUNNING", "RUNNING->PAUSED", "PAUSED->RUNNING", "RUNNING->CANCELLED"}
	if len(got) != len(want) {
		t.Fatalf("persisted transitions = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("persisted transitions[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// The lifecycle events must carry their operation reasons in order.
	var reasons string
	if err := h.DBScan(ctx,
		"SELECT string_agg(reason, ',' ORDER BY ctid) FROM project_status_events WHERE project_id = '"+created.ID+"'",
		&reasons); err != nil {
		t.Fatal(err)
	}
	if reasons != "ready,start,pause,resume,cancel" {
		t.Fatalf("event reasons = %q, want \"ready,start,pause,resume,cancel\"", reasons)
	}
}
