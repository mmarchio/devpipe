package integration

import (
	"context"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
)

// p18CrashFault runs the engine with a simulated process crash (KindCrash)
// armed at point the first time the engine reaches (action, "intent"),
// then restarts the instance.
func p18CrashFault(t *testing.T, r *p16Rig, restart func() *Harness, action execution.StepAction, point faults.Point) {
	t.Helper()
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	armed := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == action && s == "intent" && !armed {
			armed = true
			reg.Arm(faults.Action{Point: point, Times: 1, Kind: faults.KindCrash})
		}
		return nil
	})
	crashed := false
	for i := 0; i < 50 && !crashed; i++ {
		res, err := r.eng().Run(ctx, r.execID, 20)
		crashed = faults.IsCrash(err)
		if err != nil && !crashed {
			t.Fatalf("run: %v", err)
		}
		if len(res) > 0 && res[len(res)-1].Outcome.StopsRun() && !crashed && res[len(res)-1].Outcome != execution.StepWaiting {
			break
		}
	}
	r.eng().SetStepHook(nil)
	if !armed || !crashed {
		t.Fatalf("setup: %s reached %v, crash fired %v", action, armed, crashed)
	}
	restart()
}

// T18.148 — crash after the unit is DONE, before the next step: on
// restart the next unit is selected; the finished unit is not rerun.
func TestT18148CrashAfterUnitDone(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	p18CrashFault(t, r, restart, execution.ActionUnitGates, faults.AfterTransition)
	r.runUntil(t, execution.StepCompleted, 300)
	if n := r.count(t, `SELECT count(*) FROM worker_dispatches d JOIN work_units w ON w.id = d.work_unit_id
		WHERE d.project_id = '`+r.pid.String()+`' AND w.unit_key = 'a'`); n != 1 {
		t.Fatalf("unit a dispatched %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+r.pid.String()+"' AND kind = 'UNIT' AND status = 'CREATED'"); n != 2 {
		t.Fatalf("%d unit checkpoints", n)
	}
}

// T18.149 — crash during phase advancement: the advancement transaction
// rolls back; after restart exactly one phase is current at every point
// and the project completes.
func TestT18149CrashDuringPhaseAdvance(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}})
	for _, k := range []string{"a", "b"} {
		r.queuePlan(r.planJSON(p16Unit{key: k}))
		r.queue("complete")
	}
	p18CrashFault(t, r, restart, execution.ActionPhaseValidation, faults.PhaseAdvance)
	if n := r.count(t, "SELECT count(*) FROM phases WHERE project_id = '"+r.pid.String()+"' AND status NOT IN ('DONE', 'PENDING', 'COMPLETED')"); n > 1 {
		t.Fatalf("%d current phases after the crash", n)
	}
	r.runUntil(t, execution.StepCompleted, 300)
	if n := r.count(t, "SELECT count(*) FROM phases WHERE project_id = '"+r.pid.String()+"' AND status = 'DONE'"); n != 2 {
		t.Fatalf("%d phases DONE", n)
	}
}

// T18.150 — crash during project completion: the completion transaction
// rolls back; after restart the execution converges to COMPLETED with a
// single completion record.
func TestT18150CrashDuringCompletion(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18CrashFault(t, r, restart, execution.ActionFinalCheckpoint, faults.ProjectCompletion)
	if r.execStatus(t) == "COMPLETED" {
		t.Fatal("setup: completed despite the crash")
	}
	r.runUntil(t, execution.StepCompleted, 300)
	if n := r.count(t, "SELECT count(*) FROM execution_completions WHERE execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d completion records", n)
	}
	var st string
	_ = r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.pid.String()+"'", &st)
	if st != "COMPLETED" {
		t.Fatalf("project %s", st)
	}
}
