package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
)

// T18.26–T18.28 — a provider outage opens the circuit: no more traffic,
// the execution waits without corruption; after the cooldown a probe
// re-admits the provider and execution resumes.
func TestT1827ProviderCircuit(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.Circuit = config.CircuitConfig{Failures: 2, Window: time.Minute, Cooldown: 300 * time.Millisecond}
		p.Reconnect.MaxAttempts = 1
		c.WorkerProviders["hermes"] = p
	}})
	proxy := proxyOf(t, r.g)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	cut := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" && !cut {
			cut = true
			proxy.Stop()
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	for i := 0; i < 10; i++ {
		_, _ = r.eng().Run(context.Background(), r.execID, 20)
	}
	unsent := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE delivery = 'DEFINITELY_NOT_SENT'")
	if unsent < 1 || unsent > 2 {
		t.Fatalf("send attempts while down: %d (circuit should stop them at 2)", unsent)
	}
	var reason string
	_ = r.h.DBScan(context.Background(), "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &reason)
	if r.execStatus(t) != "WAITING" || !strings.Contains(reason, "unavailable") {
		t.Fatalf("status %s reason %q recovery %q", r.execStatus(t), reason, r.recoveryReason(t))
	}
	if strings.Contains(r.unitStatuses(t, 1), "DONE") || r.recoveryReason(t) != "" {
		t.Fatal("outage corrupted / escalated execution state")
	}
	avail := r.h.App.Worker().ProviderAvailability()
	if avail["hermes"] != "UNAVAILABLE" {
		t.Fatalf("availability %v", avail)
	}
	// Provider comes back; after the cooldown the probe closes the circuit.
	proxy.Start(t)
	time.Sleep(350 * time.Millisecond)
	r.runUntil(t, execution.StepCompleted, 100)
	if r.h.App.Worker().ProviderAvailability()["hermes"] != "HEALTHY" {
		t.Fatal("circuit not closed after recovery")
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND status = 'COMPLETED'"); n != 1 {
		t.Fatalf("completed unit dispatches %d", n)
	}
}

// T18.92 — the global dispatch limit queues excess work (WAITING) instead
// of sending it.
func TestT1892DispatchCapacity(t *testing.T) {
	r1 := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.MaxConcurrentDispatches = 1 }})
	r2 := &p16Rig{p15Rig: p15Build(t, r1.h, r1.g, r1.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, r2, p16Options{})
	hold := make(chan struct{})
	r1.queuePlan(r1.planJSON(p16Unit{key: "a"}))
	c := p16Reply("complete", "held", "")
	r1.g.Script(hermestest.Behavior{Content: &c, Hold: hold})
	done := make(chan struct{})
	base := len(r1.g.Messages())
	go func() { defer close(done); _, _ = r1.eng().Run(context.Background(), r1.execID, 100) }()
	// Wait until the held unit request has reached the gateway (the row
	// turns WAITING before the message lands).
	deadline := time.Now().Add(10 * time.Second)
	for (r1.count(t, "SELECT count(*) FROM worker_dispatches WHERE status = 'WAITING' AND work_unit_id IS NOT NULL") == 0 ||
		len(r1.g.Messages()) <= base+1) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r2.queuePlan(r2.planJSON(p16Unit{key: "b"}))
	sends := len(r1.g.Messages())
	res, _ := r2.eng().Run(context.Background(), r2.execID, 50)
	var reason string
	_ = r2.h.DBScan(context.Background(), "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r2.execID.String()+"'", &reason)
	if len(r1.g.Messages()) != sends || !strings.Contains(reason, "capacity") {
		t.Fatalf("second execution not queued: %s reason %q", p16Trace(res), reason)
	}
	close(hold)
	<-done
	r2.queue("complete")
	r1.queue("complete")
	r1.runUntil(t, execution.StepCompleted, 100)
	r2.runUntil(t, execution.StepCompleted, 100)
}
