package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// With MVP selected, the orchestrator works on the plan's MVP tasks only:
// the FULL task is never loaded, the phase completes with it PENDING
// (the phase-done guard allows a deferred FULL task), and the project
// completes.
func TestP21MVPScopeSkipsFullTasks(t *testing.T) {
	r := p16NewRigWith(t, p16Options{automated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	team.fullTask = true
	r.g.Responder = team.respond
	ctx := context.Background()
	if _, err := r.h.DBExec(ctx, "INSERT INTO project_task_scope (project_id, scope) VALUES ('"+r.pid.String()+"', 'MVP')"); err != nil {
		t.Fatal(err)
	}

	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{"create the output-format skill", "plan phase 1", "implement task A", "implement task B"}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.phaseStatus(t, 1); st != "DONE" {
		t.Fatalf("phase 1 %s", st)
	}
	var scope, status string
	if err := r.h.DBScan(ctx, "SELECT w.scope, w.status FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id "+
		"WHERE w.unit_key = 'F' AND p.phase_id = '"+r.phases[1].String()+"'", &scope, &status); err != nil {
		t.Fatal(err)
	}
	if scope != "FULL" || status != "PENDING" {
		t.Fatalf("task F: scope %s status %s", scope, status)
	}
}

// With MVP and FULL selected (no choice recorded), the FULL task runs too.
func TestP21FullScopeRunsEveryTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{automated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	team.fullTask = true
	r.g.Responder = team.respond

	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{"create the output-format skill", "plan phase 1", "implement task A", "implement task B", "implement task F"}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
}
