package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

func (r *p15Rig) snapshotContent(t *testing.T, e *persistence.ExecutionRecord) *execution.ConfigurationContent {
	t.Helper()
	_, c, err := r.svc().Configuration(context.Background(), r.pid, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *p15Rig) bumpPrompt(t *testing.T, role string) {
	t.Helper()
	if _, err := dbrepo.NewSystemPromptRepository(r.h.App.DB().Raw()).AppendVersion(context.Background(), r.roles[role].String(),
		"You are the "+role+" role (revised).\nNever modify files outside the active task scope."); err != nil {
		t.Fatal(err)
	}
}

// TestP15128ProjectChangeDuringBootstrap — WBS 15.90 / 15.128: a project
// configuration change between readiness and the snapshot commit reruns
// readiness; the snapshot holds the new value only.
func TestP15128ProjectChangeDuringBootstrap(t *testing.T) {
	r := p15NewRig(t)
	r.svc().SetCreateHook(func(ctx context.Context, attempt int) {
		if attempt == 0 {
			r.exec(t, "UPDATE project_settings SET context_threshold = 0.7 WHERE "+r.where())
			r.exec(t, "UPDATE projects SET version = version + 1 WHERE id = '"+r.pid.String()+"'")
		}
	})
	defer r.svc().SetCreateHook(nil)
	e := r.bootstrap(t).Execution
	c := r.snapshotContent(t, e)
	if c.Continuation.ContextThreshold != 0.7 {
		t.Fatalf("hybrid snapshot: threshold %v", c.Continuation.ContextThreshold)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()); n != 1 {
		t.Fatalf("executions %d", n)
	}
}

// TestP15129WBSChangeDuringBootstrap — WBS 15.91 / 15.129: WBS v4 → v5
// during initialization; no execution records v4 with v5-derived context —
// #1 fails (configuration drift), #2 binds v5.
func TestP15129WBSChangeDuringBootstrap(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "P", "PENDING", 4}}, nil)
	fired := false
	r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
		if st == execution.StageInitializingFiles && !fired {
			fired = true
			r.appendWBS(t, 1)
		}
		return nil
	})
	defer r.svc().SetStageHook(nil)
	e := r.bootstrap(t).Execution
	if e.WBSVersion != 5 || e.Number != 2 || e.Status != execution.ExecutionRunning {
		t.Fatalf("%+v", e)
	}
	var st, code string
	var wbs int
	if err := r.h.DBScan(context.Background(), "SELECT status, failure_code, wbs_version FROM executions WHERE number = 1 AND "+r.where(), &st, &code, &wbs); err != nil {
		t.Fatal(err)
	}
	if st != "FAILED" || code != string(execution.CodeConfigInvalid) || wbs != 4 {
		t.Fatalf("#1 %s %s %d", st, code, wbs)
	}
}

// TestP15130MachineChangeDuringBootstrap — WBS 15.93 / 15.130: a new
// machine version appears mid-bootstrap; the execution binds entirely to
// the version it snapshotted.
func TestP15130MachineChangeDuringBootstrap(t *testing.T) {
	r := p15NewRig(t)
	fired := false
	r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
		if st == execution.StageInitializingFiles && !fired {
			fired = true
			r.customMachineOpt(t, "v2", []serviceMachineState{{"BOOT", "CONTROL"}, {"PLAN", "ACTION"}, {"END", "TERMINAL"}},
				[][3]string{{"BOOT", "PROCEED", "PLAN"}, {"PLAN", "SUBMIT", "END"}}, "BOOT", map[string]string{"PLAN": "PLAN_PHASE"}, false)
		}
		return nil
	})
	defer r.svc().SetStageHook(nil)
	e := r.bootstrap(t).Execution
	rt, _ := r.svc().RuntimeForExecution(context.Background(), r.pid, e.ID)
	c := r.snapshotContent(t, e)
	if e.StateMachineID != r.machine || e.StateMachineVersion != 1 || rt.StateMachineVersion != 1 || c.StateMachine.Version != 1 ||
		e.FirstActionableStateKey != "PHASE_PLAN" {
		t.Fatalf("mixed machine binding %+v rt %+v", e, rt)
	}
}

// TestP15131RoleChangeDuringBootstrap — WBS 15.92 / 15.131: the planning
// role's prompt changes mid-bootstrap; the execution that completes holds
// one coherent role version in snapshot and context.
func TestP15131RoleChangeDuringBootstrap(t *testing.T) {
	r := p15NewRig(t)
	fired := false
	r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
		if st == execution.StageInitializingFiles && !fired {
			fired = true
			r.bumpPrompt(t, "PLANNER")
		}
		return nil
	})
	defer r.svc().SetStageHook(nil)
	e := r.bootstrap(t).Execution
	c := r.snapshotContent(t, e)
	var snapVer int
	for _, rs := range c.Roles {
		if rs.RoleKey == "PLANNER" {
			snapVer = rs.SystemPrompt.Version
		}
	}
	var ctxRole []byte
	if err := r.h.DBScan(context.Background(), "SELECT role_snapshot FROM context_snapshots WHERE id = '"+e.InitialContextSnapshotID.String()+"'", &ctxRole); err != nil {
		t.Fatal(err)
	}
	var rs role.RoleSnapshot
	if err := json.Unmarshal(ctxRole, &rs); err != nil {
		t.Fatal(err)
	}
	if snapVer != 2 || rs.SystemPrompt.Version != 2 {
		t.Fatalf("incoherent role versions: snapshot v%d context %s", snapVer, ctxRole)
	}
}

// TestP15126DirectoryRemovedBeforeFinalization — WBS 15.126.
func TestP15126DirectoryRemovedBeforeFinalization(t *testing.T) {
	r := p15NewRig(t)
	moved := r.dir + "-moved"
	r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
		if st == execution.StageFinalizing {
			return os.Rename(r.dir, moved)
		}
		return nil
	})
	_, be := r.bootstrapErr(t)
	r.svc().SetStageHook(nil)
	_ = be
	if r.projectStatus(t) != "READY" {
		t.Fatal("started without working directory")
	}
	if err := os.Rename(moved, r.dir); err != nil {
		t.Fatal(err)
	}
	if e := r.bootstrap(t).Execution; e.Status != execution.ExecutionRunning {
		t.Fatalf("%+v", e)
	}
}

// TestP15127HeadMovedDuringBootstrap — WBS 15.127: the FINAL baseline
// reflects the actual start state.
func TestP15127HeadMovedDuringBootstrap(t *testing.T) {
	r := p15NewRig(t)
	var newHead string
	r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
		if st == execution.StageFinalizing && newHead == "" {
			_ = os.WriteFile(filepath.Join(r.dir, "x.txt"), []byte("x"), 0o644)
			p15Git(t, r.dir, "add", "x.txt")
			p15Git(t, r.dir, "commit", "-q", "-m", "external")
			newHead = p15Git(t, r.dir, "rev-parse", "HEAD")
		}
		return nil
	})
	defer r.svc().SetStageHook(nil)
	e := r.bootstrap(t).Execution
	var snapHead, finalHead string
	_ = r.h.DBScan(context.Background(), "SELECT head_commit FROM execution_repository_baselines WHERE kind = 'SNAPSHOT' AND execution_id = '"+e.ID.String()+"'", &snapHead)
	_ = r.h.DBScan(context.Background(), "SELECT head_commit FROM execution_repository_baselines WHERE kind = 'FINAL' AND execution_id = '"+e.ID.String()+"'", &finalHead)
	if finalHead != newHead || snapHead == newHead {
		t.Fatalf("snapshot %s final %s new %s", snapHead, finalHead, newHead)
	}
}
