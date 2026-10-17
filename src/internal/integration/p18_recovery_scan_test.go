package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/recovery"
	"overseer/internal/service"
)

// p18Scan runs the startup recovery scan as a fresh instance would,
// shortly after the old instance's leases expired.
func p18Scan(t *testing.T, r *p16Rig, after time.Duration) *service.ScanResult {
	t.Helper()
	s := service.NewStartupRecoveryScan(dbrepo.NewRecoveryScanRepository(r.h.App.DB().Raw()), "new-instance",
		func() time.Time { return time.Now().Add(after) })
	res, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func p18Conditions(res *service.ScanResult) string {
	var out []string
	for _, f := range res.Facts {
		out = append(out, string(f.Condition))
	}
	return strings.Join(out, ",")
}

func p18CrashAt(t *testing.T, r *p16Rig, action execution.StepAction, stage string) {
	t.Helper()
	crashed := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if a == action && s == stage && !crashed {
			crashed = true
			return errP16Crash
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	_, _ = r.eng().Run(context.Background(), r.execID, 100)
	if !crashed {
		t.Fatalf("crash point %s/%s not reached", action, stage)
	}
}

// T18.6 — after a crash, the startup recovery scan identifies every
// unfinished durable operation; inspecting changes nothing.
func TestT186StartupRecoveryScan(t *testing.T) {
	t.Run("validation running + expired lease", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18CrashAt(t, r, execution.ActionRunUnitValidation, "validation")
		// A real crash leaves the dead instance's lease behind.
		r.exec(t, "UPDATE executions SET driver_owner = 'dead-instance', driver_expires_at = now() + interval '1 minute' WHERE id = '"+r.execID.String()+"'")
		before := p18Fingerprint(t, r)
		if got := p18Conditions(p18Scan(t, r, 2*time.Hour)); !strings.Contains(got, "EXPIRED_LEASE") || !strings.Contains(got, "RUNNING_VALIDATION") {
			t.Fatalf("conditions %s", got)
		}
		// A live lease of another instance is left alone (18.13).
		if res := p18Scan(t, r, 0); len(res.LiveLeases) != 1 || strings.Contains(p18Conditions(res), "EXPIRED_LEASE") {
			t.Fatalf("live lease: %+v", res)
		}
		if p18Fingerprint(t, r) != before {
			t.Fatal("scan mutated the database")
		}
	})
	t.Run("pending checkpoint", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18CrashAt(t, r, execution.ActionUnitGates, "checkpoint-committed")
		if got := p18Conditions(p18Scan(t, r, 2*time.Hour)); !strings.Contains(got, "PENDING_CHECKPOINT") {
			t.Fatalf("conditions %s", got)
		}
	})
	t.Run("dispatch intent without send", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18CrashAt(t, r, execution.ActionDispatch, "dispatch")
		res := p18Scan(t, r, 2*time.Hour)
		if got := p18Conditions(res); !strings.Contains(got, "DISPATCH_NOT_SENT") {
			t.Fatalf("conditions %s", got)
		}
		for _, f := range res.Facts {
			if f.Condition == recovery.DispatchNotSent && recovery.ClassOf(f.Condition) != recovery.SafeToRetry {
				t.Fatal("unsent dispatch not safe to retry")
			}
		}
	})
	t.Run("delivery unknown", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.g.Script(hermestest.Behavior{Drop: true})
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		res := p18Scan(t, r, 0)
		n := 0
		for _, f := range res.Facts {
			if f.Condition == recovery.DeliveryUnknown {
				n++
				if p := recovery.PlanFor(f); p.Classification == recovery.SafeToRetry || p.Safe(recovery.ActResend) {
					t.Fatal("unknown delivery classified retryable")
				}
			}
		}
		if n != 2 { // the execution's recovery reason + the ambiguous dispatch row
			t.Fatalf("conditions %s", p18Conditions(res))
		}
	})
	t.Run("in-flight dispatch + orphan precompute", func(t *testing.T) {
		r := p16NewRig(t)
		hold := make(chan struct{})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		c := p16Reply("complete", "late", "")
		r.g.Script(hermestest.Behavior{Content: &c, Hold: hold})
		done := make(chan struct{})
		go func() { defer close(done); _, _ = r.eng().Run(context.Background(), r.execID, 100) }()
		deadline := time.Now().Add(10 * time.Second)
		// Wait for the held unit dispatch (the planner's row also passes
		// through WAITING briefly on its way to COMPLETED).
		for r.count(t, "SELECT count(*) FROM worker_dispatches WHERE status = 'WAITING' AND work_unit_id IS NOT NULL") == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		r.exec(t, "INSERT INTO precompute_jobs (id, project_id, trigger, status, owner, created_at, heartbeat_at) VALUES (gen_random_uuid(), '"+
			r.pid.String()+"', 'TEST', 'RUNNING', 'dead-instance', now(), now())")
		got := p18Conditions(p18Scan(t, r, 0))
		close(hold)
		<-done
		if !strings.Contains(got, "INFLIGHT_DISPATCH") || !strings.Contains(got, "ORPHAN_PRECOMPUTE") {
			t.Fatalf("conditions %s", got)
		}
	})
}

// p18Fingerprint summarizes mutable workflow state (row counts +
// versions). Audit rows are excluded: every request, including a dry run
// or a rejected one, is audited by design.
func p18Fingerprint(t *testing.T, r *p16Rig) string {
	t.Helper()
	var s string
	_ = r.h.DBScan(context.Background(), `SELECT concat_ws('|',
		(SELECT string_agg(id::text || status || version::text || COALESCE(driver_owner, ''), ',' ORDER BY id) FROM executions),
		(SELECT count(*) FROM execution_steps), (SELECT count(*) FROM worker_dispatches), (SELECT count(*) FROM validation_runs),
		(SELECT count(*) FROM repository_checkpoints), (SELECT count(*) FROM execution_events), (SELECT count(*) FROM recovery_actions), (SELECT count(*) FROM recovery_plans))`, &s)
	return s
}
