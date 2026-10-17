package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

// TestP16API — the Phase 16 REST surface (P16-138/139): settings, step,
// run, lifecycle, recovery resolution, timeline, steps, validations,
// checkpoints, fix attempts, summary, hardening; 400 / 404 / 405 / 409.
func TestP16API(t *testing.T) {
	r := p16NewRig(t)
	base := r.h.BaseURL + "/api/v1/projects/" + r.pid.String()
	ex := base + "/executions/" + r.execID.String()

	code, raw, body := p9HTTP(t, http.MethodGet, base+"/settings/engine", "")
	if code != 200 || body["stored"] != true || !strings.Contains(raw, `"max_fix_attempts":3`) || !strings.Contains(raw, `"final_hardening"`) {
		t.Fatalf("settings %d %s", code, raw)
	}
	for _, bad := range []string{`{"checkpoint_policy":"SOMETIMES"}`, `{"max_fix_attempts":0}`, `{"nope":1}`,
		`{"unit_validation":[{"program":"sh -c 'x'"}]}`, `{"final_hardening":{"duplicate_functions":{"max_rounds":0}}}`} {
		if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/engine", bad); code != 400 {
			t.Fatalf("bad settings %s → %d %s", bad, code, raw)
		}
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/engine", `{"max_fix_attempts":5}`); code != 200 || !strings.Contains(raw, `"max_fix_attempts":5`) {
		t.Fatalf("put %d %s", code, raw)
	}

	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	if code, raw, body := p9HTTP(t, http.MethodPost, ex+"/step", ""); code != 200 || !strings.Contains(raw, `"PLAN_DISPATCH"`) {
		t.Fatalf("step %d %s %v", code, raw, body)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/run?budget=0", ""); code != 400 {
		t.Fatalf("budget %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/pause", ""); code != 200 || !strings.Contains(raw, `"status":"PAUSED"`) {
		t.Fatalf("pause %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/pause", ""); code != 200 || !strings.Contains(raw, `"status":"PAUSED"`) {
		t.Fatalf("idempotent pause %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/resume", ""); code != 200 || !strings.Contains(raw, `"status":"RUNNING"`) {
		t.Fatalf("resume %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/recovery/resolve", `{"action":"RESUME"}`); code != 409 {
		t.Fatalf("resolve on running %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/recovery/resolve", `{}`); code != 400 {
		t.Fatalf("resolve without action %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/run?budget=200", ""); code != 200 || !strings.Contains(raw, `"COMPLETED"`) {
		t.Fatalf("run %d %s", code, raw)
	}
	for path, want := range map[string]string{
		"/timeline": `"PROJECT_COMPLETED"`, "/steps": `"FINAL_CHECKPOINT"`, "/validations": `"PASS"`,
		"/checkpoints": `"CREATED"`, "/fix-attempts": `"count":0`, "/summary": `"work_units_completed":1`,
	} {
		code, raw, _ := p9HTTP(t, http.MethodGet, ex+path, "")
		if code != 200 || !strings.Contains(raw, want) {
			t.Fatalf("%s %d %s", path, code, raw)
		}
	}
	if code, raw, _ := p9HTTP(t, http.MethodGet, ex+"/summary", ""); !strings.Contains(raw, `"phases_completed":1`) || !strings.Contains(raw, `"completion"`) || code != 200 {
		t.Fatalf("summary %s", raw)
	}
	// Hardening disabled: no pass.
	if code, _, _ := p9HTTP(t, http.MethodGet, ex+"/hardening", ""); code != 404 {
		t.Fatalf("hardening %d", code)
	}
	// Completed: further steps report COMPLETED, lifecycle ops conflict.
	if code, raw, _ := p9HTTP(t, http.MethodPost, ex+"/step", ""); code != 200 || !strings.Contains(raw, `"COMPLETED"`) {
		t.Fatalf("step completed %d %s", code, raw)
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, ex+"/resume", ""); code != 409 {
		t.Fatal("resume completed project")
	}
	other := base + "/executions/" + uuid.NewString()
	for _, p := range []string{"/step", "/timeline", "/summary"} {
		m := http.MethodGet
		if p == "/step" {
			m = http.MethodPost
		}
		if code, _, _ := p9HTTP(t, m, other+p, ""); code != 404 {
			t.Fatalf("unknown execution %s → %d", p, code)
		}
	}
	if code, _, _ := p9HTTP(t, http.MethodDelete, ex+"/step", ""); code != 405 {
		t.Fatalf("DELETE step %d", code)
	}
	_ = execution.StepCompleted
	_ = context.Background
}

// TestP16DBGuards — PostgreSQL enforces the Phase 16 invariants
// (P16-54 … P16-63).
func TestP16DBGuards(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 100)
	ctx := context.Background()
	ex := r.execID.String()
	rejects := map[string]string{
		"engine settings CHECK":           "UPDATE project_engine_settings SET checkpoint_policy = 'SOMETIMES' WHERE " + r.where(),
		"step immutable":                  "UPDATE execution_steps SET outcome = 'x' WHERE execution_id = '" + ex + "' AND status = 'APPLIED'",
		"step key unique":                 "INSERT INTO execution_steps (id, execution_id, project_id, seq, action, state_key, runtime_version, idempotency_key, status, intent_at) SELECT gen_random_uuid(), execution_id, project_id, seq + 1000, action, state_key, runtime_version, idempotency_key, 'INTENT', now() FROM execution_steps WHERE execution_id = '" + ex + "' LIMIT 1",
		"event type CHECK":                "INSERT INTO execution_events (id, project_id, execution_id, type, created_at) VALUES (gen_random_uuid(), '" + r.pid.String() + "', '" + ex + "', 'NOPE', now())",
		"event sequence unique":           "INSERT INTO execution_events (id, project_id, execution_id, type, created_at, exec_seq) VALUES (gen_random_uuid(), '" + r.pid.String() + "', '" + ex + "', 'PHASE_DONE', now(), 1)",
		"DONE requires validation":        "UPDATE work_units SET done_validation_run_id = NULL WHERE " + r.where() + " AND claimed_execution_id IS NOT NULL",
		"validation run immutable":        "UPDATE validation_runs SET status = 'FAIL' WHERE execution_id = '" + ex + "'",
		"validation result immutable":     "UPDATE validation_command_results SET exit_code = 9",
		"checkpoint immutable":            "UPDATE repository_checkpoints SET error = 'x' WHERE execution_id = '" + ex + "'",
		"checkpoint key unique":           "INSERT INTO repository_checkpoints (id, project_id, execution_id, kind, checkpoint_key, status, created_at) SELECT gen_random_uuid(), project_id, execution_id, kind, checkpoint_key, 'PENDING', now() FROM repository_checkpoints WHERE execution_id = '" + ex + "' LIMIT 1",
		"candidate decided immutable":     "UPDATE plan_candidates SET status = 'REJECTED' WHERE execution_id = '" + ex + "'",
		"completed execution immutable":   "UPDATE executions SET wait_reason = 'x' WHERE id = '" + ex + "'",
		"completion immutable":            "UPDATE execution_completions SET completed_at = now() WHERE execution_id = '" + ex + "'",
		"second completion":               "INSERT INTO execution_completions (execution_id, project_id, completed_at) VALUES ('" + ex + "', '" + r.pid.String() + "', now())",
		"phase DONE with open plan units": "",
	}
	for name, sql := range rejects {
		if sql == "" {
			continue
		}
		tag, err := r.h.DBExec(ctx, sql)
		if err == nil && tag.RowsAffected() > 0 {
			t.Fatalf("%s: accepted (%d rows)", name, tag.RowsAffected())
		}
		if err == nil {
			t.Fatalf("%s: no row matched (test setup)", name)
		}
	}
	// Phase DONE trigger: a pending phase with a current plan holding a
	// non-DONE unit cannot become DONE.
	p2 := seedPhaseStatus(t, r.h, r.pid, 9, "Guarded", "PENDING")
	r.phases[9] = p2
	plan := r.plan(t, 9, 1, "EXECUTING")
	r.unit(t, plan, 1, "open", "PENDING")
	if _, err := r.h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+p2.String()+"'"); err == nil || !strings.Contains(err.Error(), "non-DONE") {
		t.Fatalf("phase DONE guard: %v", err)
	}
	// Hardening pass: one per execution generation.
	r.exec(t, "INSERT INTO hardening_passes (id, project_id, execution_id, generation, status, started_at, updated_at) VALUES (gen_random_uuid(), '"+r.pid.String()+"', '"+ex+"', 7, 'PENDING', now(), now())")
	if _, err := r.h.DBExec(ctx, "INSERT INTO hardening_passes (id, project_id, execution_id, generation, status, started_at, updated_at) VALUES (gen_random_uuid(), '"+r.pid.String()+"', '"+ex+"', 7, 'PENDING', now(), now())"); err == nil {
		t.Fatal("duplicate hardening pass for one generation")
	}
}
