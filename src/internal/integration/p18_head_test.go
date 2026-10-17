package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
)

// p18ExternalCommit commits a file to the repository outside the
// overseer.
func p18ExternalCommit(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte("external change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p15Git(t, dir, "add", name)
	p15Git(t, dir, "commit", "-q", "-m", "external "+name)
}

// p18OnSecondUnitDispatch runs fn once, when unit b's dispatch step
// begins (unit a is DONE).
func p18OnSecondUnitDispatch(r *p16Rig, t *testing.T, fn func()) *bool {
	fired := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionDispatch && s == "intent" && strings.Contains(r.unitStatuses(t, 1), "a:DONE") && !fired {
			fired = true
			fn()
		}
		return nil
	})
	return &fired
}

// T18.50 — a HEAD change no checkpoint or operator explains stops new
// work before the next dispatch; the operator's confirmed acceptance is
// audited, recorded once, and the execution then completes normally.
func TestT1850UnexpectedHeadChange(t *testing.T) {
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	fired := p18OnSecondUnitDispatch(r.p16Rig, t, func() { p18ExternalCommit(t, r.dir, "EXTERNAL.md") })
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	r.eng().SetStepHook(nil)
	if err != nil || !*fired || r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r.p16Rig) != "REPOSITORY_HEAD_CHANGED" {
		t.Fatalf("err %v fired %v status %s condition %q reason %q; %s", err, *fired, r.execStatus(t), p18RecoveryCondition(t, r.p16Rig),
			r.recoveryReason(t), p16Trace(res))
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("worker messages %d, want 2 (planner + unit a)", n)
	}
	// Acceptance is high impact: confirmation and a reason are required.
	if code, raw := r.recoveryAPI(t, map[string]any{"action": "ACCEPT_REPOSITORY_HEAD", "idempotency_key": "t1850-accept-1"}); code < 400 {
		t.Fatalf("unconfirmed acceptance: %d %s", code, raw)
	}
	body := map[string]any{"action": "ACCEPT_REPOSITORY_HEAD", "confirm": true, "reason": "reviewed the external commit",
		"idempotency_key": "t1850-accept-2"}
	if code, raw := r.recoveryAPI(t, body); code != 200 {
		t.Fatalf("accept: %d %s", code, raw)
	}
	if code, raw := r.recoveryAPI(t, body); code != 200 { // idempotent repeat
		t.Fatalf("repeat: %d %s", code, raw)
	}
	id := r.execID.String()
	if n := r.count(t, "SELECT count(*) FROM execution_expected_heads WHERE execution_id = '"+id+"' AND source = 'OPERATOR_ACCEPT'"); n != 1 {
		t.Fatalf("acceptances recorded %d", n)
	}
	var head, reason string
	_ = r.h.DBScan(context.Background(), "SELECT head, reason FROM execution_expected_heads WHERE execution_id = '"+id+
		"' AND source = 'OPERATOR_ACCEPT'", &head, &reason)
	if head != p15Git(t, r.dir, "rev-parse", "HEAD") || reason != "reviewed the external commit" {
		t.Fatalf("accepted head %s reason %q", head, reason)
	}
	if countOf(r.events(t), "REPOSITORY_HEAD_ACCEPTED") != 1 {
		t.Fatal("acceptance not in the execution timeline")
	}
	r.runUntil(t, execution.StepCompleted, 200)
	if n := len(r.g.Messages()); n != 3 || r.unitStatuses(t, 1) != "a:DONE,b:DONE" {
		t.Fatalf("messages %d units %s", n, r.unitStatuses(t, 1))
	}
	// The history explains every HEAD the execution worked on.
	var sources string
	_ = r.h.DBScan(context.Background(), "SELECT string_agg(source, ',' ORDER BY seq) FROM execution_expected_heads WHERE execution_id = '"+id+"'", &sources)
	if !strings.HasPrefix(sources, "BOOTSTRAP,CHECKPOINT,OPERATOR_ACCEPT,CHECKPOINT") {
		t.Fatalf("expected-head history %s", sources)
	}
}

// T18.51 — an accepted branch switch becomes the expected branch; work
// continues on it.
func TestT1851AcceptedBranchSwitch(t *testing.T) {
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	p18OnSecondUnitDispatch(r.p16Rig, t, func() { p15Git(t, r.dir, "checkout", "-q", "-b", "feature") })
	_, _ = r.eng().Run(context.Background(), r.execID, 200)
	r.eng().SetStepHook(nil)
	if p18RecoveryCondition(t, r.p16Rig) != "REPOSITORY_HEAD_CHANGED" || !strings.Contains(r.recoveryReason(t), "branch switched") {
		t.Fatalf("condition %q reason %q", p18RecoveryCondition(t, r.p16Rig), r.recoveryReason(t))
	}
	if code, raw := r.recoveryAPI(t, map[string]any{"action": "ACCEPT_REPOSITORY_HEAD", "confirm": true, "reason": "moved to feature branch",
		"idempotency_key": "t1851-accept"}); code != 200 {
		t.Fatalf("accept: %d %s", code, raw)
	}
	mainHead := p15Git(t, r.dir, "rev-parse", "main")
	r.runUntil(t, execution.StepCompleted, 200)
	if out := p15Git(t, r.dir, "log", "--format=%s", "main..feature"); out == "" {
		t.Fatal("no checkpoint committed on the accepted branch")
	}
	if p15Git(t, r.dir, "rev-parse", "main") != mainHead {
		t.Fatal("the old branch was modified after the accepted switch")
	}
}

// T18.62 — a commit made by someone else while the worker runs is
// surfaced: the worker-run evidence shows the HEAD change and the
// execution stops for reconciliation before validation.
func TestT1862ConcurrentModificationSurfaced(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	hold := make(chan struct{})
	c := p16Reply("complete", "done", "")
	r.g.Script(hermestest.Behavior{Content: &c, Hold: hold})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.eng().Run(context.Background(), r.execID, 100) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.g.Messages()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	p18ExternalCommit(t, r.dir, "CONCURRENT.md")
	close(hold)
	<-done
	if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "REPOSITORY_HEAD_CHANGED" {
		t.Fatalf("status %s condition %q reason %q", r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t))
	}
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d validation runs against the modified repository", n)
	}
	var raw []byte
	if err := r.h.DBScan(context.Background(), "SELECT payload FROM execution_events WHERE execution_id = '"+r.execID.String()+
		"' AND type = 'WORKER_RUN_REPOSITORY_EVIDENCE' ORDER BY exec_seq DESC LIMIT 1", &raw); err != nil {
		t.Fatalf("no worker-run evidence: %v", err)
	}
	var ev map[string]any
	_ = json.Unmarshal(raw, &ev)
	if ev["head_changed"] != true || ev["baseline_head"] == ev["after_head"] {
		t.Fatalf("evidence %v", ev)
	}
}

// An unrelated repository can never be accepted as the expected HEAD.
func TestT1849UnrelatedRepositoryNotAcceptable(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		_ = os.RemoveAll(r.dir)
		_ = os.MkdirAll(r.dir, 0o755)
		p15Git(t, r.dir, "init", "-q", "-b", "main")
		p18ExternalCommit(t, r.dir, "OTHER.md")
	}, "REPOSITORY_IDENTITY_MISMATCH", 1)
	if _, err := r.eng().AcceptRepositoryHead(context.Background(), r.pid, r.execID, "op", "try"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("unrelated repository accepted: %v", err)
	}
}
