package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/health"
	"overseer/internal/service"
)

// T18.180 — the periodic integrity scan is bounded (one batch per tick,
// round-robin) and does not block execution: a corrupted execution enters
// INVARIANT_VIOLATION recovery, the healthy one keeps running and
// completes, and INTEGRITY_VIOLATION is raised.
func TestT18180PeriodicIntegrityScan(t *testing.T) {
	a := p16NewRig(t)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	ctx := context.Background()
	for _, r := range []*p16Rig{a, b} {
		r.queuePlan(r.planJSON(p16Unit{key: "u"}))
		r.queue("complete")
		if _, err := r.eng().Run(ctx, r.execID, 2); err != nil {
			t.Fatal(err)
		}
		if st := r.execStatus(t); st != "RUNNING" && st != "WAITING" {
			t.Fatalf("setup: execution %s", st)
		}
	}
	p18SecondExecutingPhase(t, a)

	app := a.h.App
	mon := service.NewIntegrityMonitor(dbrepo.NewIntegrityScanner(app.DB().Raw()).ExecutionsAfter, a.eng().CheckInvariants,
		service.IntegrityMonitorConfig{Batch: 1}, nil)
	total := a.count(t, "SELECT count(*) FROM executions")
	for i := 0; i < total+1; i++ {
		n, err := mon.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n > 1 {
			t.Fatalf("tick checked %d executions, batch is 1", n)
		}
	}
	if a.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, a) != "INVARIANT_VIOLATION" {
		t.Fatalf("corrupted execution: %s / %s", a.execStatus(t), p18RecoveryCondition(t, a))
	}
	if st := b.execStatus(t); st == "RECOVERY_REQUIRED" {
		t.Fatal("the healthy execution was stopped")
	}
	if mon.Violations() == 0 {
		t.Fatal("violations not retained for the alert")
	}
	app.Alerts().Contribute(mon.Contribute)
	if al := p18Alert(app.Alerts().Evaluate(ctx), health.AlertIntegrityViolation); !al.Active {
		t.Fatalf("INTEGRITY_VIOLATION not raised: %+v", al)
	}
	b.runUntil(t, execution.StepCompleted, 200)
}
