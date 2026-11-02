package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// The default statefile workflow (v4) tests and accepts every task in the
// overseer: no test or acceptance command is sent. An implemented task
// goes straight to the overseer's validation; a failure sends it back to
// the engineer, a pass is accepted, checkpointed and archived, and the
// next task is loaded.
func TestP19AutomatedAcceptance(t *testing.T) {
	r := p16NewRigWith(t, p16Options{automated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	team.failFirstImplement = "A"
	r.g.Responder = team.respond
	ctx := context.Background()

	guide, err := os.ReadFile(filepath.Join(r.dir, "statefiles", "HERMES.md"))
	if err != nil || !strings.Contains(string(guide), "The overseer tests and accepts every task itself") ||
		strings.Contains(string(guide), "test task <KEY>") || strings.Contains(string(guide), "acceptance tasks") {
		t.Fatalf("HERMES.md: %s %v", guide, err)
	}

	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"implement task A", // fails the overseer's validation
		"implement task A", // passes → accepted, B loaded
		"implement task B", // passes → accepted, phase complete
	}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if seq := r.statesSeq(t); strings.Contains(seq, "TEST") || strings.Contains(seq, "→ACCEPTANCE→") ||
		!strings.Contains(seq, "IMPLEMENT_CHECK→VALIDATE→IMPLEMENT") || !strings.Contains(seq, "VALIDATE→ACCEPTANCE_CHECK→IMPLEMENT") {
		t.Fatalf("states %s", seq)
	}
	// Accepted by the overseer: DONE and archived without a dispatch,
	// one checkpoint each, and no team test or acceptance result.
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND "+
		"w.archived_at IS NOT NULL AND w.archived_by_dispatch_id IS NULL AND p.phase_id = '"+r.phases[1].String()+"'"); n != 2 {
		t.Fatalf("%d units DONE and archived by the overseer, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'CREATED' AND "+r.where()); n != 2 {
		t.Fatalf("%d unit checkpoints, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM task_reports WHERE kind IN ('TEST', 'ACCEPTANCE') AND execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d team test/acceptance results", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'FIX_ATTEMPT' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d fix attempts, want 1", n)
	}
	assertNoStatefiles(t, r.dir)
}

// No one judges a MANUAL criterion in the v4 workflow: a plan declaring
// one is rejected with every such criterion named, and the corrected plan
// runs.
func TestP19AutomatedAcceptanceRefusesManualCriteria(t *testing.T) {
	r := p16NewRigWith(t, p16Options{automated: true})
	team := newSFTeam(t, r)
	team.noManual, team.manualFirstPlan = true, true
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	team.mu.Lock()
	plans := append([]string(nil), team.plans...)
	team.mu.Unlock()
	if len(plans) != 2 || strings.Contains(plans[0], "MANUAL") || !strings.Contains(plans[0], "verification: COMMAND|TEST,") {
		t.Fatalf("plan commands %q", plans)
	}
	for _, key := range []string{"A", "B"} {
		if !strings.Contains(plans[1], "- "+key+" MANUAL_ACCEPTANCE_CHECK: acceptance check 2: MANUAL verification is not allowed") {
			t.Fatalf("retried plan command does not name %s's MANUAL criterion: %q", key, plans[1])
		}
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM acceptance_approvals"); n != 0 {
		t.Fatalf("%d approvals recorded", n)
	}
}

// A requeue in the v4 workflow returns to ACCEPTANCE_CHECK, which reloads
// the task; it restarts from IMPLEMENT without a fix attempt.
func TestP19AutomatedAcceptanceRequeue(t *testing.T) {
	r := p16NewRigWith(t, p16Options{automated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	r.g.Responder = team.respond
	ctx := context.Background()

	for i := 0; i < 50 && r.state(t) != "VALIDATE"; i++ {
		r.run(t, 1)
	}
	if r.state(t) != "VALIDATE" {
		t.Fatalf("never reached VALIDATE: %s (%s)", r.state(t), team.commandLines())
	}
	if _, err := r.eng().Requeue(ctx, r.pid, r.execID, "operator", "the task went off track"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if st := r.state(t); st != "ACCEPTANCE_CHECK" {
		t.Fatalf("state after requeue %s, want ACCEPTANCE_CHECK", st)
	}
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE unit_key = 'A' AND status = 'READY' AND project_id = '"+r.pid.String()+"'"); n != 1 {
		t.Fatal("task A was not reset to READY")
	}
	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{"create the output-format skill", "plan phase 1",
		"implement task A", // requeued before its validation
		"implement task A", "implement task B"}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery %q\ntrace %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'FIX_ATTEMPT' AND execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d fix attempts: the restarted task's first implementation counted as a fix", n)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
}
