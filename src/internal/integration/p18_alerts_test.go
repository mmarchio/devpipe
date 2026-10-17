package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/health"
)

func p18Alert(alerts []health.Alert, id health.AlertID) health.Alert {
	for _, a := range alerts {
		if a.ID == id {
			return a
		}
	}
	return health.Alert{}
}

// T18.118 — alerts activate and clear deterministically: the same facts
// keep the same activation time and log once; a cleared condition clears
// the alert (logged).
func TestT18118AlertsActivateAndClear(t *testing.T) {
	logs := captureLogs(t)
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_MISSING", 1)
	ctx := context.Background()
	svc := r.h.App.Alerts()
	first := p18Alert(svc.Evaluate(ctx), health.AlertRecoveryRequired)
	if !first.Active || !strings.Contains(first.Detail, r.execID.String()) || first.Since.IsZero() {
		t.Fatalf("RECOVERY_REQUIRED: %+v", first)
	}
	second := p18Alert(svc.Evaluate(ctx), health.AlertRecoveryRequired)
	if !second.Since.Equal(first.Since) {
		t.Fatal("re-evaluation moved the activation time")
	}
	if !p18LogLine(logs.String(), "alert activated", "RECOVERY_REQUIRED") {
		t.Fatalf("activation not logged:\n%s", logs.String())
	}
	activations := strings.Count(logs.String(), "alert activated")
	r.exec(t, "UPDATE executions SET status = 'PAUSED' WHERE id = '"+r.execID.String()+"'")
	if a := p18Alert(svc.Evaluate(ctx), health.AlertRecoveryRequired); a.Active {
		t.Fatalf("not cleared: %+v", a)
	}
	if !strings.Contains(logs.String(), "alert cleared") || strings.Count(logs.String(), "alert activated") != activations {
		t.Fatal("clearing not logged once")
	}
	if code, raw := newBrowser(t, r.h.BaseURL).api("GET", "/api/v1/system/alerts", nil); code != 200 || !strings.Contains(string(raw), `"SCHEDULER_STALLED"`) {
		t.Fatalf("alerts API %d %s", code, raw)
	}
}

// T18.119 / T18.120 — a WAITING execution (on the worker) is never
// "stalled"; a RUNNING one without progress past the threshold is; a
// runnable execution whose remaining units can never become eligible is
// deadlocked.
func TestT18119StallAndDeadlock(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.StallThreshold = time.Second }})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	ctx := context.Background()
	svc := r.h.App.Alerts()
	exec := r.execID.String()
	backdate := "SET session_replication_role = replica; UPDATE execution_steps SET intent_at = intent_at - interval '1 hour', finished_at = finished_at - interval '1 hour' WHERE execution_id = '" + exec + "'; SET session_replication_role = origin"
	r.exec(t, backdate)
	r.exec(t, "SET session_replication_role = replica; UPDATE executions SET status = 'WAITING', updated_at = now() - interval '1 hour' WHERE id = '"+exec+"'; SET session_replication_role = origin")
	if a := p18Alert(svc.Evaluate(ctx), health.AlertSchedulerStalled); a.Active {
		t.Fatalf("WAITING execution reported stalled: %+v", a)
	}
	r.exec(t, "SET session_replication_role = replica; UPDATE executions SET status = 'RUNNING', updated_at = now() - interval '1 hour' WHERE id = '"+exec+"'; SET session_replication_role = origin")
	if a := p18Alert(svc.Evaluate(ctx), health.AlertSchedulerStalled); !a.Active || !strings.Contains(a.Detail, exec) {
		t.Fatalf("stall not detected: %+v", a)
	}

	// Deadlock: unit a FAILED, b depends on it; nothing in progress.
	pid := r.pid.String()
	r.exec(t, `SET session_replication_role = replica;
		UPDATE work_units SET status = 'FAILED' WHERE project_id = '`+pid+`' AND unit_key = 'a';
		UPDATE work_units SET status = 'PENDING' WHERE project_id = '`+pid+`' AND unit_key = 'b';
		UPDATE execution_plans SET status = 'EXECUTING' WHERE id = (SELECT execution_plan_id FROM work_units WHERE project_id = '`+pid+`' AND unit_key = 'a');
		UPDATE executions SET current_work_unit_id = NULL,
			current_phase_id = (SELECT p.phase_id FROM execution_plans p JOIN work_units w ON w.execution_plan_id = p.id WHERE w.project_id = '`+pid+`' AND w.unit_key = 'a')
			WHERE id = '`+exec+`';
		SET session_replication_role = origin`)
	if a := p18Alert(svc.Evaluate(ctx), health.AlertExecutionDeadlocked); !a.Active || !strings.Contains(a.Detail, exec) {
		t.Fatalf("deadlock not detected: %+v", a)
	}
	r.exec(t, "SET session_replication_role = replica; UPDATE work_units SET status = 'DONE' WHERE project_id = '"+pid+"' AND unit_key = 'a'; SET session_replication_role = origin")
	if a := p18Alert(svc.Evaluate(ctx), health.AlertExecutionDeadlocked); a.Active {
		t.Fatalf("an eligible unit is not a deadlock: %+v", a)
	}
}

// p18LogLine reports whether one log line contains every part.
func p18LogLine(logs string, parts ...string) bool {
	for _, l := range strings.Split(logs, "\n") {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return true
		}
	}
	return false
}
