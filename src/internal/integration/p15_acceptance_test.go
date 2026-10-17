package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
)

func (r *p15Rig) column(t *testing.T, sql string) []string {
	t.Helper()
	rows, err := r.h.App.DB().Raw().Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestP15168Acceptance — WBS 15.168: the complete bootstrap chain on a
// three-phase project (1 DONE, 2 PENDING with WBS v4 and no plan, 3
// PENDING).
func TestP15168Acceptance(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "Foundation", "DONE", 1}, {2, "Persistence", "PENDING", 4}, {3, "API", "PENDING", 1}}, nil)
	ctx := context.Background()
	before := r.boundarySnapshot(t)
	headBefore := p15Git(t, r.dir, "rev-parse", "HEAD")
	res := r.bootstrap(t)
	e := res.Execution
	eid := e.ID.String()

	// 1–3: lock, READY confirmed, readiness passed (events), 13 hash.
	events := r.column(t, "SELECT type FROM execution_events WHERE execution_id = '"+eid+"' ORDER BY seq")
	want := []string{"EXECUTION_BOOTSTRAP_REQUESTED", "EXECUTION_READINESS_VALIDATED", "EXECUTION_CONFIGURATION_SNAPSHOTTED",
		"EXECUTION_STATE_FILES_INITIALIZED", "EXECUTION_RUNTIME_INITIALIZED", "EXECUTION_CONTEXT_PREPARED", "EXECUTION_PROMPT_PREPARED",
		"EXECUTION_BOOTSTRAP_COMPLETED", "EXECUTION_STARTED"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("event chain:\n%v\nwant\n%v", events, want)
	}
	stages := r.column(t, "SELECT to_stage || ':' || status FROM execution_bootstrap_transitions WHERE execution_id = '"+eid+"' ORDER BY sequence")
	wantStages := []string{"VALIDATING:INITIALIZING", "SNAPSHOTTING:INITIALIZING", "INITIALIZING_FILES:INITIALIZING", "INITIALIZING_RUNTIME:INITIALIZING",
		"PREPARING_CONTEXT:INITIALIZING", "FINALIZING:INITIALIZING", "COMPLETE:INITIALIZING", "COMPLETE:READY", "COMPLETE:RUNNING"}
	if strings.Join(stages, ",") != strings.Join(wantStages, ",") {
		t.Fatalf("stage chain:\n%v\nwant\n%v", stages, wantStages)
	}
	// 4–5: canonical working directory + repository baseline.
	_, c, err := r.svc().Configuration(ctx, r.pid, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkingDirectory != r.dir && !strings.HasSuffix(c.WorkingDirectory, r.dir) {
		t.Fatalf("working directory %q", c.WorkingDirectory)
	}
	var head string
	_ = r.h.DBScan(ctx, "SELECT head_commit FROM execution_repository_baselines WHERE kind = 'SNAPSHOT' AND execution_id = '"+eid+"'", &head)
	if head != headBefore {
		t.Fatal("baseline HEAD")
	}
	// 6–8: phase 2, WBS v4, missing plan.
	if e.InitialPhaseID != r.phases[2] || e.WBSVersion != 4 || c.WBS.Version != 4 || e.PlanState != execution.PlanMissing || e.PlanID != nil {
		t.Fatalf("phase/WBS/plan %+v", e)
	}
	// 9–12: machine, roles, templates, configs captured.
	if c.StateMachine.InitialState.Key != "PROJECT_INIT" || len(c.Roles) == 0 || len(c.PromptTemplates) == 0 ||
		c.Compression.Provider != "fake" || c.Worker.Provider != "hermes" || c.ResponseProtocol.ProtocolID == nil || c.Continuation.ContextThreshold != 0.8 {
		t.Fatalf("captured configuration %+v", c)
	}
	// 14: state files; 15–17: runtime + history + control resolution.
	if n := r.count(t, "SELECT count(*) FROM state_files WHERE "+r.where()); n != 4 {
		t.Fatalf("state files %d", n)
	}
	hist := r.column(t, "SELECT outcome FROM state_transition_history WHERE execution_id = '"+eid+"' ORDER BY seq")
	if strings.Join(hist, ",") != "BOOTSTRAP,PLAN_REQUIRED" {
		t.Fatalf("runtime history %v", hist)
	}
	key, unit := r.runtimeKey(t, e.ID)
	if key != "PHASE_PLAN" || unit != nil {
		t.Fatal("runtime position")
	}
	// 18–22: planning role, context snapshot, prompt, compression, cache.
	if e.InitialRoleKey != "PLANNER" || e.InitialContextSnapshotID == nil || e.InitialWorkerPromptID == nil || e.InitialTemplateKey != "PHASE_PLAN" {
		t.Fatalf("context/prompt %+v", e)
	}
	var stateKey string
	_ = r.h.DBScan(ctx, "SELECT state_key FROM context_snapshots WHERE id = '"+e.InitialContextSnapshotID.String()+"'", &stateKey)
	if stateKey != "PHASE_PLAN" {
		t.Fatal("context snapshot state")
	}
	if n := r.count(t, "SELECT count(*) FROM compression_runs cr JOIN canonical_prompts cp ON cp.id = cr.canonical_prompt_id WHERE cp."+r.where()); n == 0 {
		t.Fatal("prompt not run through compression preparation")
	}
	if n := r.count(t, "SELECT count(*) FROM prompt_cache_entries WHERE cache_key = '"+e.InitialCacheKey+"'"); n == 0 {
		t.Fatal("prompt not cached")
	}
	// 23: precomputation scheduled (asynchronously).
	deadline := time.Now().Add(5 * time.Second)
	for r.count(t, "SELECT count(*) FROM precompute_jobs WHERE "+r.where()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.count(t, "SELECT count(*) FROM precompute_jobs WHERE trigger = 'PROJECT_START' AND "+r.where()) == 0 {
		t.Fatal("precompute not scheduled")
	}
	// 24–28: checkpoint created + verified, activation, RUNNING.
	if probs, err := r.svc().VerifyBootstrapCheckpoint(ctx, e.ID); err != nil || len(probs) != 0 {
		t.Fatalf("checkpoint %v %v", probs, err)
	}
	if e.Status != execution.ExecutionRunning || r.projectStatus(t) != "RUNNING" {
		t.Fatal("not activated")
	}
	// Invariants.
	if after := r.boundarySnapshot(t); after != before {
		t.Fatal("phases / WBS / plans / units changed")
	}
	phaseStatus := r.column(t, "SELECT status FROM phases WHERE "+r.where()+" ORDER BY phase_order")
	if strings.Join(phaseStatus, ",") != "DONE,PENDING,PENDING" {
		t.Fatalf("phases %v", phaseStatus)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where()); n != 0 {
		t.Fatal("plan fabricated")
	}
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE "+r.where()); n != 0 {
		t.Fatal("work unit fabricated")
	}
	r.noWorker(t)
}
