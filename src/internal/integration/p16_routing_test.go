package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/hardening"
	"overseer/internal/service"
)

// TestP16ResultRouting — WBS 16.37, 16.40–16.42, 16.75, 16.76, 16.26, 16.27.
func TestP16ResultRouting(t *testing.T) {
	t.Run("blocked requires recovery, not FAILED or COMPLETE", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("blocked")
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		if !strings.Contains(r.recoveryReason(t), "BLOCKED") || r.execStatus(t) != "RECOVERY_REQUIRED" || r.projectStatus(t) != "RUNNING" {
			t.Fatalf("reason %q exec %s project %s", r.recoveryReason(t), r.execStatus(t), r.projectStatus(t))
		}
		if strings.Contains(r.unitStatuses(t, 1), "DONE") || strings.Contains(r.unitStatuses(t, 1), "FAILED") {
			t.Fatal(r.unitStatuses(t, 1))
		}
		// Scheduler excludes it (16.76).
		rows, _ := r.engRepo().ListSchedulable(context.Background(), r.now(), 10)
		if len(rows) != 0 {
			t.Fatal("recovery execution schedulable")
		}
	})
	t.Run("worker FAILED follows the configured FIX edge", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("failed")
		r.queue("complete")
		r.runUntil(t, execution.StepCompleted, 100)
		if !strings.Contains(r.statesSeq(t), "RESPONSE_EVALUATE→FIX→IMPLEMENT→RESPONSE_EVALUATE→VALIDATE") || r.projectStatus(t) != "COMPLETED" {
			t.Fatalf("history %s", r.statesSeq(t))
		}
	})
	t.Run("malformed result is a protocol failure", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queueRaw("this is prose, not the protocol", 1000)
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		if !strings.Contains(r.recoveryReason(t), "protocol") || strings.Contains(r.statesSeq(t), "FIX") {
			t.Fatalf("reason %q history %s", r.recoveryReason(t), r.statesSeq(t))
		}
	})
	t.Run("delivery unknown is never resent", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.g.Script(hermestest.Behavior{Drop: true})
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		sends := len(r.g.Messages())
		if !strings.Contains(r.recoveryReason(t), "delivery unknown") {
			t.Fatalf("reason %q", r.recoveryReason(t))
		}
		// Even an explicit operator retry re-evaluates; it never resends.
		if _, err := r.eng().ResolveRecovery(context.Background(), r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryRetryStep}); err != nil {
			t.Fatal(err)
		}
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		if len(r.g.Messages()) != sends {
			t.Fatal("unknown delivery resent")
		}
	})
	t.Run("context limit routes to a reset continuation", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.g.Script(hermestest.Behavior{Status: 400, ErrorCode: "context_limit_exceeded", Message: "context window exceeded"})
		r.queue("complete")
		r.runUntil(t, execution.StepCompleted, 100)
		if !strings.Contains(r.statesSeq(t), "CONTINUE_DECISION→RESET_SESSION→CONTINUE") {
			t.Fatalf("history %s", r.statesSeq(t))
		}
	})
	t.Run("tampered prompt is not sent", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		// Storage corruption of the prepared IMPLEMENT prompt (the
		// immutability trigger is bypassed to simulate it).
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionDispatch && stage == "intent" {
				r.queueWorkerPromptTamper(t)
			}
			return nil
		})
		defer r.eng().SetStepHook(nil)
		before := len(r.g.Messages())
		res, _ := r.eng().Run(context.Background(), r.execID, 50)
		_ = res
		if len(r.g.Messages()) != before+1 { // only the planning message went out
			t.Fatalf("messages %d → %d", before, len(r.g.Messages()))
		}
		if !strings.Contains(r.recoveryReason(t), "integrity") {
			t.Fatalf("reason %q", r.recoveryReason(t))
		}
	})
}

// TestP16WrongCWDBlocksDispatch: a session whose "cd" prompt fails never
// gets the work prompt.
func TestP16WrongCWDBlocksDispatch(t *testing.T) {
	r := p16NewRig(t)
	r.g.FailCD(r.dir)
	r.runUntil(t, execution.StepRecoveryRequired, 20)
	if len(r.g.Messages()) != 0 || !strings.Contains(r.recoveryReason(t), "session is not ready") {
		t.Fatalf("messages %d reason %q", len(r.g.Messages()), r.recoveryReason(t))
	}
}

// TestP16Lifecycle — WBS 16.72–16.74, 16.76: pause keeps the position,
// resume continues exactly, cancel stops dispatch; recovery resolution.
func TestP16Lifecycle(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b"}))
	r.queue("complete")
	r.queue("complete")
	lc := r.h.App.Lifecycle()
	// Run until the first unit is claimed, then pause.
	paused := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionSelectUnit && stage == "intent" && !paused {
			paused = true
			if _, err := lc.Pause(ctx, r.pid.String()); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	r.run(t, 100)
	r.eng().SetStepHook(nil)
	if r.execStatus(t) != "PAUSED" {
		t.Fatalf("exec %s", r.execStatus(t))
	}
	pos := r.state(t)
	hist := len(r.history(t))
	if res, _ := r.eng().Step(context.Background(), r.execID); res.Outcome != execution.StepPaused {
		t.Fatalf("paused step %+v", res)
	}
	if r.state(t) != pos || len(r.history(t)) != hist {
		t.Fatal("position changed while paused")
	}
	if _, err := lc.Resume(context.Background(), r.pid.String()); err != nil {
		t.Fatal(err)
	}
	if r.execStatus(t) != "RUNNING" {
		t.Fatalf("resumed exec %s", r.execStatus(t))
	}
	// Cancel after the first unit: no further dispatch.
	cancelled := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionMoreWork && stage == "intent" && !cancelled {
			cancelled = true
			if _, err := lc.Cancel(ctx, r.pid.String()); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	r.run(t, 100)
	r.eng().SetStepHook(nil)
	d := r.dispatches(t)
	r.run(t, 100)
	if r.execStatus(t) != "CANCELLED" || r.dispatches(t) != d {
		t.Fatalf("exec %s dispatches %d→%d", r.execStatus(t), d, r.dispatches(t))
	}
	if strings.Count(r.unitStatuses(t, 1), "DONE") != 1 {
		t.Fatalf("units %s", r.unitStatuses(t, 1))
	}
}

// TestP16ManualCriteria — WBS 16.48 / 16.76: an unverified manual criterion
// requires review; the operator's approval resolves it.
func TestP16ManualCriteria(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a", manual: true}))
	r.queue("complete")
	r.runUntil(t, execution.StepRecoveryRequired, 50)
	if !strings.Contains(r.recoveryReason(t), "manual review") || strings.Contains(r.unitStatuses(t, 1), "DONE") {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
	var unit uuid.UUID
	if err := r.h.DBScan(context.Background(), "SELECT id FROM work_units WHERE unit_key = 'a' AND "+r.where(), &unit); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng().ResolveRecovery(context.Background(), r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryApproveCriteria,
		WorkUnitID: unit.String(), Indexes: []int{1}, By: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	r.runUntil(t, execution.StepCompleted, 100)
	if countOf(r.events(t), "EXECUTION_RECOVERY_RESOLVED") != 1 {
		t.Fatal("resolution event")
	}
}

// TestP16CheckpointPolicies — WBS 16.53–16.59.
func TestP16CheckpointPolicies(t *testing.T) {
	t.Run("REQUIRED failure keeps the unit non-DONE", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionUnitGates && stage == "intent" {
				r.exec(t, "SELECT 1") // keep hook signature
				_ = renameGit(r.dir, true)
			}
			return nil
		})
		defer func() { _ = renameGit(r.dir, false) }()
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		if !strings.Contains(r.recoveryReason(t), "required checkpoint failed") || strings.Contains(r.unitStatuses(t, 1), "DONE") {
			t.Fatalf("reason %q units %s", r.recoveryReason(t), r.unitStatuses(t, 1))
		}
		if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE status = 'FAILED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
			t.Fatal("failed checkpoint not recorded")
		}
	})
	t.Run("BEST_EFFORT failure is recorded and progresses", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) {
			s.CheckpointPolicy = execution.CheckpointBestEffort
		}})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionUnitGates && stage == "intent" {
				_ = renameGit(r.dir, true)
			}
			if a == execution.ActionMoreWork && stage == "intent" {
				_ = renameGit(r.dir, false)
			}
			return nil
		})
		r.runUntil(t, execution.StepCompleted, 100)
		if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE status = 'FAILED' AND kind = 'UNIT' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
			t.Fatal("failure not recorded")
		}
	})
	t.Run("DISABLED never touches git", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) {
			s.CheckpointPolicy = execution.CheckpointDisabled
		}})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		head := r.head(t)
		var progress []string
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionUnitGates && stage == "intent" {
				progress = append(progress, r.unitStatuses(t, 1))
			}
			return nil
		})
		r.runUntil(t, execution.StepCompleted, 100)
		if r.head(t) != head {
			t.Fatal("git mutated with checkpoints DISABLED")
		}
		if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE status = 'SKIPPED' AND execution_id = '"+r.execID.String()+"'"); n != 2 {
			t.Fatalf("skipped checkpoints = %d", n)
		}
		if len(progress) != 1 || progress[0] != "a:VALIDATING" {
			t.Fatalf("VALIDATING unit progress %v", progress)
		}
	})
}
