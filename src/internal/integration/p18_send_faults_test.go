package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/resilience"
)

// p18ArmAtDispatch arms action on reg the first time the engine reaches
// the unit dispatch intent (after the planner's request).
func p18ArmAtDispatch(r *p16Rig, reg *faults.Registry, a faults.Action) *bool {
	armed := false
	r.eng().SetStepHook(func(_ context.Context, act execution.StepAction, s string) error {
		if act == execution.ActionDispatch && s == "intent" && !armed {
			armed = true
			reg.Arm(a)
		}
		return nil
	})
	return &armed
}

func p18UnitSends(t *testing.T, r *p16Rig) int {
	return r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND work_unit_id IS NOT NULL AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT'")
}

// T18.142 — a database commit failure right before the dispatch (its
// intent cannot be made durable): nothing is sent. A non-retryable
// failure stops the execution safely; a transient one is retried under
// the db_tx policy and the unit is then dispatched exactly once.
func TestT18142CommitFailureBeforeDispatch(t *testing.T) {
	for _, transient := range []bool{false, true} {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		reg := faults.NewRegistry()
		ctx, done := faults.WithRegistry(context.Background(), reg)
		ferr := errors.New("commit failed")
		if transient {
			ferr = resilience.Mark(ferr, resilience.Transient)
		}
		armed := p18ArmAtDispatch(r, reg, faults.Action{Point: faults.BeforeDBCommit, Times: 1, Kind: faults.KindError, Err: ferr})
		_, _ = r.eng().Run(ctx, r.execID, 50)
		r.eng().SetStepHook(nil)
		if !*armed {
			t.Fatal("setup: dispatch not reached")
		}
		if !transient {
			if n := len(r.g.Messages()); n != 1 {
				t.Fatalf("%d requests: the unit was sent without durable intent", n)
			}
			if s := r.execStatus(t); s == "COMPLETED" {
				t.Fatal("a non-retryable commit failure was ignored")
			}
		} else {
			r.runUntil(t, execution.StepCompleted, 200)
			if n := len(r.g.Messages()); n != 2 {
				t.Fatalf("%d requests: the unit was not dispatched exactly once", n)
			}
		}
		done()
	}
}

// T18.143 — the send fails before transmission: the dispatch is
// DEFINITELY_NOT_SENT and is safely resent once.
func TestT18143SendFailureBeforeTransmission(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	p18ArmAtDispatch(r, reg, faults.Action{Point: faults.BeforeWorkerSend, Times: 1, Kind: faults.KindError})
	for i := 0; i < 20 && r.execStatus(t) != "COMPLETED"; i++ {
		if _, err := r.eng().Run(ctx, r.execID, 50); err != nil {
			t.Fatal(err)
		}
	}
	r.eng().SetStepHook(nil)
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("execution %s (%s)", r.execStatus(t), r.recoveryReason(t))
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND delivery = 'DEFINITELY_NOT_SENT'"); n != 1 {
		t.Fatalf("%d definitely-not-sent dispatches, want 1", n)
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("%d requests reached the worker, want planner + one unit send", n)
	}
}

// T18.144 — the connection is lost after transmission: DELIVERY_UNKNOWN,
// recovery required, never resent automatically.
func TestT18144DisconnectAfterSend(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	p18ArmAtDispatch(r, reg, faults.Action{Point: faults.AfterWorkerSend, Times: 1, Kind: faults.KindError})
	_, _ = r.eng().Run(ctx, r.execID, 50)
	_, _ = r.eng().Run(ctx, r.execID, 50)
	r.eng().SetStepHook(nil)
	if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "DELIVERY_UNKNOWN" {
		t.Fatalf("execution %s / %s", r.execStatus(t), p18RecoveryCondition(t, r))
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("%d requests: the unknown delivery was resent", n)
	}
}

// T18.145 — the response arrives but cannot be persisted: no result is
// fabricated, the request is not resent, and the dispatch stays
// reconcilable (WAITING) for transport-capability recovery.
func TestT18145ResponseNotPersisted(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	p18ArmAtDispatch(r, reg, faults.Action{Point: faults.AfterWorkerResponse, Times: 1, Kind: faults.KindError})
	_, _ = r.eng().Run(ctx, r.execID, 50)
	r.eng().SetStepHook(nil)
	unit := "SELECT %s FROM worker_dispatches WHERE project_id = '" + r.pid.String() + "' AND work_unit_id IS NOT NULL"
	if n := r.count(t, "SELECT count(*) FROM worker_results res JOIN worker_dispatches d ON d.id = res.dispatch_id WHERE d.project_id = '"+
		r.pid.String()+"' AND d.work_unit_id IS NOT NULL"); n != 0 {
		t.Fatal("a result was recorded without a persisted response")
	}
	var status string
	_ = r.h.DBScan(context.Background(), fmt.Sprintf(unit, "status"), &status)
	if status != "WAITING" {
		t.Fatalf("dispatch %s, want WAITING (reconcilable)", status)
	}
	_, _ = r.eng().Run(ctx, r.execID, 50)
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("%d requests: the request was resent", n)
	}
	if s := r.execStatus(t); s == "COMPLETED" {
		t.Fatal("completed on a fabricated result")
	}
}
