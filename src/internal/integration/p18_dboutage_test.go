package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/hardening"
)

// p18DBRig is a Phase 16 rig whose application reaches PostgreSQL only
// through a fault proxy.
func p18DBRig(t *testing.T) (*p16Rig, *pgProxy) {
	t.Helper()
	px := newPGProxy(t)
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) { px.configure(c) }})
	return r, px
}

// p18CutAt cuts the database the first time the engine reaches
// (action, stage).
func p18CutAt(r *p16Rig, px *pgProxy, action execution.StepAction, stage string) *bool {
	cut := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if a == action && s == stage && !cut {
			cut = true
			px.Cut()
		}
		return nil
	})
	return &cut
}

// T18.32 / T18.33 — the database goes away right before durable intent
// (the dispatch step's intent) or right after it (before the worker
// service records its own dispatch row): no worker request is sent while
// intent cannot be recorded; after recovery the unit is dispatched once.
func TestT1832NoDispatchWithoutDurableIntent(t *testing.T) {
	for _, stage := range []string{"intent", "dispatch"} {
		t.Run(stage, func(t *testing.T) {
			r, px := p18DBRig(t)
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			r.queue("complete")
			cut := p18CutAt(r, px, execution.ActionDispatch, stage)
			res, err := r.eng().Run(context.Background(), r.execID, 100)
			if !*cut || err == nil {
				t.Fatalf("cut %v err %v: %s", *cut, err, p16Trace(res))
			}
			// Only the planner request went out.
			if n := len(r.g.Messages()); n != 1 {
				t.Fatalf("worker messages while the database was down: %d", n)
			}
			if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL"); n != 0 {
				t.Fatalf("%d unit dispatch rows", n)
			}
			r.eng().SetStepHook(nil)
			px.Restore()
			r.runUntil(t, execution.StepCompleted, 100)
			if n := len(r.g.Messages()); n != 2 {
				t.Fatalf("worker messages %d, want 2 (one per request)", n)
			}
			if s := r.unitStatuses(t, 1); s != "a:DONE" {
				t.Fatalf("units %s", s)
			}
		})
	}
}

// T18.34 — the database is lost after the worker request was sent and
// before its response could be recorded: the outcome is ambiguous. Once
// the database returns it is reconciled to DELIVERY_UNKNOWN (recovery),
// never blindly replayed — in the same instance at once, and in a
// restarted instance once the dead instance's dispatch lease expires.
func TestT1834NoBlindReplayAfterDBLoss(t *testing.T) {
	for _, restartFirst := range []bool{false, true} {
		name := "same instance"
		if restartFirst {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			px := newPGProxy(t)
			r, restart := p16RestartRig(t, p16Options{config: func(c *config.Config) { px.configure(c) },
				// A short driver lease: the crashed instance's lease could not
				// be released while the database was down (WBS 18.12).
				engine: func(es *execution.EngineSettings, _ *hardening.Settings) { es.DriverLease = time.Second }})
			// A short dispatch lease, so a restarted instance may take over.
			r.exec(t, "INSERT INTO project_worker_settings (project_id, session_policy, concurrent_send, lease_ms, wait_ms) VALUES ('"+
				r.pid.String()+"', 'SINGLE_ACTIVE_PER_PROJECT', 'REJECT', 1000, 0) ON CONFLICT (project_id) DO UPDATE SET lease_ms = 1000")
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			hold := make(chan struct{})
			c := p16Reply("complete", "done", "")
			r.g.Script(hermestest.Behavior{Content: &c, Hold: hold})
			errc := make(chan error, 1)
			go func() {
				_, err := r.eng().Run(context.Background(), r.execID, 100)
				errc <- err
			}()
			deadline := time.Now().Add(10 * time.Second)
			for len(r.g.Messages()) < 2 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if len(r.g.Messages()) != 2 {
				t.Fatalf("unit request never reached the worker (%d messages)", len(r.g.Messages()))
			}
			px.Cut()
			close(hold) // the response arrives while the database is down
			if err := <-errc; err == nil {
				t.Fatal("run succeeded without a database")
			}
			px.Restore()
			if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND status = 'COMPLETED'"); n != 0 {
				t.Fatal("a response was recorded while the database was down")
			}
			if restartFirst {
				restart()
			} else {
				// While another live instance holds the dispatch lease the
				// call may still be running there: it is not reconciled.
				r.exec(t, "UPDATE worker_session_leases SET owner = 'another-instance', expires_at = now() + interval '1 hour' WHERE dispatch_id IS NOT NULL")
				_, _ = r.eng().Run(context.Background(), r.execID, 20)
				var st string
				_ = r.h.DBScan(context.Background(), "SELECT status FROM worker_dispatches WHERE work_unit_id IS NOT NULL", &st)
				if st != "WAITING" || r.execStatus(t) != "WAITING" {
					t.Fatalf("reconciled under a live foreign lease: dispatch %s execution %s", st, r.execStatus(t))
				}
				r.exec(t, "UPDATE worker_session_leases SET acquired_at = now() - interval '2 seconds', expires_at = now() - interval '1 second' WHERE dispatch_id IS NOT NULL")
			}
			var res []execution.StepResult
			for i := 0; i < 20 && r.execStatus(t) != "RECOVERY_REQUIRED"; i++ {
				var rerr error
				res, rerr = r.eng().Run(context.Background(), r.execID, 20)
				if rerr != nil && i == 19 {
					t.Logf("run: %v", rerr)
				}
				if r.execStatus(t) != "RECOVERY_REQUIRED" {
					time.Sleep(200 * time.Millisecond) // a live foreign lease is waited out
				}
			}
			if n := len(r.g.Messages()); n != 2 {
				t.Fatalf("request replayed: %d messages (%s)", n, p16Trace(res))
			}
			var rows string
			_ = r.h.DBScan(context.Background(), "SELECT string_agg(status || '/' || COALESCE(delivery, '-'), ',') FROM worker_dispatches WHERE work_unit_id IS NOT NULL", &rows)
			if r.execStatus(t) != "RECOVERY_REQUIRED" || !strings.Contains(strings.ToLower(r.recoveryReason(t)), "delivery unknown") || rows != "UNKNOWN/DELIVERY_UNKNOWN" {
				t.Fatalf("status %s reason %q dispatches %s; trace %s", r.execStatus(t), r.recoveryReason(t), rows, p16Trace(res))
			}
			if strings.Contains(r.unitStatuses(t, 1), "DONE") {
				t.Fatal("ambiguous dispatch marked the unit DONE")
			}
		})
	}
}

// T18.37 — PostgreSQL "restarts" (every connection dropped, new ones
// refused for a while) between and during steps: the pool reconnects, the
// engine reconstructs authoritative state from the database, and the
// execution completes with no duplicated work.
func TestT1837DatabaseRestart(t *testing.T) {
	r, px := p18DBRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	outages := 0
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if s == "intent" && (a == execution.ActionSelectUnit || a == execution.ActionRunUnitValidation) && outages < 3 {
			outages++
			px.Cut()
			go func() { time.Sleep(300 * time.Millisecond); px.Restore() }()
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	var res []execution.StepResult
	for i := 0; i < 20 && last(res) != execution.StepCompleted; i++ {
		res, _ = r.eng().Run(context.Background(), r.execID, 100)
		if last(res) != execution.StepCompleted {
			time.Sleep(350 * time.Millisecond)
		}
	}
	if last(res) != execution.StepCompleted || outages < 3 {
		t.Fatalf("outages %d, final %s; recovery %q", outages, p16Trace(res), r.recoveryReason(t))
	}
	if n := len(r.g.Messages()); n != 3 {
		t.Fatalf("worker messages %d, want 3", n)
	}
	evs := r.events(t)
	if countOf(evs, "WORK_UNIT_DONE") != 2 || countOf(evs, "PROJECT_COMPLETED") != 1 {
		t.Fatalf("duplicated effects: %v", evs)
	}
	if !r.h.App.IsReady() {
		t.Fatal("application not ready after the database returned")
	}
}
