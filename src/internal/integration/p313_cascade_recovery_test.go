package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
)

// T3.13.6 — deleting an executed project removes every project-owned row,
// including the immutable recovery plans/actions; the audit log is kept.
func TestT3136DeleteExecutedProjectCascades(t *testing.T) {
	r := p17NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true }})
	ctx := context.Background()
	pid, eid := r.pid.String(), r.execID.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18CheckpointRejected(t, r.dir, true)
	r.runUntil(t, execution.StepRecoveryRequired, 100)
	base := "/api/v1/projects/" + pid + "/executions/" + eid + "/recovery"
	var plan struct {
		CurrentHash string `json:"current_hash"`
	}
	r.b.getJSON(base+"/plan", &plan)
	p18CheckpointRejected(t, r.dir, false)
	if code, raw := r.b.api(http.MethodPost, base+"/actions", map[string]any{"action": "RETRY_CHECKPOINT", "reason": "lock removed",
		"idempotency_key": "k-delete-cascade-1", "evidence_hash": plan.CurrentHash}); code != 200 {
		t.Fatalf("recovery action: %d %s", code, raw)
	}
	if r.count(t, "SELECT count(*) FROM recovery_actions WHERE project_id = '"+pid+"'") == 0 ||
		r.count(t, "SELECT count(*) FROM recovery_plans WHERE project_id = '"+pid+"'") == 0 {
		t.Fatal("seed: no recovery history recorded")
	}
	// Recovery history stays immutable while the project exists.
	if _, err := r.h.DBExec(ctx, "DELETE FROM recovery_actions WHERE project_id = '"+pid+"'"); err == nil {
		t.Fatal("recovery action deleted while its project exists")
	}
	audit := r.count(t, "SELECT count(*) FROM audit_events WHERE project_id = '"+pid+"'")

	// RUNNING/PAUSED are protected; a terminal project is deletable.
	if _, err := r.h.DBExec(ctx, "UPDATE projects SET status = 'RUNNING' WHERE id = '"+pid+"'"); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.b.api(http.MethodDelete, "/api/v1/projects/"+pid, nil); code != http.StatusConflict {
		t.Fatalf("delete RUNNING: %d, want 409", code)
	}
	if _, err := r.h.DBExec(ctx, "UPDATE projects SET status = 'FAILED' WHERE id = '"+pid+"'"); err != nil {
		t.Fatal(err)
	}
	if code, raw := r.b.api(http.MethodDelete, "/api/v1/projects/"+pid, nil); code != http.StatusNoContent {
		t.Fatalf("delete FAILED: %d %s", code, raw)
	}

	rows, err := r.h.App.DB().Raw().Query(ctx, `SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'project_id' AND table_name NOT IN ('audit_events')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tbl := range append(tables, "projects") {
		col := "project_id"
		if tbl == "projects" {
			col = "id"
		}
		if n := r.count(t, "SELECT count(*) FROM "+tbl+" WHERE "+col+"::text = '"+pid+"'"); n != 0 {
			t.Errorf("%s: %d rows left for the deleted project", tbl, n)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE id = '"+eid+"'"); n != 0 {
		t.Errorf("execution left behind")
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE project_id = '"+pid+"'"); n < audit {
		t.Errorf("audit events lost: %d -> %d", audit, n)
	}
}
