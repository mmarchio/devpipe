package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
)

// T18.231 — invariant acceptance: each corruption, introduced right before
// the unit's worker request, is caught before any further side effect
// (INVARIANT_VIOLATION, nothing sent after the planner's request) — or is
// impossible to create because the database refuses it.
func TestT18231InvariantAcceptance(t *testing.T) {
	cases := map[string]func(t *testing.T, r *p16Rig, other *p16Rig) error{
		"two current units": func(t *testing.T, r *p16Rig, _ *p16Rig) error {
			_, err := r.h.DBExec(context.Background(), `UPDATE work_units SET status = 'EXECUTING', claimed_execution_id = '`+r.execID.String()+`'
				WHERE project_id = '`+r.pid.String()+`' AND unit_key = 'b'`)
			return err
		},
		"DONE without validation or checkpoint": func(t *testing.T, r *p16Rig, _ *p16Rig) error {
			r.exec(t, "SET session_replication_role = replica; UPDATE work_units SET status = 'DONE' WHERE project_id = '"+r.pid.String()+"' AND unit_key = 'b'; SET session_replication_role = origin")
			return nil
		},
		"current unit on a superseded plan": func(t *testing.T, r *p16Rig, _ *p16Rig) error {
			r.exec(t, "SET session_replication_role = replica; UPDATE execution_plans SET status = 'SUPERSEDED' WHERE id = (SELECT execution_plan_id FROM work_units WHERE project_id = '"+r.pid.String()+"' AND unit_key = 'a'); SET session_replication_role = origin")
			return nil
		},
		"plan against a stale WBS": func(t *testing.T, r *p16Rig, _ *p16Rig) error {
			r.appendWBS(t, 1)
			return nil
		},
		"dispatch of another project": func(t *testing.T, r *p16Rig, other *p16Rig) error {
			r.exec(t, `SET session_replication_role = replica; UPDATE worker_dispatches SET work_unit_id =
				(SELECT id FROM work_units WHERE project_id = '`+other.pid.String()+`' LIMIT 1) WHERE project_id = '`+r.pid.String()+`';
				SET session_replication_role = origin`)
			return nil
		},
		"second current phase": func(t *testing.T, r *p16Rig, _ *p16Rig) error {
			p18SecondExecutingPhase(t, r)
			return nil
		},
	}
	for name, corrupt := range cases {
		corrupt := corrupt
		t.Run(name, func(t *testing.T) {
			r := p16NewRig(t)
			other := &p16Rig{p15Rig: p15Build(t, r.h, r.g, r.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
			p16Configure(t, other, p16Options{})
			other.queuePlan(other.planJSON(p16Unit{key: "z"}))
			other.queue("complete")
			other.runUntil(t, execution.StepCompleted, 100)
			sent := len(r.g.Messages())
			r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
			r.queue("complete")
			var refused error
			fired := false
			r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
				if a == execution.ActionDispatch && s == "intent" && !fired {
					fired = true
					refused = corrupt(t, r, other)
				}
				return nil
			})
			_, _ = r.eng().Run(context.Background(), r.execID, 50)
			r.eng().SetStepHook(nil)
			if !fired {
				t.Fatal("setup: dispatch not reached")
			}
			if refused != nil {
				t.Logf("the database refused the corruption: %v", refused)
				return
			}
			if name == "plan against a stale WBS" {
				// A WBS amendment is a supported flow (WBS 16.11; the
				// invariant is a WARNING, "replanning pending"): the unit
				// step already in flight finishes, and the phase is replanned
				// at the next control point — no further unit of the stale
				// plan is dispatched.
				if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND work_unit_id IS NOT NULL"); n > 1 {
					t.Fatalf("%d units of the stale plan were dispatched", n)
				}
				// Caught either way: the engine's unit invariants stop the
				// execution at the next side effect, or the phase replans.
				msgs := r.g.Messages()[sent:]
				replanned := len(msgs) >= 3
				for i, m := range msgs {
					if i >= 2 && m.Metadata["state_key"] != "PHASE_PLAN" {
						t.Fatalf("non-planning request %q after the WBS changed", m.Metadata["state_key"])
					}
				}
				if !replanned && (r.execStatus(t) != "RECOVERY_REQUIRED" || !strings.Contains(r.recoveryReason(t), "invariant")) {
					t.Fatalf("stale plan neither replanned nor stopped: %s (%s)", r.execStatus(t), r.recoveryReason(t))
				}
				return
			}
			if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "INVARIANT_VIOLATION" {
				t.Fatalf("not caught: %s / %q (%s)", r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t))
			}
			if n := len(r.g.Messages()) - sent; n != 1 {
				t.Fatalf("%d requests after the corruption (only the planner's expected)", n-1)
			}
		})
	}
}

// T18.232 (worker output) — an oversized worker reply is rejected by the
// transport bound instead of being loaded: no result is created from it
// and the unit is not DONE. (Worker / validation / compression
// concurrency: TestT18162 / TestT18163 / TestT1895; request size:
// TestP17127Security; validation output: the head+tail capture of
// internal/valrun; artifact loading: TestT1896LargeArtifactsBounded.)
func TestT18232OversizedWorkerOutput(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	c := p16Reply("complete", "Worker reports complete.", "")
	r.g.Script(hermestest.Behavior{Content: &c, Oversize: 64 << 20})
	_, _ = r.eng().Run(context.Background(), r.execID, 50)
	if n := r.count(t, "SELECT count(*) FROM worker_results r JOIN worker_dispatches d ON d.id = r.dispatch_id WHERE d.project_id = '"+r.pid.String()+"' AND d.work_unit_id IS NOT NULL"); n != 0 {
		t.Fatal("a result was created from an oversized reply")
	}
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+r.pid.String()+"' AND status = 'DONE'"); n != 0 {
		t.Fatal("unit DONE on an oversized reply")
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND work_unit_id IS NOT NULL AND status = 'COMPLETED'"); n != 0 {
		t.Fatal("the oversized reply was accepted as a completed dispatch")
	}
}
