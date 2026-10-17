package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
)

// T18.17 — a dispatch the transport proves was never delivered is resent
// safely: exactly one eventual delivery.
func TestT1817ConfirmedUnsentResent(t *testing.T) {
	r := p16NewRig(t)
	proxy := proxyOf(t, r.g)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	cut := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" && !cut {
			cut = true
			proxy.Stop() // connection refused: provably not sent
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, _ := r.eng().Run(context.Background(), r.execID, 100)
	if !cut {
		t.Fatal("cut point not reached")
	}
	proxy.Start(t)
	r.eng().SetStepHook(nil)
	if last(res) != execution.StepCompleted {
		res = r.runUntil(t, execution.StepCompleted, 100)
	}
	var unitMsgs int
	for _, m := range r.g.Messages() {
		if !strings.Contains(m.Content, "Planned") && !strings.Contains(m.Content, "plan") {
			unitMsgs++
		}
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND status = 'COMPLETED'"); n != 1 {
		t.Fatalf("completed unit dispatches %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND delivery = 'DEFINITELY_NOT_SENT'"); n < 1 {
		t.Fatal("unsent attempt not recorded")
	}
	if got := len(r.g.Messages()); got != 2 { // plan + exactly one unit delivery
		t.Fatalf("gateway received %d requests", got)
	}
}

// T18.18 — a delivered request whose response is delayed is waited for;
// no second request is created.
func TestT1818DelayedResponseNotResent(t *testing.T) {
	r := p16NewRig(t)
	hold := make(chan struct{})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	c := p16Reply("complete", "slow but fine", "")
	r.g.Script(hermestest.Behavior{Content: &c, Hold: hold})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.eng().Run(context.Background(), r.execID, 100) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.g.Messages()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if len(r.g.Messages()) != 2 {
		t.Fatalf("requests while waiting: %d", len(r.g.Messages()))
	}
	close(hold)
	<-done
	r.runUntil(t, execution.StepCompleted, 100)
	if len(r.g.Messages()) != 2 {
		t.Fatalf("second request created: %d", len(r.g.Messages()))
	}
}

// T18.19–T18.22 — an unknown delivery is never resent automatically;
// the recovery evidence carries the repository baseline / current state,
// changes, timestamps and session without claiming causality; a repository
// change does not classify the unit COMPLETE.
func TestT1819UnknownDeliveryEvidence(t *testing.T) {
	for _, changed := range []bool{false, true} {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.g.Script(hermestest.Behavior{Drop: true})
		if changed {
			r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
				if a == execution.ActionDispatch && stage == "dispatched" {
					_ = writeFile(r.dir, "src/a/new.go", "package a\n") // something changed the repository
				}
				return nil
			})
		}
		_, _ = r.eng().Run(context.Background(), r.execID, 100)
		r.eng().SetStepHook(nil)
		if r.execStatus(t) != "RECOVERY_REQUIRED" {
			// The hook point after a dropped send may not be reached; make
			// the change before re-evaluation instead.
			t.Fatalf("status %s", r.execStatus(t))
		}
		sends := len(r.g.Messages())
		for i := 0; i < 3; i++ {
			_, _ = r.eng().Run(context.Background(), r.execID, 20)
		}
		if len(r.g.Messages()) != sends || r.execStatus(t) != "RECOVERY_REQUIRED" {
			t.Fatal("unknown delivery resent / left recovery automatically")
		}
		var ev string
		_ = r.h.DBScan(context.Background(), "SELECT recovery_evidence::text FROM executions WHERE id = '"+r.execID.String()+"'", &ev)
		for _, want := range []string{`"baseline_head"`, `"current_head"`, `"baseline_fingerprint"`, `"current_fingerprint"`, `"dispatch_created_at"`,
			`"worker_session_id"`, `"evidence_note"`, `"repository_changed_since_intent"`} {
			if !strings.Contains(ev, want) {
				t.Fatalf("evidence lacks %s: %s", want, ev)
			}
		}
		if strings.Contains(r.unitStatuses(t, 1), "DONE") || strings.Contains(strings.ToUpper(r.unitStatuses(t, 1)), "COMPLETE") {
			t.Fatalf("unit classified complete: %s", r.unitStatuses(t, 1))
		}
	}
}

func writeFile(dir, rel, content string) error {
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
