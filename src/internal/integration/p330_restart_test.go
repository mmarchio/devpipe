package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// P3-30 restart persistence (WBS 3.30): a project driven through
// READY -> RUNNING -> PAUSED must reload with its status, settings, phases,
// WBS, phase order, progress, and lifecycle history intact after a stop/
// restart cycle. No lifecycle information may depend solely on process
// memory. The test boots TWO harness instances on the SAME persisted
// database (the restart/reconstruction pattern) and compares a field-level
// snapshot before and after, not row counts or spot checks.

// p330Snapshot captures every answer the acceptance criteria must verify,
// read through the running instance's endpoints and direct SQL.
type p330Snapshot struct {
	Status   string
	Settings map[string]any
	Phases   []map[string]any
	WBS      string
	Progress map[string]any
	History  []string
}

func p330Capture(t *testing.T, h *Harness, id, phaseID string) *p330Snapshot {
	t.Helper()
	ctx := context.Background()
	s := &p330Snapshot{}

	s.Status = statusOf(t, h, id)

	st, settings := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+id+"/settings", "")
	if st != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200", st)
	}
	s.Settings = settings

	st, list := doJSON(t, http.MethodGet, phasesURL(t, h, id), "")
	if st != http.StatusOK {
		t.Fatalf("GET phases = %d, want 200", st)
	}
	for _, item := range list["phases"].([]any) {
		m := item.(map[string]any)
		s.Phases = append(s.Phases, map[string]any{
			"id": m["id"], "order": m["order"], "name": m["name"], "status": m["status"],
		})
	}

	st, wbsOut := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+id+"/phases/"+phaseID+"/wbs", "")
	if st != http.StatusOK {
		t.Fatalf("GET WBS = %d, want 200", st)
	}
	s.WBS, _ = wbsOut["content"].(string)

	st, sum := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+id+"/summary", "")
	if st != http.StatusOK {
		t.Fatalf("GET summary = %d, want 200", st)
	}
	prog, _ := sum["progress"].(map[string]any)
	s.Progress = map[string]any{
		"completed":  prog["completed"],
		"total":      prog["total"],
		"percentage": prog["percentage"],
	}

	for {
		var pair string
		if err := h.DBScan(ctx,
			"SELECT from_status || '->' || to_status FROM project_status_events WHERE project_id = '"+id+"' ORDER BY ctid LIMIT 1 OFFSET "+itoa(len(s.History)),
			&pair); err != nil {
			break
		}
		s.History = append(s.History, pair)
	}
	return s
}

func TestP330RestartPersistence(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	// First boot (migrates), drive the project through the full lifecycle.
	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}

	created := createViaAPI(t, h1, "p330 restart", "")
	dir := t.TempDir()
	st, _ := doJSON(t, http.MethodPut, h1.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{"working_directory": dir}))
	if st != http.StatusOK {
		t.Fatalf("settings: HTTP %d, want 200", st)
	}
	st, out := doJSON(t, http.MethodPost, phasesURL(t, h1, created.ID), `{"name":"Phase 1"}`)
	if st != http.StatusCreated {
		t.Fatalf("phase: HTTP %d, want 201", st)
	}
	phaseID, _ := out["id"].(string)
	st, _ = doJSON(t, http.MethodPut,
		h1.BaseURL+"/api/v1/projects/"+created.ID+"/phases/"+phaseID+"/wbs", `{"content":"# P330 WBS"}`)
	if st != http.StatusOK {
		t.Fatalf("wbs: HTTP %d, want 200", st)
	}
	// Seed a plan + work units so progress is non-trivial and comparable
	// across the restart (progress is derived from persisted unit status).
	seedPlanUnits(t, h1, phaseID)

	st, _ = doJSON(t, http.MethodPost, h1.BaseURL+"/api/v1/projects/"+created.ID+"/ready", "")
	if st != http.StatusOK {
		t.Fatalf("ready: HTTP %d, want 200", st)
	}
	st, _ = doJSON(t, http.MethodPost, lifecycleURL(t, h1, created.ID, "start"), "")
	if st != http.StatusOK {
		t.Fatalf("start: HTTP %d, want 200", st)
	}
	st, _ = doJSON(t, http.MethodPost, lifecycleURL(t, h1, created.ID, "pause"), "")
	if st != http.StatusOK {
		t.Fatalf("pause: HTTP %d, want 200", st)
	}
	if got := statusOf(t, h1, created.ID); got != "PAUSED" {
		t.Fatalf("pre-restart status = %q, want PAUSED", got)
	}

	before := p330Capture(t, h1, created.ID, phaseID)
	h1.Close()

	// Second boot against the SAME persisted database.
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	// NewHarnessOn registers no cleanup; close h2 so its pool releases the
	// DB connections before the t.Cleanup dropDB runs (registered first, so
	// it runs last under LIFO).
	t.Cleanup(h2.Close)
	after := p330Capture(t, h2, created.ID, phaseID)

	// Field-by-field comparison.
	if after.Status != before.Status || after.Status != "PAUSED" {
		t.Fatalf("status = %q, want PAUSED (pre-restart %q)", after.Status, before.Status)
	}
	if len(after.Settings) != len(before.Settings) {
		t.Fatalf("settings fields differ: after=%d before=%d", len(after.Settings), len(before.Settings))
	}
	for k, v := range before.Settings {
		if after.Settings[k] != v {
			t.Fatalf("settings[%s] = %v, want %v", k, after.Settings[k], v)
		}
	}
	if len(after.Phases) != len(before.Phases) {
		t.Fatalf("phase count = %d, want %d", len(after.Phases), len(before.Phases))
	}
	for i := range before.Phases {
		for k, v := range before.Phases[i] {
			if after.Phases[i][k] != v {
				t.Fatalf("phase[%d][%s] = %v, want %v", i, k, after.Phases[i][k], v)
			}
		}
	}
	if after.WBS != before.WBS {
		t.Fatalf("WBS = %q, want %q", after.WBS, before.WBS)
	}
	for k, v := range before.Progress {
		if after.Progress[k] != v {
			t.Fatalf("progress[%s] = %v, want %v", k, after.Progress[k], v)
		}
	}
	if len(after.History) != len(before.History) {
		t.Fatalf("lifecycle history = %v, want %v", after.History, before.History)
	}
	wantHistory := []string{"DRAFT->READY", "READY->RUNNING", "RUNNING->PAUSED"}
	if len(after.History) != len(wantHistory) {
		t.Fatalf("lifecycle history = %v, want exactly %v", after.History, wantHistory)
	}
	for i := range wantHistory {
		if after.History[i] != wantHistory[i] {
			t.Fatalf("history[%d] = %q, want %q (full %v)", i, after.History[i], wantHistory[i], after.History)
		}
	}
}

// seedPlanUnits creates an execution plan for the phase plus three work units
// (two DONE, one PENDING) directly, so progress is non-trivial and comparable
// across the restart.
func seedPlanUnits(t *testing.T, h *Harness, phaseID string) {
	t.Helper()
	ctx := context.Background()
	sql := "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES (gen_random_uuid(), '" +
		phaseID + "', 1, 1, 'READY');" +
		"INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES" +
		"(gen_random_uuid(), (SELECT id FROM execution_plans WHERE phase_id = '" + phaseID + "'), 1, 'u1', '', 'ATOMIC', 'DONE', '', '', '[]', '[]', '[]', now())," +
		"(gen_random_uuid(), (SELECT id FROM execution_plans WHERE phase_id = '" + phaseID + "'), 2, 'u2', '', 'ATOMIC', 'DONE', '', '', '[]', '[]', '[]', now())," +
		"(gen_random_uuid(), (SELECT id FROM execution_plans WHERE phase_id = '" + phaseID + "'), 3, 'u3', '', 'ATOMIC', 'PENDING', '', '', '[]', '[]', '[]', now())"
	if _, err := h.DBExec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}
