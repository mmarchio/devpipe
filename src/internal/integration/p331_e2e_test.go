package integration

import (
	"net/http"
	"testing"
)

// P3-31 Phase 3 end-to-end acceptance (WBS 3.31): a user can fully prepare an
// overseen project without any LLM interaction — create → configure
// (working dir, worker model, compression model, context limits) → add phase
// → add WBS → validate readiness → READY → START → RUNNING — with nothing
// dispatched to an LLM. The proof of "no dispatch" is that START is
// lifecycle-only (P3-08): seeded work units remain in their pre-START
// statuses (a dispatch would have moved one to an executing/dispatched
// state), so the assertion is non-vacuous.

func TestP331EndToEndAcceptanceNoLLM(t *testing.T) {
	h := NewHarness(t)
	base := h.BaseURL

	// 1. Create project.
	created := createViaAPI(t, h, "p331 acceptance", "")
	if created.Status != "DRAFT" {
		t.Fatalf("create: body status = %q, want DRAFT", created.Status)
	}

	// 2. Configure project (working directory, worker model, compression
	// model, context limits) via settings PUT.
	dir := t.TempDir()
	st, out := doJSON(t, http.MethodPut, base+"/api/v1/projects/"+created.ID+"/settings",
		p319Settings(map[string]any{
			"working_directory": dir,
			"worker_model":      "gpt-5",
			"compression_model": "llama-compress",
			"context_window":    128000,
			"context_threshold": 0.75,
		}))
	if st != http.StatusOK {
		t.Fatalf("configure: HTTP %d, want 200 (body %v)", st, out)
	}

	// 3. Add phase.
	st, out = doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Foundation"}`)
	if st != http.StatusCreated {
		t.Fatalf("add phase: HTTP %d, want 201", st)
	}
	phaseID, _ := out["id"].(string)

	// 4. Add WBS.
	st, out = doJSON(t, http.MethodPut,
		base+"/api/v1/projects/"+created.ID+"/phases/"+phaseID+"/wbs", `{"content":"# Foundation WBS\n- unit 1\n- unit 2"}`)
	if st != http.StatusOK {
		t.Fatalf("add wbs: HTTP %d, want 200", st)
	}

	// Seed work units so the "no dispatch" assertion is non-vacuous: a START
	// that dispatched to an LLM would have flipped one to an executing state.
	seedPlanUnits(t, h, phaseID)

	// 5. Validate readiness (non-mutating) then mark READY.
	st, out = doJSON(t, http.MethodGet, base+"/api/v1/projects/"+created.ID+"/readiness", "")
	if st != http.StatusOK {
		t.Fatalf("readiness: HTTP %d, want 200", st)
	}
	if out["ready"] != true {
		t.Fatalf("readiness: ready = %v, want true (checks %v)", out["ready"], out["checks"])
	}
	st, out = doJSON(t, http.MethodPost, base+"/api/v1/projects/"+created.ID+"/ready", "")
	if st != http.StatusOK {
		t.Fatalf("mark ready: HTTP %d, want 200", st)
	}
	if got := statusOf(t, h, created.ID); got != "READY" {
		t.Fatalf("after ready: stored status = %q, want READY", got)
	}

	// 6. START → RUNNING.
	st, out = doJSON(t, http.MethodPost, lifecycleURL(t, h, created.ID, "start"), "")
	if st != http.StatusOK {
		t.Fatalf("start: HTTP %d, want 200 (body %v)", st, out)
	}
	if got := statusOf(t, h, created.ID); got != "RUNNING" {
		t.Fatalf("after start: stored status = %q, want RUNNING", got)
	}

	// No LLM dispatched: START is lifecycle-only (P3-08). The seeded work
	// units must remain in their pre-START statuses — a dispatch would have
	// moved at least one to an executing/dispatched state.
	var pending, done, executing int32
	if err := h.DBScan(t.Context(),
		"SELECT count(*) FROM work_units wu JOIN execution_plans p ON p.id = wu.execution_plan_id "+
			"WHERE p.phase_id = '"+phaseID+"' AND wu.status = 'PENDING'", &pending); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(t.Context(),
		"SELECT count(*) FROM work_units wu JOIN execution_plans p ON p.id = wu.execution_plan_id "+
			"WHERE p.phase_id = '"+phaseID+"' AND wu.status = 'DONE'", &done); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(t.Context(),
		"SELECT count(*) FROM work_units wu JOIN execution_plans p ON p.id = wu.execution_plan_id "+
			"WHERE p.phase_id = '"+phaseID+"' AND wu.status <> 'PENDING' AND wu.status <> 'DONE'", &executing); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || done != 2 {
		t.Fatalf("work units = pending %d / done %d, want 1/2 (unchanged by START)", pending, done)
	}
	if executing != 0 {
		t.Fatalf("START dispatched to an LLM: %d work unit(s) moved to a non-PENDING/DONE status", executing)
	}
}
