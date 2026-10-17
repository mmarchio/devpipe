package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/hardening"
)

// TestP16162SingleUnit — WBS 16.162: PLAN → IMPLEMENT → COMPLETE →
// VALIDATE PASS → CHECKPOINT → DONE → PHASE VALIDATE → PHASE DONE →
// HARDENING → FINAL VALIDATE → FINAL CHECKPOINT → COMPLETED.
func TestP16162SingleUnit(t *testing.T) {
	r := p16NewRig(t)
	if _, err := os.Stat(filepath.Join(r.dir, "statefiles")); !os.IsNotExist(err) {
		t.Fatalf("the composed workflow wrote statefiles: %v", err)
	}
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queue("complete")
	head0 := r.head(t)
	res := r.runUntil(t, execution.StepCompleted, 100)
	t.Logf("trace: %s", p16Trace(res))
	want := "PROJECT_INIT,PHASE_PLAN,PLAN_VALIDATE,WORK_UNIT_PREPARE,IMPLEMENT,RESPONSE_EVALUATE,VALIDATE,VALIDATION_RESULT,PASS,UNIT_DONE," +
		"MORE_WORK,PHASE_VALIDATE,PHASE_DONE,MORE_PHASES,FINAL_HARDENING,FINAL_VALIDATE,FINAL_CHECKPOINT,PROJECT_DONE"
	if got := strings.Join(r.history(t), ","); got != want {
		t.Fatalf("history\n got %s\nwant %s", got, want)
	}
	if r.execStatus(t) != "COMPLETED" || r.projectStatus(t) != "COMPLETED" || r.phaseStatus(t, 1) != "DONE" || r.unitStatuses(t, 1) != "u1:DONE" {
		t.Fatalf("final state: exec %s project %s phase %s units %s", r.execStatus(t), r.projectStatus(t), r.phaseStatus(t, 1), r.unitStatuses(t, 1))
	}
	if r.dispatches(t) != 2 {
		t.Fatalf("dispatches = %d", r.dispatches(t))
	}
	evs := r.events(t)
	for _, e := range []string{"PLAN_ACTIVATED", "WORK_UNIT_CLAIMED", "WORKER_DISPATCH_INTENT", "VALIDATION_PASSED", "CHECKPOINT_CREATED",
		"WORK_UNIT_DONE", "PHASE_DONE", "HARDENING_COMPLETED", "FINAL_VALIDATION_PASSED", "PROJECT_COMPLETED"} {
		if countOf(evs, e) == 0 {
			t.Fatalf("missing event %s in %v", e, evs)
		}
	}
	if countOf(evs, "PROJECT_COMPLETED") != 1 {
		t.Fatal("completion event not once")
	}
	// Checkpoints: unit + final, both commits after the initial HEAD.
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE execution_id = '"+r.execID.String()+"' AND status = 'CREATED'"); n != 2 {
		t.Fatalf("checkpoints = %d", n)
	}
	if r.head(t) == head0 {
		t.Fatal("no checkpoint commit")
	}
	if n := r.count(t, "SELECT count(*) FROM execution_completions WHERE execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatal("completion record")
	}
}

// TestP16163Incomplete — WBS 16.163 / 16.33 / 16.34: IMPLEMENT →
// INCOMPLETE → (Phase 14 same-session) CONTINUE → COMPLETE → VALIDATE →
// DONE; the continuation prompt carries the captured status.
func TestP16163Incomplete(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queue("incomplete")
	r.queue("complete")
	res := r.runUntil(t, execution.StepCompleted, 120)
	h := strings.Join(r.history(t), ",")
	if !strings.Contains(h, "IMPLEMENT,RESPONSE_EVALUATE,CONTINUE_DECISION,SAME_SESSION,CONTINUE,IMPLEMENT,RESPONSE_EVALUATE,VALIDATE") {
		t.Fatalf("history %s\ntrace %s", h, p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM continuations WHERE "+r.where()+" AND strategy = 'same_session'"); n != 1 {
		t.Fatalf("same-session continuations = %d", n)
	}
	if r.dispatches(t) != 3 {
		t.Fatalf("dispatches = %d", r.dispatches(t))
	}
	msgs := r.g.Messages()
	if !strings.Contains(msgs[2].Content, "finish the remaining work") {
		t.Fatal("continuation prompt lacks the captured status")
	}
	if r.unitStatuses(t, 1) != "u1:DONE" {
		t.Fatal(r.unitStatuses(t, 1))
	}
}

// TestP16164ContextReset — WBS 16.164 / 16.35 / 16.36: INCOMPLETE above
// the context threshold → NEW SESSION → CWD RESTORE → CONTINUE → COMPLETE;
// the same work unit stays active and the old conversation is not needed.
func TestP16164ContextReset(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queueRaw(p16Reply("incomplete", "Most of it.", ""), 60000)
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 120)
	h := strings.Join(r.history(t), ",")
	if !strings.Contains(h, "CONTINUE_DECISION,RESET_SESSION,CONTINUE,IMPLEMENT") {
		t.Fatalf("history %s", h)
	}
	if n := len(r.g.Sessions()); n < 2 {
		t.Fatalf("sessions = %d, want a new session", n)
	}
	var units int
	if err := r.h.DBScan(t.Context(), "SELECT count(DISTINCT work_unit_id) FROM worker_dispatches WHERE "+r.where()+" AND work_unit_id IS NOT NULL", &units); err != nil {
		t.Fatal(err)
	}
	if units != 1 {
		t.Fatalf("units dispatched = %d", units)
	}
	sess := r.g.Sessions()
	if cwd := r.g.SessionCWD(sess[len(sess)-1]); cwd == nil || *cwd != r.dir {
		t.Fatalf("CWD not restored on the new session: %v", cwd)
	}
}

// TestP16165FailedValidation — WBS 16.165 / 16.46 / 16.49–16.52: COMPLETE →
// VALIDATE FAIL → FIX (evidence in the prompt) → COMPLETE → VALIDATE PASS →
// CHECKPOINT → DONE. Then the fix limit: persistent failure → recovery.
func TestP16165FailedValidation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queue("complete")
	r.queue("complete")
	r.setCheck(t, "u1", 3)
	// The worker "fixes" the code while handling the FIX dispatch.
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if stage == "dispatch" && r.state(t) == "FIX" {
			r.setCheck(t, "u1", 0)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 120)
	h := strings.Join(r.history(t), ",")
	if !strings.Contains(h, "VALIDATE,VALIDATION_RESULT,FAIL,FIX,IMPLEMENT,RESPONSE_EVALUATE,VALIDATE,VALIDATION_RESULT,PASS,UNIT_DONE") {
		t.Fatalf("history %s", h)
	}
	var fixPrompt string
	for _, m := range r.g.Messages() {
		if strings.Contains(m.Content, "check u1 failed") {
			fixPrompt = m.Content
		}
	}
	if fixPrompt == "" {
		for _, m := range r.g.Messages() {
			t.Logf("MESSAGE:\n%s\n----", m.Content)
		}
	}
	if fixPrompt == "" || !strings.Contains(fixPrompt, "Deliver u1") {
		t.Fatal("fix prompt lacks the failure evidence or the task definition")
	}
	if n := r.count(t, "SELECT count(*) FROM fix_attempts WHERE execution_id = '"+r.execID.String()+"' AND failed_run_id IS NOT NULL AND dispatch_id IS NOT NULL"); n != 1 {
		t.Fatalf("fix attempts = %d", n)
	}
}

func TestP16165FixLimit(t *testing.T) {
	r := p16NewRigWith(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.MaxFixAttempts = 2 }})
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	for i := 0; i < 4; i++ {
		r.queue("complete")
	}
	r.setCheck(t, "u1", 1)
	r.runUntil(t, execution.StepRecoveryRequired, 200)
	if !strings.Contains(r.recoveryReason(t), "fix attempts exhausted") {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
	if r.unitStatuses(t, 1) == "u1:DONE" {
		t.Fatal("unit DONE despite failing validation")
	}
	if n := r.count(t, "SELECT count(*) FROM fix_attempts WHERE execution_id = '"+r.execID.String()+"'"); n != 2 {
		t.Fatalf("fix attempts = %d", n)
	}
}
