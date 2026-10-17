package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// T18.24 — the worker session disappears between units: task and
// continuation state are preserved, a fresh session is created, and the
// next unit's prompt comes from PostgreSQL alone (no chat history).
func TestT1824SessionLoss(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queue("complete")
	r.queue("complete")
	killed := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionSelectUnit && stage == "intent" && strings.Contains(r.unitStatuses(t, 1), "u1:DONE") && !killed {
			killed = true
			for _, s := range r.g.Sessions() {
				r.g.KillSession(s)
			}
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res := r.run(t, 200)
	if last(res) != execution.StepCompleted {
		// One bounded reconciliation may be needed; it must not need history.
		res = r.runUntil(t, execution.StepCompleted, 200)
	}
	if !killed {
		t.Fatal("session never killed")
	}
	if len(r.g.Sessions()) < 1 || r.count(t, "SELECT count(*) FROM worker_sessions WHERE status = 'LOST'") < 1 {
		t.Fatalf("sessions %v lost %d", r.g.Sessions(), r.count(t, "SELECT count(*) FROM worker_sessions WHERE status = 'LOST'"))
	}
	msgs := r.g.Messages()
	lastMsg := msgs[len(msgs)-1]
	if lastMsg.SessionID == msgs[0].SessionID {
		t.Fatal("work continued in the lost session")
	}
	if !strings.Contains(lastMsg.Content, "u2") {
		t.Fatal("fresh session prompt lacks the task (not reconstructed)")
	}
	if strings.Count(r.unitStatuses(t, 1), "DONE") != 2 {
		t.Fatalf("units %s", r.unitStatuses(t, 1))
	}
}

// T18.25 — the gateway restarts while the overseer runs (transport down,
// sessions forgotten): safe operations resume afterwards.
func TestT1825ProviderRestart(t *testing.T) {
	r := p16NewRig(t)
	proxy := proxyOf(t, r.g)
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queue("complete")
	r.queue("complete")
	restarted := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionSelectUnit && stage == "intent" && strings.Contains(r.unitStatuses(t, 1), "u1:DONE") && !restarted {
			restarted = true
			proxy.Stop()
			for _, s := range r.g.Sessions() {
				r.g.KillSession(s)
			}
			proxy.Start(t)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res := r.run(t, 200)
	for i := 0; i < 3 && last(res) != execution.StepCompleted; i++ {
		res = r.run(t, 200)
	}
	if last(res) != execution.StepCompleted || r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND status = 'COMPLETED'") != 2 {
		t.Fatalf("after provider restart: %s recovery=%q", p16Trace(res), r.recoveryReason(t))
	}
}
