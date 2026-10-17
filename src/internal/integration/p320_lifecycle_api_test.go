package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-20 lifecycle command endpoints (WBS 3.20, T3.20.1-T3.20.7):
// POST /api/v1/projects/{id}/start|pause|resume|cancel. Invalid transitions
// return 409; a direct status PATCH is rejected; events are persisted.

func lifecycleURL(t *testing.T, h *Harness, projectID, op string) string {
	t.Helper()
	return h.BaseURL + "/api/v1/projects/" + projectID + "/" + op
}

func setStatus(t *testing.T, h *Harness, projectID, status string) {
	t.Helper()
	if _, err := h.DBExec(context.Background(),
		"UPDATE projects SET status = '"+status+"' WHERE id = '"+projectID+"'"); err != nil {
		t.Fatal(err)
	}
}

func statusOf(t *testing.T, h *Harness, projectID string) string {
	t.Helper()
	var s string
	if err := h.DBScan(context.Background(),
		"SELECT status FROM projects WHERE id = '"+projectID+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func eventCount(t *testing.T, h *Harness, projectID string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+projectID+"'", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T3.20.1 — Start endpoint: READY → RUNNING.
func TestT3201Start(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p320 start", "")
	setStatus(t, h, created.ID, "READY")
	status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "start"), "")
	if status != http.StatusOK {
		t.Fatalf("POST /start = %d, want 200", status)
	}
	if got := statusOf(t, h, created.ID); got != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING", got)
	}
}

// T3.20.2 — Pause endpoint: RUNNING → PAUSED.
func TestT3202Pause(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p320 pause", "")
	setStatus(t, h, created.ID, "RUNNING")
	status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "pause"), "")
	if status != http.StatusOK {
		t.Fatalf("POST /pause = %d, want 200", status)
	}
	if got := statusOf(t, h, created.ID); got != "PAUSED" {
		t.Fatalf("status = %q, want PAUSED", got)
	}
}

// T3.20.3 — Resume endpoint: PAUSED → RUNNING.
func TestT3203Resume(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p320 resume", "")
	setStatus(t, h, created.ID, "PAUSED")
	status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "resume"), "")
	if status != http.StatusOK {
		t.Fatalf("POST /resume = %d, want 200", status)
	}
	if got := statusOf(t, h, created.ID); got != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING", got)
	}
}

// T3.20.4 — Cancel endpoint: RUNNING and PAUSED → CANCELLED.
func TestT3204Cancel(t *testing.T) {
	for _, from := range []string{"RUNNING", "PAUSED"} {
		t.Run(from, func(t *testing.T) {
			h := NewHarness(t)
			created := createViaAPI(t, h, "p320 cancel", "")
			setStatus(t, h, created.ID, from)
			status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "cancel"), "")
			if status != http.StatusOK {
				t.Fatalf("POST /cancel from %s = %d, want 200", from, status)
			}
			if got := statusOf(t, h, created.ID); got != "CANCELLED" {
				t.Fatalf("status = %q, want CANCELLED", got)
			}
		})
	}
}

// T3.20.5 — Invalid transition returns 409 with status unchanged.
func TestT3205InvalidTransition409(t *testing.T) {
	cases := []struct{ op, from string }{
		{"start", "DRAFT"},
		{"start", "PAUSED"}, // resume, not start
		{"pause", "DRAFT"},
		{"resume", "RUNNING"},
		{"cancel", "DRAFT"},
	}
	for _, c := range cases {
		t.Run(c.op+"_"+c.from, func(t *testing.T) {
			h := NewHarness(t)
			created := createViaAPI(t, h, "p320 invalid", "")
			if c.from != "DRAFT" {
				setStatus(t, h, created.ID, c.from)
			}
			status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, c.op), "")
			if status != http.StatusConflict {
				t.Fatalf("POST /%s from %s = %d, want 409", c.op, c.from, status)
			}
			if got := statusOf(t, h, created.ID); got != c.from {
				t.Fatalf("status = %q, want %q (unchanged)", got, c.from)
			}
		})
	}
}

// T3.20.6 — Direct arbitrary status mutation rejected.
func TestT3206StatusPatchRejected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p320 patch", "")
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+created.ID, `{"status":"RUNNING"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH {status} = %d, want 400", status)
	}
	if got := statusOf(t, h, created.ID); got != "DRAFT" {
		t.Fatalf("status = %q, want DRAFT (unchanged)", got)
	}
}

// T3.20.7 — Lifecycle commands persist transition event.
func TestT3207EventPersisted(t *testing.T) {
	cases := []struct {
		op, from, to, reason string
	}{
		{"start", "READY", "RUNNING", "start"},
		{"pause", "RUNNING", "PAUSED", "pause"},
		{"resume", "PAUSED", "RUNNING", "resume"},
		{"cancel", "RUNNING", "CANCELLED", "cancel"},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			h := NewHarness(t)
			created := createViaAPI(t, h, "p320 event", "")
			setStatus(t, h, created.ID, c.from)
			if eventCount(t, h, created.ID) != 0 {
				t.Fatalf("pre-event count != 0")
			}
			if status, _ := doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, c.op), ""); status != http.StatusOK {
				t.Fatalf("POST /%s = %d, want 200", c.op, status)
			}
			if n := eventCount(t, h, created.ID); n != 1 {
				t.Fatalf("event count = %d, want 1", n)
			}
			var reason, fromStatus, toStatus string
			if err := h.DBScan(context.Background(),
				"SELECT reason, from_status, to_status FROM project_status_events WHERE project_id = '"+created.ID+"'",
				&reason, &fromStatus, &toStatus); err != nil {
				t.Fatal(err)
			}
			if reason != c.reason || fromStatus != c.from || toStatus != c.to {
				t.Fatalf("event = %s %s->%s, want %s %s->%s", reason, fromStatus, toStatus, c.reason, c.from, c.to)
			}
		})
	}
}
