package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// TestP15160NewProject — WBS 15.160 / 15.115 / 15.77 / 15.113: a new
// project bootstraps to an activated execution positioned at PHASE_PLAN with
// no worker activity.
func TestP15160NewProject(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "Foundation", "PENDING", 3}, {2, "Persistence", "PENDING", 1}}, nil)
	ctx := context.Background()
	before := r.boundarySnapshot(t)
	res := r.bootstrap(t)
	e := res.Execution
	if e.Number != 1 || e.Status != execution.ExecutionRunning || e.Stage != execution.StageComplete {
		t.Fatalf("execution %+v", e)
	}
	if e.WBSVersion != 3 || e.StateMachineVersion != 1 || e.InitialPhaseID != r.phases[1] || e.PlanState != execution.PlanMissing {
		t.Fatalf("binding %+v", e)
	}
	if e.InitialStateKey != "PROJECT_INIT" || e.FirstActionableStateKey != "PHASE_PLAN" || e.InitialRoleKey != "PLANNER" || e.InitialWorkUnitID != nil {
		t.Fatalf("routing %+v", e)
	}
	if r.projectStatus(t) != "RUNNING" {
		t.Fatal("project not RUNNING")
	}
	if n := r.count(t, "SELECT count(*) FROM execution_configuration_snapshots WHERE "+r.where()); n != 1 {
		t.Fatalf("snapshots = %d", n)
	}
	// Runtime at PHASE_PLAN, bound, bootstrap history NULL → PROJECT_INIT → PHASE_PLAN.
	rt, err := r.svc().RuntimeForExecution(ctx, r.pid, e.ID)
	if err != nil || r.stateKey(t, rt.CurrentStateID) != "PHASE_PLAN" || rt.WorkUnitID != nil || *rt.PhaseID != r.phases[1] {
		t.Fatalf("runtime %+v %v", rt, err)
	}
	var outcomes string
	if err := r.h.DBScan(ctx, "SELECT string_agg(outcome, ',' ORDER BY seq) FROM state_transition_history WHERE execution_id = '"+e.ID.String()+"'", &outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != "BOOTSTRAP,PLAN_REQUIRED" {
		t.Fatalf("history outcomes %q", outcomes)
	}
	// State files, context snapshot, prompt + cache.
	for _, k := range []string{"PROJECT_STATE", "PHASE_STATE", "TASK_ARCHIVE", "TEST_STATE"} {
		if n := r.count(t, "SELECT count(*) FROM state_files f JOIN state_file_types ty ON ty.id = f.type_id WHERE f.project_id = '"+r.pid.String()+"' AND ty.key = '"+k+"' AND f.current_version = 1"); n != 1 {
			t.Fatalf("%s files = %d", k, n)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM state_files f JOIN state_file_types ty ON ty.id = f.type_id WHERE f.project_id = '"+r.pid.String()+"' AND ty.key = 'ACTIVE_TASK'"); n != 0 {
		t.Fatal("ACTIVE_TASK fabricated")
	}
	if e.InitialContextSnapshotID == nil || e.InitialWorkerPromptID == nil || e.InitialCacheKey == "" {
		t.Fatalf("artifacts %+v", e)
	}
	if n := r.count(t, "SELECT count(*) FROM prompt_cache_entries WHERE cache_key = '"+e.InitialCacheKey+"'"); n == 0 {
		t.Fatal("initial prompt not cached")
	}
	if probs, err := r.svc().VerifyBootstrapCheckpoint(ctx, e.ID); err != nil || len(probs) != 0 {
		t.Fatalf("checkpoint %v %v", probs, err)
	}
	r.noWorker(t)
	if after := r.boundarySnapshot(t); after != before {
		t.Fatalf("boundary rows changed:\n%s\n---\n%s", before, after)
	}
	if strings.Contains(e.FailureReason, "x") {
		t.Fatal("unexpected failure reason")
	}
}
