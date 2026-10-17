package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
)

// T18.127 — the overseer is stopped (context cancelled) while a unit's
// validation runs: the run is ABANDONED — never PASS / FAIL — the unit is
// not DONE, and the next run validates afresh and completes.
func TestT18127ShutdownDuringValidation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	orig, err := os.ReadFile(r.script)
	if err != nil {
		t.Fatal(err)
	}
	p18ReplaceCheck(t, r, "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionRunUnitValidation && s == "validation" {
			go func() { time.Sleep(300 * time.Millisecond); cancel() }()
		}
		return nil
	})
	start := time.Now()
	_, _ = r.eng().Run(ctx, r.execID, 100)
	r.eng().SetStepHook(nil)
	if time.Since(start) > 20*time.Second {
		t.Fatal("cancellation did not stop the validation")
	}
	var status string
	_ = r.h.DBScan(context.Background(), "SELECT status FROM validation_runs WHERE execution_id = '"+r.execID.String()+"' AND scope = 'UNIT'", &status)
	if status != "ABANDONED" {
		t.Fatalf("validation run %s, want ABANDONED", status)
	}
	p18NotDone(t, r)
	if err := os.WriteFile(r.script, orig, 0o755); err != nil {
		t.Fatal(err)
	}
	r.runUntil(t, execution.StepCompleted, 100)
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"' AND scope = 'UNIT' AND status = 'PASS'"); n != 1 {
		t.Fatalf("passing unit runs %d", n)
	}
}

// T18.67 — the validation runner itself is unavailable (the executable is
// missing): bounded infrastructure retry, then VALIDATION_INFRA recovery —
// never a FIX dispatch for code that may be fine.
func TestT1867InfrastructureFailureNotFix(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	if err := os.Remove(r.script); err != nil {
		t.Fatal(err)
	}
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	if err != nil || r.execStatus(t) != "RECOVERY_REQUIRED" || !strings.Contains(r.recoveryReason(t), "validation could not run") {
		t.Fatalf("err %v status %s reason %q; %s", err, r.execStatus(t), r.recoveryReason(t), p16Trace(res))
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("worker messages %d: a FIX dispatch followed an infrastructure failure", n)
	}
}

// T18.67 — a transient infrastructure failure clears within the bounded
// retry: the unit validates and completes without recovery.
func TestT1867TransientInfrastructureRetried(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	body, _ := os.ReadFile(r.script)
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionRunUnitValidation && s == "validation" {
			_ = os.Remove(r.script)
			go func() { time.Sleep(150 * time.Millisecond); _ = os.WriteFile(r.script, body, 0o755) }()
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 100)
}

// T18.93 — the global validation limit queues excess validations: with a
// limit of 1, a second execution reaching validation waits, then
// completes once the slot frees.
func TestT1893ValidationCapacity(t *testing.T) {
	r1 := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.MaxConcurrentValidations = 1 }})
	r2 := &p16Rig{p15Rig: p15Build(t, r1.h, r1.g, r1.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, r2, p16Options{})
	r1.queuePlan(r1.planJSON(p16Unit{key: "a"}))
	r1.queue("complete")
	p18ReplaceCheck(t, r1, "sleep 2")
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r1.eng().Run(context.Background(), r1.execID, 100) }()
	deadline := time.Now().Add(15 * time.Second)
	for r1.count(t, "SELECT count(*) FROM validation_runs WHERE status = 'RUNNING' AND execution_id = '"+r1.execID.String()+"'") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r2.queuePlan(r2.planJSON(p16Unit{key: "b"}))
	r2.queue("complete")
	res, _ := r2.eng().Run(context.Background(), r2.execID, 100)
	var reason string
	_ = r2.h.DBScan(context.Background(), "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r2.execID.String()+"'", &reason)
	if r2.execStatus(t) != "WAITING" || !strings.Contains(reason, "validation capacity") {
		t.Fatalf("second validation not queued: status %s reason %q; %s", r2.execStatus(t), reason, p16Trace(res))
	}
	if n := r2.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r2.execID.String()+"'"); n != 0 {
		t.Fatalf("queued validation recorded %d runs", n)
	}
	<-done
	r2.runUntil(t, execution.StepCompleted, 100)
}
