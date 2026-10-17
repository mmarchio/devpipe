package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/hardening"
)

// TestP16166DependencyGraph — WBS 16.166 / 16.20–16.22 / 16.60: A, B → C → D;
// C is claimed only after A and B are DONE.
func TestP16166DependencyGraph(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b"}, p16Unit{key: "c", kind: "COMPOSITION", deps: []string{"a", "b"}},
		p16Unit{key: "d", kind: "INTEGRATION", deps: []string{"c"}}))
	for i := 0; i < 4; i++ {
		r.queue("complete")
	}
	var order []string
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" {
			order = append(order, r.unitStatuses(t, 1))
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 200)
	// When C is dispatched, A and B are DONE; when D is dispatched, C is.
	want := []string{"a:EXECUTING,b:PENDING,c:PENDING,d:PENDING", "a:DONE,b:EXECUTING,c:PENDING,d:PENDING",
		"a:DONE,b:DONE,c:EXECUTING,d:PENDING", "a:DONE,b:DONE,c:DONE,d:EXECUTING"}
	if strings.Join(order, " | ") != strings.Join(want, " | ") {
		t.Fatalf("dispatch order\n got %v\nwant %v", order, want)
	}
}

// TestP1661Deadlock — WBS 16.61: pending units with none executable is a
// recovery condition, never phase completion.
func TestP1661Deadlock(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionMoreWork && stage == "intent" {
			// An inconsistent plan: b is blocked by a unit that will never run.
			r.exec(t, "UPDATE work_units SET status = 'BLOCKED' WHERE unit_key = 'b' AND "+r.where())
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepRecoveryRequired, 100)
	if !strings.Contains(r.recoveryReason(t), "deadlock") || r.phaseStatus(t, 1) == "DONE" {
		t.Fatalf("reason %q phase %s", r.recoveryReason(t), r.phaseStatus(t, 1))
	}
}

// TestP16Planning — WBS 16.11–16.19: malformed / invalid / policy-violating
// plans are rejected (no plan, attempts bounded); a valid plan activates
// atomically and is bound to the WBS version.
func TestP16Planning(t *testing.T) {
	t.Run("malformed and invalid rejected then valid accepted", func(t *testing.T) {
		r := p16NewRig(t)
		r.queueRaw(p16Reply("complete", "Planned.", `"plan":{"units":"nope"}`), 1000)                         // malformed
		r.queuePlan(`{"units":[{"key":"a","name":"A","kind":"ATOMIC","objective":"o","depends_on":["zz"]}]}`) // invalid
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		r.runUntil(t, execution.StepCompleted, 150)
		if n := r.count(t, "SELECT count(*) FROM plan_candidates WHERE execution_id = '"+r.execID.String()+"' AND status = 'REJECTED' AND plan_id IS NULL"); n != 2 {
			t.Fatalf("rejected candidates = %d", n)
		}
		if n := r.count(t, "SELECT count(*) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where()); n != 1 {
			t.Fatalf("plans = %d (an invalid plan was activated)", n)
		}
		var wbsV int
		if err := r.h.DBScan(context.Background(), "SELECT ep.wbs_version FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where(), &wbsV); err != nil || wbsV != 1 {
			t.Fatalf("plan WBS version %d %v", wbsV, err)
		}
	})
	t.Run("atomic policy violations bounded", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.MaxPlanAttempts = 2 }})
		bad := `{"units":[{"key":"a","name":"A","kind":"ATOMIC","objective":"o"}]}` // no tests, criteria, single_invocation
		r.queuePlan(bad)
		r.queuePlan(bad)
		r.runUntil(t, execution.StepRecoveryRequired, 50)
		if !strings.Contains(r.recoveryReason(t), "planning attempts exhausted") {
			t.Fatalf("reason %q", r.recoveryReason(t))
		}
		var report string
		_ = r.h.DBScan(context.Background(), "SELECT report::text FROM plan_candidates WHERE execution_id = '"+r.execID.String()+"' ORDER BY created_at LIMIT 1", &report)
		for _, code := range []string{"NOT_INDEPENDENTLY_TESTABLE", "MISSING_ACCEPTANCE_CRITERIA", "NOT_SINGLE_INVOCATION"} {
			if !strings.Contains(report, code) {
				t.Fatalf("report lacks %s: %s", code, report)
			}
		}
		if n := r.count(t, "SELECT count(*) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where()); n != 0 {
			t.Fatal("plan created from rejected candidates")
		}
	})
	t.Run("stale WBS during planning", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		edited := false
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if a == execution.ActionPlanValidate && stage == "intent" && !edited {
				edited = true
				r.appendWBS(t, 1) // the WBS changes while the plan was generated
			}
			return nil
		})
		defer r.eng().SetStepHook(nil)
		r.runUntil(t, execution.StepCompleted, 150)
		var wbsV int
		if err := r.h.DBScan(context.Background(), "SELECT ep.wbs_version FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where(), &wbsV); err != nil || wbsV != 2 {
			t.Fatalf("plan bound to WBS v%d (%v), want v2", wbsV, err)
		}
		if n := r.count(t, "SELECT count(*) FROM plan_candidates WHERE execution_id = '"+r.execID.String()+"' AND report::text LIKE '%STALE_WBS%'"); n != 1 {
			t.Fatal("stale candidate not rejected")
		}
	})
}

// TestP16167PlanAmendment — WBS 16.167 / 16.63–16.68: the phase reaches
// 100 %, phase validation fails, repair adds units (denominator grows),
// the new units execute, the phase passes; the old plan is unchanged.
func TestP16167PlanAmendment(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b"}))
	r.queue("complete")
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "r1", deps: []string{"b"}}))
	r.queue("complete")
	r.setCheck(t, "phase", 1)
	var progressAtAmend string
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionSelectUnit && stage == "intent" && strings.Contains(r.unitStatuses(t, 1), "r1:PENDING") {
			progressAtAmend = r.unitStatuses(t, 1)
			r.setCheck(t, "phase", 0) // the repair makes the phase pass
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 250)
	if progressAtAmend != "a:DONE,b:DONE,r1:PENDING" {
		t.Fatalf("progress at amendment %q (want 2/3)", progressAtAmend)
	}
	var kinds string
	if err := r.h.DBScan(context.Background(), "SELECT string_agg(ep.kind || ':' || ep.status, ',' ORDER BY ep.version) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where(), &kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "INITIAL:SUPERSEDED,AMENDMENT:COMPLETED" {
		t.Fatalf("plans %s", kinds)
	}
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans ep ON ep.id = w.execution_plan_id WHERE ep.kind = 'INITIAL' AND w."+r.where()+" AND w.status = 'DONE'"); n != 2 {
		t.Fatal("historical plan units changed")
	}
	evs := r.events(t)
	if countOf(evs, "PHASE_VALIDATION_FAILED") != 1 || countOf(evs, "PLAN_AMENDED") != 1 || r.phaseStatus(t, 1) != "DONE" {
		t.Fatalf("events %v", evs)
	}
}

// TestP16168AppendedPhase — WBS 16.168 / 16.9 / 16.69–16.71: a phase
// appended while phase 1 executes doesn't preempt it; order 1 → 2 → 3
// (ordering gaps don't matter).
func TestP16168AppendedPhase(t *testing.T) {
	r := p16NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {3, "Three", "PENDING", 1}}})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "b"}))
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "c"}))
	r.queue("complete")
	appended := false
	var unitBefore, unitAfter string
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" && !appended {
			appended = true
			unitBefore = r.unitStatuses(t, 1)
			id := seedPhaseStatus(t, r.h, r.pid, 7, "Seven (appended)", "PENDING")
			r.phases[7] = id
			r.appendWBS(t, 7)
			unitAfter = r.unitStatuses(t, 1)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 300)
	if unitBefore != unitAfter {
		t.Fatal("appending a phase changed the active unit")
	}
	var order string
	if err := r.h.DBScan(context.Background(), `SELECT string_agg(p.name, ',' ORDER BY ep.created_at) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p.`+r.where(), &order); err != nil {
		t.Fatal(err)
	}
	if order != "One,Three,Seven (appended)" {
		t.Fatalf("phase order %s", order)
	}
	for _, o := range []int{1, 3, 7} {
		if r.phaseStatus(t, o) != "DONE" {
			t.Fatalf("phase %d %s", o, r.phaseStatus(t, o))
		}
	}
}
