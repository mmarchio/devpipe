package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-29 phase addition during a running project (WBS 3.29, T3.29.1-T3.29.5):
// with a project RUNNING and existing phases in place, appending a new phase
// via the API must succeed, receive order max+1, leave the existing phases
// untouched, and not displace the current phase. This is a validation test
// over the existing P3-16 append endpoint (no new production code).

func TestP329AppendDuringRunningProject(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	// Fixture: project with phases {1 Foundation DONE, 2 Persistence
	// EXECUTING, 3 API PENDING}, project RUNNING. The three phases are created
	// via the API (which assigns orders 1/2/3) and their statuses are seeded
	// by direct SQL, since a test op cannot reach the RUNNING/DONE/EXECUTING
	// combination on its own.
	created := createViaAPI(t, h, "p329 running append", "")
	ids := make([]string, 3)
	for i, name := range []string{"Foundation", "Persistence", "API"} {
		st, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"`+name+`"}`)
		if st != http.StatusCreated {
			t.Fatalf("seed POST %s = %d, want 201", name, st)
		}
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("seed POST %s: 201 body missing id: %v", name, out)
		}
		ids[i] = id
	}
	// Order 1 Foundation DONE, order 2 Persistence EXECUTING, order 3 API PENDING.
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'DONE', completed_at = now() WHERE id = '"+ids[0]+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+ids[1]+"'"); err != nil {
		t.Fatal(err)
	}
	// ids[2] ("API") stays PENDING.
	if _, err := h.DBExec(ctx,
		"UPDATE projects SET status = 'RUNNING' WHERE id = '"+created.ID+"'"); err != nil {
		t.Fatal(err)
	}

	// Append "Security Hardening" through the API while RUNNING.
	st, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Security Hardening"}`)

	// T3.29.1 — Request succeeds while project RUNNING.
	if st != http.StatusCreated {
		t.Fatalf("T3.29.1: append while RUNNING = %d, want 201", st)
	}

	// T3.29.2 — New phase receives max + 1 (order 4).
	if out["order"] != float64(4) {
		t.Fatalf("T3.29.2: appended order = %v, want 4 (max+1)", out["order"])
	}
	var newOrder int
	if err := h.DBScan(ctx,
		"SELECT phase_order FROM phases WHERE project_id = '"+created.ID+"' AND name = 'Security Hardening'", &newOrder); err != nil {
		t.Fatal(err)
	}
	if newOrder != 4 {
		t.Fatalf("T3.29.2: stored order = %d, want 4", newOrder)
	}

	// T3.29.3 — Existing orders unchanged (1/2/3 with their names intact).
	st, list := doJSON(t, http.MethodGet, phasesURL(t, h, created.ID), "")
	if st != http.StatusOK {
		t.Fatalf("T3.29.3: GET list = %d, want 200", st)
	}
	phases := list["phases"].([]any)
	if len(phases) != 4 {
		t.Fatalf("T3.29.3: list = %d phases, want 4", len(phases))
	}
	type gotPhase struct {
		name  string
		order int
	}
	var got []gotPhase
	for _, item := range phases {
		m := item.(map[string]any)
		got = append(got, gotPhase{name: m["name"].(string), order: int(m["order"].(float64))})
	}
	want := []gotPhase{
		{"Foundation", 1},
		{"Persistence", 2},
		{"API", 3},
		{"Security Hardening", 4},
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].order != want[i].order {
			t.Fatalf("T3.29.3: list = %v, want %v", got, want)
		}
	}

	// T3.29.4 — Current phase remains Phase 2 (the next incomplete phase is
	// unchanged by the append). Verified through the readiness/read-model rule
	// (GetNextIncomplete) via the summary endpoint, cross-checked with direct SQL.
	st, sum := doJSON(t, http.MethodGet,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/summary", "")
	if st != http.StatusOK {
		t.Fatalf("T3.29.4: GET summary = %d, want 200", st)
	}
	ph, _ := sum["phase"].(map[string]any)
	if ph == nil {
		t.Fatalf("T3.29.4: summary phase missing: %+v", sum)
	}
	if ph["id"] != ids[1] || ph["name"] != "Persistence" || ph["order"] != float64(2) {
		t.Fatalf("T3.29.4: summary current phase = %+v, want Persistence (order 2)", ph)
	}
	var curOrder int32
	var curName string
	if err := h.DBScan(ctx,
		"SELECT phase_order, name FROM phases WHERE project_id = '"+created.ID+"' AND status <> 'DONE' ORDER BY phase_order ASC LIMIT 1",
		&curOrder, &curName); err != nil {
		t.Fatal(err)
	}
	if curOrder != 2 || curName != "Persistence" {
		t.Fatalf("T3.29.4: next incomplete = %d %q, want order 2 Persistence", curOrder, curName)
	}

	// T3.29.5 — New phase status PENDING.
	var newStatus string
	if err := h.DBScan(ctx,
		"SELECT status FROM phases WHERE project_id = '"+created.ID+"' AND name = 'Security Hardening'", &newStatus); err != nil {
		t.Fatal(err)
	}
	if newStatus != "PENDING" {
		t.Fatalf("T3.29.5: new phase status = %q, want PENDING", newStatus)
	}
}
