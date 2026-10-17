package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/hardening"
)

// TestP16Finalization — WBS 16.150–16.160 (P16-159).
func TestP16Finalization(t *testing.T) {
	t.Run("final validation failure blocks completion until repaired", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.queue("complete") // HARDENING_FIX reply
		r.setCheck(t, "final", 2)
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionHardeningFix && stage == "dispatch" {
				if r.projectStatus(t) == "COMPLETED" {
					t.Error("project completed before final validation passed")
				}
				r.setCheck(t, "final", 0) // the repair fixes the regression
			}
			return nil
		})
		defer r.eng().SetStepHook(nil)
		r.runUntil(t, execution.StepCompleted, 150)
		h := r.statesSeq(t)
		if !strings.Contains(h, "FINAL_VALIDATE→FINAL_HARDENING→HARDENING_FIX→FINAL_HARDENING→FINAL_VALIDATE→FINAL_CHECKPOINT→PROJECT_DONE") {
			t.Fatalf("history %s", h)
		}
		evs := r.events(t)
		if countOf(evs, "FINAL_VALIDATION_FAILED") != 1 || countOf(evs, "FINAL_VALIDATION_PASSED") != 1 {
			t.Fatalf("events %v", evs)
		}
	})
	t.Run("persistent final failure ends in recovery, not completion", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.MaxFixAttempts = 1 }})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.queue("complete")
		r.setCheck(t, "final", 2)
		r.runUntil(t, execution.StepRecoveryRequired, 150)
		if r.projectStatus(t) != "RUNNING" || r.execStatus(t) != "RECOVERY_REQUIRED" {
			t.Fatalf("project %s exec %s", r.projectStatus(t), r.execStatus(t))
		}
	})
	t.Run("required final checkpoint failure blocks completion", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionFinalCheckpoint && stage == "intent" {
				_ = renameGit(r.dir, true)
			}
			return nil
		})
		defer func() { _ = renameGit(r.dir, false) }()
		r.runUntil(t, execution.StepRecoveryRequired, 100)
		if r.projectStatus(t) == "COMPLETED" || !strings.Contains(r.recoveryReason(t), "final checkpoint") {
			t.Fatalf("project %s reason %q", r.projectStatus(t), r.recoveryReason(t))
		}
		if n := r.count(t, "SELECT count(*) FROM execution_completions WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
			t.Fatal("completion written")
		}
	})
	t.Run("completion is written once and immutable", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.runUntil(t, execution.StepCompleted, 100)
		var at1 string
		if err := r.h.DBScan(context.Background(), "SELECT completed_at::text FROM execution_completions WHERE execution_id = '"+r.execID.String()+"'", &at1); err != nil {
			t.Fatal(err)
		}
		// Further steps / scheduling never touch a COMPLETED execution.
		if res, err := r.eng().Step(context.Background(), r.execID); err != nil || res.Outcome != execution.StepCompleted {
			t.Fatalf("step on completed: %+v %v", res, err)
		}
		if _, err := r.eng().RecoverExecutions(context.Background()); err != nil {
			t.Fatal(err)
		}
		rows, _ := r.engRepo().ListSchedulable(context.Background(), r.now(), 10)
		if len(rows) != 0 {
			t.Fatal("completed execution schedulable")
		}
		if _, err := r.h.DBExec(context.Background(), "UPDATE executions SET status = 'RUNNING' WHERE id = '"+r.execID.String()+"'"); err == nil {
			t.Fatal("completed execution mutated")
		}
		if _, err := r.h.DBExec(context.Background(), "UPDATE execution_completions SET completed_at = now() WHERE execution_id = '"+r.execID.String()+"'"); err == nil {
			t.Fatal("completion record mutated")
		}
		var at2 string
		_ = r.h.DBScan(context.Background(), "SELECT completed_at::text FROM execution_completions WHERE execution_id = '"+r.execID.String()+"'", &at2)
		if at1 != at2 || countOf(r.events(t), "PROJECT_COMPLETED") != 1 {
			t.Fatal("completion rewritten")
		}
		// A completion payload names every WBS 16.158 item.
		var payload string
		_ = r.h.DBScan(context.Background(), "SELECT payload::text FROM execution_events WHERE type = 'PROJECT_COMPLETED' AND execution_id = '"+r.execID.String()+"'", &payload)
		for _, k := range []string{"execution_id", "final_phase_id", "final_validation_run_id", "final_checkpoint_id", "completed_at"} {
			if !strings.Contains(payload, k) {
				t.Fatalf("payload lacks %s: %s", k, payload)
			}
		}
	})
	t.Run("final validation is not eligible early", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionMorePhases && stage == "intent" {
				// Force the workflow straight to final validation while phase
				// two is still pending.
				r.exec(t, "UPDATE runtime_state SET current_state_id = (SELECT id FROM workflow_states WHERE key = 'FINAL_VALIDATE' AND machine_id = "+
					"(SELECT id FROM state_machines WHERE id = runtime_state.state_machine_id OR project_id = runtime_state.project_id ORDER BY version DESC LIMIT 1)) "+
					"WHERE execution_id = '"+r.execID.String()+"'")
				return errP16Crash
			}
			return nil
		})
		_, _ = r.eng().Run(context.Background(), r.execID, 100)
		r.eng().SetStepHook(nil)
		if r.state(t) != "FINAL_VALIDATE" {
			t.Skipf("could not position the runtime (%s)", r.state(t))
		}
		r.exec(t, "UPDATE execution_steps SET status = 'ABANDONED', finished_at = now() WHERE status = 'INTENT' AND execution_id = '"+r.execID.String()+"'")
		r.runUntil(t, execution.StepRecoveryRequired, 10)
		if !strings.Contains(r.recoveryReason(t), "not eligible") || r.count(t, "SELECT count(*) FROM validation_runs WHERE scope = 'FINAL' AND execution_id = '"+r.execID.String()+"'") != 0 {
			t.Fatalf("reason %q", r.recoveryReason(t))
		}
	})
}
