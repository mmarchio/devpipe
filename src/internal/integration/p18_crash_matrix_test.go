package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/recovery"
)

// crashInjection is how one matrix location is produced for one
// operation: a crash returned from an engine stage hook, or a simulated
// crash at a fault point — armed when the operation's step intent is
// reached (or, for "before intent", its predecessor's).
type crashInjection struct {
	stage string       // engine hook stage ("" = fault point)
	point faults.Point // fault point
	pred  bool         // arm at the predecessor step (before intent)
}

type crashOp struct {
	action execution.StepAction
	// pred / after: the predecessor action, seen after `after` (the
	// predecessor action may occur several times, e.g. CONTROL).
	pred, after execution.StepAction
	locs        map[recovery.CrashLocation]crashInjection
}

func p18CrashOps() map[recovery.CrashOperation]crashOp {
	tr := func(m map[recovery.CrashLocation]crashInjection) map[recovery.CrashLocation]crashInjection {
		m[recovery.BeforeIntent] = crashInjection{point: faults.AfterTransition, pred: true}
		m[recovery.AfterIntent] = crashInjection{stage: "intent"}
		if _, ok := m[recovery.BeforeTransitionCrash]; !ok {
			m[recovery.BeforeTransitionCrash] = crashInjection{point: faults.BeforeTransition}
		}
		m[recovery.AfterTransitionCrash] = crashInjection{point: faults.AfterTransition}
		return m
	}
	return map[recovery.CrashOperation]crashOp{
		recovery.OpDispatch: {action: execution.ActionDispatch, pred: execution.ActionSelectUnit, locs: tr(map[recovery.CrashLocation]crashInjection{
			recovery.BeforeExternalAction:    {stage: "dispatch"},
			recovery.DuringExternalAction:    {point: faults.AfterWorkerSend},
			recovery.AfterExternalAction:     {point: faults.AfterWorkerResponse},
			recovery.BeforeResultPersistence: {point: faults.AfterWorkerResponse},
			recovery.AfterResultPersistence:  {stage: "dispatched"},
		})},
		recovery.OpValidation: {action: execution.ActionRunUnitValidation, pred: execution.ActionEvaluateResult, locs: tr(map[recovery.CrashLocation]crashInjection{
			recovery.BeforeExternalAction:    {stage: "validation"},
			recovery.DuringExternalAction:    {point: faults.BeforeValidation},
			recovery.AfterExternalAction:     {point: faults.AfterValidation},
			recovery.BeforeResultPersistence: {point: faults.AfterValidation},
			recovery.AfterResultPersistence:  {point: faults.BeforeTransition},
		})},
		recovery.OpCheckpoint: {action: execution.ActionUnitGates, pred: execution.ActionControl, after: execution.ActionRunUnitValidation,
			locs: tr(map[recovery.CrashLocation]crashInjection{
				recovery.BeforeExternalAction:    {point: faults.GitLock},
				recovery.DuringExternalAction:    {point: faults.BeforeGitCommit},
				recovery.AfterExternalAction:     {point: faults.AfterGitCommit},
				recovery.BeforeResultPersistence: {point: faults.AfterGitCommit},
				recovery.AfterResultPersistence:  {point: faults.BeforeTransition},
			})},
		recovery.OpTransition: {action: execution.ActionMoreWork, pred: execution.ActionUnitGates, locs: tr(map[recovery.CrashLocation]crashInjection{
			recovery.BeforeExternalAction:    {stage: "intent"},
			recovery.DuringExternalAction:    {stage: "intent"},
			recovery.AfterExternalAction:     {stage: "intent"},
			recovery.BeforeResultPersistence: {stage: "intent"},
			recovery.AfterResultPersistence:  {point: faults.BeforeTransition},
		})},
		recovery.OpPhaseAdvance: {action: execution.ActionPhaseValidation, pred: execution.ActionMoreWork, locs: tr(map[recovery.CrashLocation]crashInjection{
			recovery.BeforeExternalAction:    {stage: "validation"},
			recovery.DuringExternalAction:    {point: faults.BeforeValidation},
			recovery.AfterExternalAction:     {point: faults.AfterValidation},
			recovery.BeforeResultPersistence: {point: faults.AfterValidation},
			recovery.AfterResultPersistence:  {point: faults.PhaseAdvance},
			recovery.BeforeTransitionCrash:   {point: faults.PhaseAdvance},
		})},
		recovery.OpCompletion: {action: execution.ActionFinalCheckpoint, pred: execution.ActionFinalValidation, locs: tr(map[recovery.CrashLocation]crashInjection{
			recovery.BeforeExternalAction:    {point: faults.GitLock},
			recovery.DuringExternalAction:    {point: faults.BeforeGitCommit},
			recovery.AfterExternalAction:     {point: faults.AfterGitCommit},
			recovery.BeforeResultPersistence: {point: faults.AfterGitCommit},
			recovery.AfterResultPersistence:  {point: faults.ProjectCompletion},
			recovery.BeforeTransitionCrash:   {point: faults.ProjectCompletion},
		})},
	}
}

// T18.222 / T18.151 — the crash matrix: every operation × crash location
// is injected into a real run, the instance restarts, and the documented
// recovery happens — the execution converges with every side effect
// exactly once, or (worker request possibly processed) stops for an
// operator without resending.
func TestT18222CrashMatrix(t *testing.T) {
	ops := p18CrashOps()
	for _, c := range recovery.CrashMatrix() {
		c := c
		op := ops[c.Operation]
		inj, ok := op.locs[c.Location]
		if !ok {
			t.Fatalf("no injection for %s × %s", c.Operation, c.Location)
		}
		t.Run(string(c.Operation)+"/"+string(c.Location), func(t *testing.T) {
			r, restart := p16RestartRig(t, p16Options{})
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			r.queue("complete")
			reg := faults.NewRegistry()
			ctx, done := faults.WithRegistry(context.Background(), reg)
			defer done()
			armed, seenAfter := false, op.after == ""
			r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
				if a == op.after {
					seenAfter = true
				}
				target := op.action
				if inj.pred {
					target = op.pred
				}
				if armed || a != target || (inj.pred && !seenAfter) || (!inj.pred && a == op.pred && op.after != "" && !seenAfter) {
					if armed && inj.stage != "" && a == op.action && s == inj.stage {
						return errP16Crash
					}
					return nil
				}
				if s != "intent" {
					return nil
				}
				armed = true
				if inj.stage == "intent" {
					return errP16Crash
				}
				if inj.stage == "" {
					reg.Arm(faults.Action{Point: inj.point, Times: 1, Kind: faults.KindCrash})
				}
				return nil
			})
			crashed := false
			for i := 0; i < 40 && !crashed; i++ {
				res, err := r.eng().Run(ctx, r.execID, 20)
				if err != nil {
					crashed = armed
					if !crashed {
						t.Fatalf("run before arming: %v", err)
					}
					break
				}
				if len(res) > 0 && res[len(res)-1].Outcome == execution.StepCompleted {
					break
				}
			}
			r.eng().SetStepHook(nil)
			if !crashed {
				t.Fatalf("crash not produced (armed %v, status %s)", armed, r.execStatus(t))
			}
			restart()
			switch c.Outcome {
			case recovery.ConvergesOnce:
				r.runUntil(t, execution.StepCompleted, 300)
				p18AssertOnce(t, r)
			case recovery.StopsDeliveryUnknown:
				_, _ = r.eng().Run(context.Background(), r.execID, 50)
				cond := p18RecoveryCondition(t, r)
				if r.execStatus(t) != "RECOVERY_REQUIRED" || (cond != "DELIVERY_UNKNOWN" && cond != "RESPONSE_UNPERSISTED") {
					t.Fatalf("execution %s / %q (%s)", r.execStatus(t), cond, r.recoveryReason(t))
				}
				if n := len(r.g.Messages()); n != 2 {
					t.Fatalf("%d requests: resent", n)
				}
			}
		})
	}
}

// p18AssertOnce checks every side effect happened exactly once.
func p18AssertOnce(t *testing.T, r *p16Rig) {
	t.Helper()
	pid := r.pid.String()
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+pid+"' AND work_unit_id IS NOT NULL AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT'"); n != 1 {
		t.Fatalf("unit request sent %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+pid+"' AND kind = 'UNIT' AND status = 'CREATED'"); n != 1 {
		t.Fatalf("%d unit checkpoints", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_completions WHERE project_id = '"+pid+"'"); n != 1 {
		t.Fatalf("%d completion records", n)
	}
	if n := r.count(t, "SELECT count(*) FROM phases WHERE project_id = '"+pid+"' AND status <> 'DONE'"); n != 0 {
		t.Fatalf("%d phases not DONE", n)
	}
	c := exec.Command("git", "log", "--all", "--format=%B")
	c.Dir = r.dir
	b, _ := c.Output()
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Overseer-Checkpoint:") {
			if seen[l] {
				t.Fatalf("checkpoint committed twice: %s", l)
			}
			seen[l] = true
		}
	}
}

// p18FinalSummary describes a finished execution independently of how it
// got there.
func p18FinalSummary(t *testing.T, r *p16Rig) string {
	t.Helper()
	pid := r.pid.String()
	q := func(sql string) string { return itoa(r.count(t, sql)) }
	return strings.Join([]string{
		r.execStatus(t),
		q("SELECT count(*) FROM work_units WHERE project_id = '" + pid + "' AND status = 'DONE'"),
		q("SELECT count(*) FROM phases WHERE project_id = '" + pid + "' AND status = 'DONE'"),
		q("SELECT count(*) FROM repository_checkpoints WHERE project_id = '" + pid + "' AND kind = 'UNIT' AND status = 'CREATED'"),
		q("SELECT count(*) FROM worker_dispatches WHERE project_id = '" + pid + "' AND work_unit_id IS NOT NULL AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT'"),
		q("SELECT count(*) FROM execution_completions WHERE project_id = '" + pid + "'"),
	}, "/")
}

// T18.151 — a seeded randomized crash schedule over a multi-unit,
// two-phase project (each crash followed by a new instance) ends in a
// state equivalent to the run without crashes.
func TestT18151RandomCrashScheduleEquivalent(t *testing.T) {
	phases := []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}
	script := func(r *p16Rig) {
		r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
		r.queue("complete")
		r.queue("complete")
		r.queuePlan(r.planJSON(p16Unit{key: "c"}))
		r.queue("complete")
	}
	base := p16NewRigWith(t, p16Options{phases: phases})
	script(base)
	base.runUntil(t, execution.StepCompleted, 400)
	want := p18FinalSummary(t, base)

	points := []faults.Point{faults.BeforeDBCommit, faults.AfterDBCommit, faults.BeforeTransition, faults.AfterTransition,
		faults.BeforeValidation, faults.AfterValidation, faults.BeforeGitCommit, faults.AfterGitCommit, faults.PhaseAdvance,
		faults.ProjectCompletion}
	total := 0
	defer func() {
		if total < 3 {
			t.Errorf("the schedules produced only %d crashes", total)
		}
	}()
	for _, seed := range []int64{1, 7, 42, 99, 123} {
		seed := seed
		t.Run("seed"+itoa(int(seed)), func(t *testing.T) {
			r, restart := p16RestartRig(t, p16Options{phases: phases})
			script(r)
			reg := faults.NewRegistry()
			for _, a := range faults.Random(seed, points, 4, 25, faults.KindCrash) {
				reg.Arm(a)
			}
			ctx, done := faults.WithRegistry(context.Background(), reg)
			defer done()
			crashes := 0
			for i := 0; i < 200 && r.execStatus(t) != "COMPLETED"; i++ {
				_, err := r.eng().Run(ctx, r.execID, 50)
				switch {
				case faults.IsCrash(err):
					crashes++
					restart()
				case err != nil:
					t.Fatalf("run: %v", err)
				}
				if s := r.execStatus(t); s == "RECOVERY_REQUIRED" || s == "FAILED" {
					t.Fatalf("seed %d: %s (%s)", seed, s, r.recoveryReason(t))
				}
			}
			if got := p18FinalSummary(t, r); got != want {
				t.Fatalf("seed %d (%d crashes): final state %s, want %s", seed, crashes, got, want)
			}
			total += crashes
			t.Logf("seed %d: %d crashes, final state %s", seed, crashes, want)
		})
	}
}
