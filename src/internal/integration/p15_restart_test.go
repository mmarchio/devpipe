package integration

import (
	"context"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/persistence"
)

func p15Recovery(c *config.Config) { c.Bootstrap.RecoveryDisabled = false }

// TestP15107RestartBeforeExecution — WBS 15.107: dying before the
// execution record exists leaves no ghost; a new request bootstraps.
func TestP15107RestartBeforeExecution(t *testing.T) {
	r := p15NewRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.svc().SetCreateHook(func(context.Context, int) { cancel() })
	if _, err := r.svc().Bootstrap(ctx, r.pid); err == nil {
		t.Fatal("expected interrupted bootstrap")
	}
	r.svc().SetCreateHook(nil)
	if n := r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()); n != 0 {
		t.Fatalf("ghost executions %d", n)
	}
	if e := r.bootstrap(t).Execution; e.Number != 1 {
		t.Fatalf("%+v", e)
	}
}

// TestP15108RestartDuringStages — WBS 15.108–15.111: an instance freezes in
// each stage (no failure recorded); after its claim expires a restarted
// instance resumes the same execution from the persisted stage — no second
// execution, runtime, logical state file or recomposition.
func TestP15108RestartDuringStages(t *testing.T) {
	for _, stage := range []execution.BootstrapStage{execution.StageSnapshotting, execution.StageInitializingFiles,
		execution.StageInitializingRuntime, execution.StagePreparingContext, execution.StageFinalizing} {
		t.Run(string(stage), func(t *testing.T) {
			r, boot := p15RestartRig(t, p15DefaultPhases, nil)
			ctx := context.Background()
			if _, err := r.svc().UpdateExecutionSettings(ctx, r.pid, execution.Settings{DirtyPolicy: execution.DirtyWarn,
				MaxBootstrapControlTransitions: 100, BootstrapLease: time.Second}); err != nil {
				t.Fatal(err)
			}
			frozen := make(chan struct{})
			release := make(chan struct{})
			r.svc().SetStageHook(func(ctx context.Context, rec *persistence.ExecutionRecord, st execution.BootstrapStage) error {
				if st == stage {
					close(frozen)
					<-release
					return context.Canceled // the "dead" instance never proceeds
				}
				return nil
			})
			done := make(chan struct{})
			go func() { defer close(done); _, _ = r.svc().Bootstrap(ctx, r.pid) }()
			<-frozen
			var execID string
			_ = r.h.DBScan(ctx, "SELECT id FROM executions WHERE "+r.where(), &execID)
			time.Sleep(1100 * time.Millisecond) // claim expires
			canonicalBefore := r.count(t, "SELECT count(*) FROM canonical_prompts WHERE "+r.where())
			h2 := boot(p15Recovery) // restart: startup recovery resumes the bootstrap
			r2 := *r
			r2.h = h2
			var status, id string
			if err := h2.DBScan(ctx, "SELECT id, status FROM executions WHERE "+r.where(), &id, &status); err != nil {
				t.Fatal(err)
			}
			if id != execID || status != "RUNNING" {
				t.Fatalf("resumed %s %s (want %s RUNNING)", id, status, execID)
			}
			if stage == execution.StageFinalizing {
				if n := r2.count(t, "SELECT count(*) FROM canonical_prompts WHERE "+r.where()); n != canonicalBefore {
					t.Fatalf("recomposed: %d → %d", canonicalBefore, n)
				}
			}
			close(release)
			<-done
			r2.assertSingleBootstrap(t)
		})
	}
}

// TestP15166RestartAfterBootstrap — WBS 15.112 / 15.134 / 15.166: after
// activation a fresh instance reconstructs every bootstrap fact from the
// database, reruns nothing and calls no worker.
func TestP15166RestartAfterBootstrap(t *testing.T) {
	r, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 2}}, nil)
	ctx := context.Background()
	e1 := r.bootstrap(t).Execution
	events := r.eventCounts(t)
	r.h.Close()
	h2 := boot(p15Recovery)
	r2 := *r
	r2.h = h2
	svc := h2.App.Bootstrap()
	e, err := svc.ActiveExecution(ctx, r.pid)
	if err != nil || e.ID != e1.ID || e.Status != execution.ExecutionRunning || e.Number != 1 {
		t.Fatalf("%+v %v", e, err)
	}
	if r2.projectStatus(t) != "RUNNING" {
		t.Fatal("project not RUNNING")
	}
	snap, c, err := svc.Configuration(ctx, r.pid, e.ID)
	if err != nil || snap.Hash != execution.HashCanonical(snap.Canonical) || c.WBS.Version != 2 || c.StateMachine.Version != 1 {
		t.Fatalf("configuration %v", err)
	}
	rt, err := svc.RuntimeForExecution(ctx, r.pid, e.ID)
	if err != nil || r2.stateKey(t, rt.CurrentStateID) != e.FirstActionableStateKey || e.FirstActionableStateKey != "PHASE_PLAN" {
		t.Fatalf("runtime %v", err)
	}
	if e.InitialContextSnapshotID == nil || e.InitialCacheKey == "" || e.InitialWorkerPromptID == nil {
		t.Fatal("context / prompt identity lost")
	}
	if n := r2.count(t, "SELECT count(*) FROM execution_repository_baselines WHERE execution_id = '"+e.ID.String()+"'"); n != 2 {
		t.Fatalf("baselines %d", n)
	}
	if probs, err := svc.VerifyBootstrapCheckpoint(ctx, e.ID); err != nil || len(probs) != 0 {
		t.Fatalf("checkpoint %v %v", probs, err)
	}
	_, v, err := svc.ActiveExecutionView(ctx, r.pid)
	if err != nil || v.Worker != "Not yet dispatched" || v.Role != "PLANNER" || v.WorkflowState != "PHASE_PLAN" {
		t.Fatalf("view %+v %v", v, err)
	}
	if after := r2.eventCounts(t); after[string(execution.EventStarted)] != events[string(execution.EventStarted)] {
		t.Fatal("bootstrap rerun after restart")
	}
	r2.assertSingleBootstrap(t)
}
