package integration

import (
	"context"
	"testing"

	"overseer/internal/execution"
)

// T18.196 / T18.197 — a panic inside one project's step is contained: that
// execution enters PANIC recovery (logged with its stack), it is not
// retried blindly, and the other project keeps running to completion.
func TestT18196PanicContained(t *testing.T) {
	logs := captureLogs(t)
	a := p16NewRig(t)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	ctx := context.Background()
	a.queuePlan(a.planJSON(p16Unit{key: "a"}))
	a.queue("complete")
	panicked := false
	a.eng().SetStepHook(func(_ context.Context, act execution.StepAction, s string) error {
		if act == execution.ActionDispatch && s == "intent" && !panicked {
			panicked = true
			panic("injected failure")
		}
		return nil
	})
	if _, err := a.eng().Run(ctx, a.execID, 50); err != nil {
		t.Logf("run: %v", err)
	}
	a.eng().SetStepHook(nil)
	if !panicked || a.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, a) != "PANIC" {
		t.Fatalf("panic not contained: %v %s / %s", panicked, a.execStatus(t), p18RecoveryCondition(t, a))
	}
	if !p18LogLine(logs.String(), "engine step panicked", "injected failure", "stack") {
		t.Fatal("panic not logged with its stack")
	}
	// 18.197: no blind retry — running again does nothing.
	sent := len(a.g.Messages())
	_, _ = a.eng().Run(ctx, a.execID, 10)
	if len(a.g.Messages()) != sent || a.execStatus(t) != "RECOVERY_REQUIRED" {
		t.Fatal("the panicked step was retried automatically")
	}
	// 18.196: the other project is unaffected.
	b.queuePlan(b.planJSON(p16Unit{key: "x"}))
	b.queue("complete")
	b.runUntil(t, execution.StepCompleted, 200)
}
