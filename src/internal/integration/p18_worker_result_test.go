package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// p18StoppedNotDone asserts the execution stopped in recovery without the
// unit being completed or validated.
func p18StoppedNotDone(t *testing.T, r *p16Rig, reasonPart string) {
	t.Helper()
	if r.execStatus(t) != "RECOVERY_REQUIRED" || !strings.Contains(r.recoveryReason(t), reasonPart) {
		t.Fatalf("status %s reason %q (want %q)", r.execStatus(t), r.recoveryReason(t), reasonPart)
	}
	p18NotDone(t, r)
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"' AND scope = 'UNIT'"); n != 0 {
		t.Fatalf("%d unit validation runs started on an untrusted result", n)
	}
}

// T18.68 — a malformed response (prose claiming completion) is never
// inferred COMPLETE.
func TestT1868MalformedNeverComplete(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queueRaw("All done! Every test passes and the work is complete.", 1000)
	_, _ = r.eng().Run(context.Background(), r.execID, 100)
	p18StoppedNotDone(t, r, "violates the result protocol")
}

// T18.69 — an oversized response is rejected before it is parsed into a
// result; nothing advances on it.
func TestT1869OversizedRejected(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queueRaw(p16Reply("complete", strings.Repeat("x", 400<<10), ""), 1000)
	_, _ = r.eng().Run(context.Background(), r.execID, 100)
	p18StoppedNotDone(t, r, "violates the result protocol")
	if n := r.count(t, "SELECT count(*) FROM worker_results WHERE project_id = '"+r.pid.String()+"' AND status = 'COMPLETE'"); n != 0 {
		t.Fatal("an oversized response became an accepted result")
	}
}

// T18.71 — a worker reporting workflow state it does not own
// (project_status / next_state) cannot change it: the response is
// rejected (strict protocol) and the project, runtime state and unit are
// unchanged.
func TestT1871HallucinatedStateMutation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queueRaw(p16Reply("complete", "done", `"project_status":"COMPLETED","next_state":"PROJECT_DONE"`), 1000)
	_, _ = r.eng().Run(context.Background(), r.execID, 100)
	p18StoppedNotDone(t, r, "violates the result protocol")
	if r.projectStatus(t) == "COMPLETED" || r.state(t) == "PROJECT_DONE" {
		t.Fatalf("project %s state %s", r.projectStatus(t), r.state(t))
	}
}

// T18.70 — even when the workflow routes protocol errors back to the
// implementation state, a model that keeps violating the protocol gets a
// bounded number of repairs, then PROTOCOL_VIOLATION recovery.
func TestT1870ProtocolRepairsBounded(t *testing.T) {
	r := p16NewRigWith(t, p16Options{noBootstrap: true})
	r.exec(t, "INSERT INTO state_transitions (id, machine_id, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+
		r.machine.String()+"', '"+r.states["RESPONSE_EVALUATE"].String()+"', '"+r.states["IMPLEMENT"].String()+"', 'PROTOCOL_ERROR', 0, true, now(), now())")
	r.boot(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	for i := 0; i < 6; i++ {
		r.queueRaw("not json, attempt "+string(rune('1'+i)), 1000)
	}
	res, _ := r.eng().Run(context.Background(), r.execID, 200)
	if r.execStatus(t) != "RECOVERY_REQUIRED" || !strings.Contains(r.recoveryReason(t), "repeated worker protocol violations") ||
		p18RecoveryCondition(t, r) != "PROTOCOL_VIOLATION" {
		t.Fatalf("status %s reason %q; %s", r.execStatus(t), r.recoveryReason(t), p16Trace(res))
	}
	// Planner + the original unit response + 2 repairs.
	if n := len(r.g.Messages()); n != 4 {
		t.Fatalf("worker messages %d, want 4", n)
	}
	p18NotDone(t, r)
}

// T18.201 — a stored worker result altered after it was recorded (hash
// no longer matches its content) is corrupt: nothing advances on it.
func TestT18201CorruptStoredResult(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	tampered := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionEvaluateResult && s == "intent" && !tampered && r.count(t, "SELECT count(*) FROM worker_results WHERE work_unit_id IS NOT NULL") == 1 {
			tampered = true
			r.exec(t, "SET session_replication_role = replica; UPDATE worker_results SET summary = 'tampered: everything passed' WHERE work_unit_id IS NOT NULL")
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, _ := r.eng().Run(context.Background(), r.execID, 100)
	if !tampered || p18RecoveryCondition(t, r) != "ARTIFACT_CORRUPT" {
		t.Fatalf("tampered %v condition %q reason %q; %s", tampered, p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18StoppedNotDone(t, r, "corrupt")
}
