package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/gateway/hermes/hermestest"
)

// T18.125 / T18.126 + decision U6 — shutdown during a worker call: unsafe
// requests are refused (503) while draining, the scheduler starts nothing
// new, the drain is bounded, the interrupted dispatch is not marked FAILED
// (it stays reconcilable) and the driver lease is released, so a new
// instance takes the execution over at once and reconciles it without
// resending.
func TestT18125ShutdownDrainsAndReleases(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{config: p17Config(func(c *config.Config) {
		c.Engine.SchedulerDisabled = false
		c.Engine.Interval = 50 * time.Millisecond
		c.Shutdown.WorkerDrain = 300 * time.Millisecond
	})})
	hold := make(chan struct{})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	plan := p16Reply("complete", "Planned the phase.", `"plan":`+r.planJSON(p16Unit{key: "a"}))
	r.g.Script(hermestest.Behavior{Content: &plan, Hold: hold})
	r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")
	r.eng().Kick()
	deadline := time.Now().Add(15 * time.Second)
	for len(r.g.Messages()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("setup: the scheduler sent nothing: %s wait %q steps %d blocked %q", r.execStatus(t), r.recoveryReason(t), r.count(t, "SELECT count(*) FROM execution_steps"), r.h.App.WorkGate().Blocked())
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := newBrowser(t, r.h.BaseURL)
	b.page("/") // session + CSRF before draining

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.h.App.Shutdown(context.Background()) }()
	refused := false
	for !refused && time.Since(start) < 250*time.Millisecond {
		code, body := b.api(http.MethodPost, "/api/v1/projects/"+r.pid.String()+"/pause", nil)
		if code == http.StatusServiceUnavailable && strings.Contains(string(body), "SHUTTING_DOWN") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("an unsafe request was not refused while draining")
	}
	if err := <-done; err != nil {
		t.Logf("shutdown: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("drain not bounded: %v", el)
	}
	if n := len(r.g.Messages()); n != 1 {
		t.Fatalf("%d worker requests; a new one started during shutdown", n)
	}
	interrupted := r.g.Messages()[0].RequestID

	// 18.126: the interrupted dispatch is not FAILED / CANCELLED.
	h := restart()
	ctx := context.Background()
	var status string
	_ = h.DBScan(ctx, "SELECT status FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' ORDER BY created_at LIMIT 1", &status)
	if status == "FAILED" || status == "CANCELLED" {
		t.Fatalf("our own shutdown marked the dispatch %s", status)
	}
	// The lease was released: the new instance drives the execution now
	// (no ErrExecutionBusy) and reconciles the unknown delivery without
	// resending.
	if _, err := r.eng().Run(ctx, r.execID, 5); err != nil && strings.Contains(err.Error(), "busy") {
		t.Fatalf("lease still held after shutdown: %v", err)
	}
	// (Later planning attempts are new requests; the interrupted request
	// itself is never sent again.)
	seen := map[string]int{}
	for _, m := range r.g.Messages() {
		seen[m.RequestID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("request %s sent %d times", id, n)
		}
	}
	if seen[interrupted] != 1 {
		t.Fatal("the interrupted request was resent")
	}
	_ = h.DBScan(ctx, "SELECT status FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' ORDER BY created_at LIMIT 1", &status)
	if status != "UNKNOWN" {
		t.Fatalf("interrupted dispatch reconciled to %s, want UNKNOWN", status)
	}
}
