package integration

import (
	"context"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
)

// T18.141 (P18-76) — fault-point coverage: a scripted two-phase project
// run passes through every fault point at least once, so each can be
// armed by the crash / chaos suites.
func TestT18141FaultPointsReachable(t *testing.T) {
	r := p16NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}})
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	for _, k := range []string{"a", "b"} {
		r.queuePlan(r.planJSON(p16Unit{key: k}))
		r.queue("complete")
	}
	for i := 0; i < 400; i++ {
		res, err := r.eng().Run(ctx, r.execID, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) > 0 && res[len(res)-1].Outcome == execution.StepCompleted {
			break
		}
		if s := r.execStatus(t); s != "RUNNING" && s != "WAITING" {
			t.Fatalf("execution %s: %s", s, r.recoveryReason(t))
		}
	}
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("execution %s", r.execStatus(t))
	}
	// fs_rename belongs to state-file materialization, which this engine
	// run does not perform; it is exercised with real process kills in
	// internal/service (TestT1855KillBeforeRename).
	offPath := map[faults.Point]bool{faults.FSRename: true}
	for _, p := range faults.Points() {
		if reg.Hits(p) == 0 && !offPath[p] {
			t.Errorf("fault point %s never reached", p)
		}
	}
}
