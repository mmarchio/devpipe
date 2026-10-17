package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestP1595API — WBS 15.95–15.106: the REST surface.
func TestP1595API(t *testing.T) {
	r := p15NewRig(t)
	base := r.h.BaseURL + "/api/v1/projects/" + r.pid.String()

	// Settings GET defaults / PUT / 400.
	code, raw, body := p9HTTP(t, http.MethodGet, base+"/settings/execution", "")
	if code != 200 || body["stored"] != false || !strings.Contains(raw, `"dirty_policy":"WARN"`) {
		t.Fatalf("settings %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/execution", `{"dirty_policy":"SOMETIMES"}`); code != 400 {
		t.Fatalf("bad policy %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/execution", `{"max_bootstrap_control_transitions":0}`); code != 400 {
		t.Fatalf("bad limit %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/execution", `{"unknown":1}`); code != 400 {
		t.Fatalf("unknown field %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/execution", `{"dirty_policy":"ALLOW"}`); code != 200 {
		t.Fatalf("put %d %s", code, raw)
	}

	// Readiness + preview do not write (WBS 15.96 / 15.100).
	before := r.dbCounts(t)
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/execution-readiness", "")
	if code != 200 || body["ready"] != true || !strings.Contains(raw, "Ready to Start") {
		t.Fatalf("readiness %d %s", code, raw)
	}
	code, raw, _ = p9HTTP(t, http.MethodGet, base+"/bootstrap-preview", "")
	if code != 200 || !strings.Contains(raw, `"first_actionable_state":"PHASE_PLAN"`) || !strings.Contains(raw, "Missing → planning required") ||
		!strings.Contains(raw, `"role":"PLANNER"`) {
		t.Fatalf("preview %d %s", code, raw)
	}
	if after := r.dbCounts(t); after != before {
		t.Fatal("readiness/preview mutated")
	}
	if code, _, body := p9HTTP(t, http.MethodGet, base+"/active-execution", ""); code != 404 || !strings.Contains(fmtJSON(body), "no_active_execution") {
		t.Fatalf("active none %d", code)
	}

	// Bootstrap (WBS 15.95).
	code, raw, body = p9HTTP(t, http.MethodPost, base+"/bootstrap", "")
	if code != 201 || body["outcome"] != "BOOTSTRAPPED" || body["first_actionable_state"] != "PHASE_PLAN" || body["initial_workflow_state"] != "PROJECT_INIT" {
		t.Fatalf("bootstrap %d %s", code, raw)
	}
	ex := body["execution"].(map[string]any)
	id := ex["id"].(string)
	if ex["status"] != "RUNNING" || ex["number"].(float64) != 1 {
		t.Fatalf("execution %v", ex)
	}
	if code, _, body := p9HTTP(t, http.MethodPost, base+"/bootstrap", ""); code != 200 || body["outcome"] != "ALREADY_RUNNING" {
		t.Fatalf("idempotent %d %v", code, body)
	}

	// Executions / active / get / bootstrap status / configuration / metrics.
	if code, raw, body := p9HTTP(t, http.MethodGet, base+"/executions", ""); code != 200 || body["total"].(float64) != 1 {
		t.Fatalf("list %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodGet, base+"/executions/"+id, ""); code != 200 || !strings.Contains(raw, `"plan_hash"`) {
		t.Fatalf("get %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodGet, base+"/active-execution", ""); code != 200 || !strings.Contains(raw, "Not yet dispatched") || !strings.Contains(raw, "Execution #1") {
		t.Fatalf("active %d %s", code, raw)
	}
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/executions/"+id+"/bootstrap", "")
	if code != 200 || body["stage"] != "COMPLETE" || len(body["history"].([]any)) < 7 {
		t.Fatalf("bootstrap status %d %s", code, raw)
	}
	code, raw, _ = p9HTTP(t, http.MethodGet, base+"/executions/"+id+"/configuration", "")
	if code != 200 || !strings.Contains(raw, "P12_GATEWAY_TOKEN") {
		t.Fatalf("configuration %d %s", code, raw)
	}
	for _, secret := range []string{p12Token, p9SentinelKey, "pguser", "password"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("configuration exposes %q", secret)
		}
	}
	if code, raw, body := p9HTTP(t, http.MethodGet, base+"/executions/metrics", ""); code != 200 || body["bootstrap_successes"].(float64) != 1 {
		t.Fatalf("metrics %d %s", code, raw)
	}

	// 400 / 404 / 405 / isolation.
	if code, _, _ := p9HTTP(t, http.MethodGet, base+"/executions/not-a-uuid", ""); code != 404 { // path-id convention
		t.Fatalf("bad id %d", code)
	}
	if code, _, _ := p9HTTP(t, http.MethodGet, base+"/executions?limit=-1", ""); code != 400 {
		t.Fatalf("bad paging %d", code)
	}
	if code, _, _ := p9HTTP(t, http.MethodGet, base+"/executions/"+uuid.NewString(), ""); code != 404 {
		t.Fatalf("unknown id %d", code)
	}
	other := p15Build(t, r.h, r.g, r.srv, p15DefaultPhases)
	if code, _, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+other.pid.String()+"/executions/"+id, ""); code != 404 {
		t.Fatalf("cross-project %d", code)
	}
	if code, _, _ := p9HTTP(t, http.MethodDelete, base+"/bootstrap", ""); code != 405 {
		t.Fatalf("405 %d", code)
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, r.h.BaseURL+"/api/v1/projects/"+uuid.NewString()+"/bootstrap", ""); code != 404 {
		t.Fatalf("unknown project %d", code)
	}

	// 422 readiness failure with the report; 409 not ready.
	other.exec(t, "UPDATE project_settings SET working_directory = '/nonexistent/x' WHERE "+other.where())
	ob := r.h.BaseURL + "/api/v1/projects/" + other.pid.String()
	code, raw, body = p9HTTP(t, http.MethodPost, ob+"/bootstrap", "")
	if code != 422 || !strings.Contains(raw, "WORKDIR_INVALID") || !strings.Contains(raw, "Cannot Start Project") {
		t.Fatalf("422 %d %s", code, raw)
	}
	other.exec(t, "UPDATE projects SET status = 'DRAFT' WHERE id = '"+other.pid.String()+"'")
	if code, raw, _ := p9HTTP(t, http.MethodPost, ob+"/bootstrap", ""); code != 409 || !strings.Contains(raw, "PROJECT_NOT_READY") {
		t.Fatalf("409 %d %s", code, raw)
	}
}

func fmtJSON(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.TrimSpace(strings.ReplaceAll(fmtAnyValue(v), "\n", " ")))
		b.WriteString(";")
	}
	return b.String()
}

func fmtAnyValue(v any) string {
	if m, ok := v.(map[string]any); ok {
		return fmtJSON(m)
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestP1591DBGuards — WBS 15.4 / 15.5 / 15.3.3 / 15.40 / 15.10 / 15.80 /
// 15.149: PostgreSQL rejects a second active execution, a duplicate number,
// binding changes and mutation of immutable bootstrap records.
func TestP1591DBGuards(t *testing.T) {
	r := p15NewRig(t)
	ctx := context.Background()
	e := r.bootstrap(t).Execution
	id := e.ID.String()
	mustFail := func(name, sql string) {
		t.Helper()
		if _, err := r.h.DBExec(ctx, sql); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	mustFail("second active", "INSERT INTO executions (id, project_id, number, status, state_machine_id, state_machine_version, state_machine_hash, "+
		"initial_phase_id, wbs_version, plan_state, initial_state_key, plan_hash, bootstrap_stage, started_at, created_at, updated_at) "+
		"SELECT gen_random_uuid(), project_id, 99, 'INITIALIZING', state_machine_id, state_machine_version, state_machine_hash, initial_phase_id, "+
		"wbs_version, plan_state, initial_state_key, plan_hash, 'SNAPSHOTTING', now(), now(), now() FROM executions WHERE id = '"+id+"'")
	r.exec(t, "UPDATE executions SET status = 'COMPLETED' WHERE id = '"+id+"'")
	mustFail("duplicate number", "INSERT INTO executions (id, project_id, number, status, state_machine_id, state_machine_version, state_machine_hash, "+
		"initial_phase_id, wbs_version, plan_state, initial_state_key, plan_hash, bootstrap_stage, started_at, created_at, updated_at) "+
		"SELECT gen_random_uuid(), project_id, 1, 'INITIALIZING', state_machine_id, state_machine_version, state_machine_hash, initial_phase_id, "+
		"wbs_version, plan_state, initial_state_key, plan_hash, 'SNAPSHOTTING', now(), now(), now() FROM executions WHERE id = '"+id+"'")
	mustFail("reopen", "UPDATE executions SET status = 'RUNNING' WHERE id = '"+id+"'")
	mustFail("binding", "UPDATE executions SET state_machine_version = 7 WHERE id = '"+id+"'")
	mustFail("number", "UPDATE executions SET number = 5 WHERE id = '"+id+"'")
	mustFail("snapshot ref", "UPDATE executions SET configuration_snapshot_id = NULL WHERE id = '"+id+"'")
	mustFail("snapshot", "UPDATE execution_configuration_snapshots SET hash = repeat('0', 64) WHERE execution_id = '"+id+"'")
	mustFail("stage history", "UPDATE execution_bootstrap_transitions SET reason = 'x' WHERE execution_id = '"+id+"'")
	mustFail("baseline", "UPDATE execution_repository_baselines SET dirty = NOT dirty WHERE execution_id = '"+id+"'")
	mustFail("checkpoint", "UPDATE execution_bootstrap_checkpoints SET checkpoint_hash = repeat('0', 64) WHERE execution_id = '"+id+"'")
	mustFail("events", "UPDATE execution_events SET type = 'EXECUTION_STARTED' WHERE execution_id = '"+id+"'")
	mustFail("bootstrap from-null", "INSERT INTO state_transition_history (id, project_id, state_machine_id, state_machine_version, from_state_id, to_state_id, outcome, runtime_version, seq) "+
		"SELECT gen_random_uuid(), project_id, state_machine_id, state_machine_version, NULL, to_state_id, 'PROCEED', 1, 9999 FROM state_transition_history WHERE execution_id = '"+id+"' LIMIT 1")
}
