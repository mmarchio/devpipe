package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/health"
)

// p18DiskAt makes every monitored filesystem report pct % used.
func p18DiskAt(r *p16Rig, pct uint64) {
	r.h.App.DiskMonitor().SetStatFS(func(string) (uint64, uint64, error) { return 1000, 1000 - pct*10, nil })
	r.h.App.DiskMonitor().Check(context.Background())
}

// T18.97 + decision U7 — crossing the warning threshold raises
// DISK_THRESHOLD only; crossing the critical one holds new worker
// dispatches and validation runs (the execution WAITs, nothing is sent or
// run); freeing space lets the execution resume by itself.
func TestT1897DiskThresholds(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")

	p18DiskAt(r, 90)
	if a := p18Alert(r.h.App.Alerts().Evaluate(ctx), health.AlertDiskThreshold); !a.Active {
		t.Fatalf("warning not raised: %+v", a)
	}
	if b := r.h.App.WorkGate().Blocked(); b != "" {
		t.Fatalf("a warning blocked work: %s", b)
	}
	if s := p18Subsystem(r.h.App.SystemHealth().Report(ctx), "disk"); s.Status != health.Degraded {
		t.Fatalf("disk health at 90%%: %+v", s)
	}

	// Critical before the first dispatch: nothing is sent.
	p18DiskAt(r, 96)
	_, _ = r.eng().Run(ctx, r.execID, 10)
	if n := len(r.g.Messages()); n != 0 {
		t.Fatalf("%d worker requests at critical disk usage", n)
	}
	var wait string
	_ = r.h.DBScan(ctx, "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &wait)
	if r.execStatus(t) != "WAITING" || !strings.Contains(wait, "disk usage") {
		t.Fatalf("execution %s / %q", r.execStatus(t), wait)
	}

	// Space freed: work resumes; critical again right before validation
	// holds the validation run.
	p18DiskAt(r, 50)
	held := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionRunUnitValidation && s == "intent" && !held {
			held = true
			p18DiskAt(r, 97)
		}
		return nil
	})
	_, _ = r.eng().Run(ctx, r.execID, 50)
	r.eng().SetStepHook(nil)
	if !held {
		t.Fatal("setup: validation not reached")
	}
	_ = r.h.DBScan(ctx, "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &wait)
	if r.execStatus(t) != "WAITING" || !strings.Contains(wait, "validation held") {
		t.Fatalf("validation not held: %s / %q", r.execStatus(t), wait)
	}
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d validation runs started at critical disk usage", n)
	}
	p18DiskAt(r, 50)
	if b := r.h.App.WorkGate().Blocked(); b != "" {
		t.Fatalf("gate still closed after freeing space: %s", b)
	}
	// And right before the checkpoint commit: the checkpoint is held.
	before := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE execution_id = '"+r.execID.String()+"'")
	held = false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == "intent" && !held {
			held = true
			p18DiskAt(r, 97)
		}
		return nil
	})
	_, _ = r.eng().Run(ctx, r.execID, 50)
	r.eng().SetStepHook(nil)
	_ = r.h.DBScan(ctx, "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &wait)
	if !held || r.execStatus(t) != "WAITING" || !strings.Contains(wait, "checkpoint held") {
		t.Fatalf("checkpoint not held: %v %s / %q", held, r.execStatus(t), wait)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE execution_id = '"+r.execID.String()+"'"); n != before {
		t.Fatal("a checkpoint was created at critical disk usage")
	}
	p18DiskAt(r, 50)
	r.runUntil(t, execution.StepCompleted, 200)
}
