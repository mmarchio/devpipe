package integration

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
)

// p19HangFirstImplement makes the first "implement task A" command hang
// (a worker command that never returns) and signals when it was sent.
func p19HangFirstImplement(team *sfTeam) (func(hermestest.Message) *hermestest.Behavior, <-chan struct{}) {
	var once sync.Once
	inFlight := make(chan struct{})
	return requeueAware(team, func(m hermestest.Message) *hermestest.Behavior {
		if first, _, _ := strings.Cut(m.Content, "\n"); first == "implement task A" {
			var hung *hermestest.Behavior
			once.Do(func() {
				close(inFlight)
				b := team.respond(m)
				b.Delay = 10 * time.Minute
				hung = b
			})
			return hung
		}
		return nil
	}), inFlight
}

// TestP19ClearProcesses — a worker command hangs; clearing processes
// interrupts it at once (the dispatch is CANCELLED, the step ABANDONED),
// requeues task A in a new session, and the execution completes with A
// restarted from acceptance.
func TestP19ClearProcesses(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	responder, inFlight := p19HangFirstImplement(team)
	r.g.Responder = responder
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := r.eng().Run(ctx, r.execID, 200)
		done <- err
	}()
	select {
	case <-inFlight:
	case <-time.After(30 * time.Second):
		t.Fatal("implement task A never sent")
	}
	// The worker call is stuck: a plain requeue only records a request.
	if res, err := r.eng().Requeue(ctx, r.pid, r.execID, "operator", "stuck"); err != nil || res.Status != service.RequeueRequested {
		t.Fatalf("requeue while stuck: %+v %v", res, err)
	}

	start := time.Now()
	res, err := r.eng().ClearProcesses(ctx, r.pid, r.execID, "operator", "the worker command hangs")
	if err != nil || res.Status != service.RequeueApplied || res.Task != "A" || !res.InterruptedStep {
		t.Fatalf("clear processes: %+v %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("clear processes took %s", d)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupted run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted run did not return")
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE status = 'CANCELLED' AND state_key = 'IMPLEMENT' AND project_id = '"+r.pid.String()+"'"); n != 1 {
		t.Fatalf("%d cancelled IMPLEMENT dispatches, want 1", n)
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE status NOT IN ('COMPLETED','FAILED','CANCELLED','UNKNOWN') AND project_id = '"+r.pid.String()+"'"); n != 0 {
		t.Fatalf("%d dispatches still in flight", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_steps WHERE status = 'ABANDONED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d abandoned steps, want 1", n)
	}
	if st := r.state(t); st != "ACCEPTANCE" {
		t.Fatalf("state after clearing %s, want ACCEPTANCE", st)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionRunning) {
		t.Fatalf("execution %s after clearing, want RUNNING (recovery %q)", st, r.recoveryReason(t))
	}

	runRes, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(runRes))
	}
	want := strings.Join([]string{
		"create the output-format skill", "plan phase 1", "have the project manager perform acceptance tasks",
		"implement task A", // hung, interrupted by clear processes
		"have the project manager perform acceptance tasks", // A reloaded
		"implement task A", "test task A",
		"have the project manager perform acceptance tasks",
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks",
	}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery %q", got, want, r.recoveryReason(t))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
}

// TestP19ClearProcessesIdle — on an idle execution clearing processes is a
// requeue that also withdraws a requested pause after the task.
func TestP19ClearProcessesIdle(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	r.g.Responder = requeueAware(team, nil)
	ctx := context.Background()
	for i := 0; i < 50 && r.state(t) != "TEST"; i++ {
		r.run(t, 1)
	}
	if r.state(t) != "TEST" {
		t.Fatalf("never reached TEST: %s", r.state(t))
	}
	if err := r.eng().RequestPauseAfterTask(ctx, r.pid, r.execID, "operator"); err != nil {
		t.Fatal(err)
	}
	url := r.h.BaseURL + "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/clear-processes"
	if code := p19Post(t, url, ""); code != http.StatusBadRequest {
		t.Fatalf("clear processes without a reason: %d", code)
	}
	res, err := r.eng().ClearProcesses(ctx, r.pid, r.execID, "operator", "start A over")
	if err != nil || res.Status != service.RequeueApplied || res.InterruptedStep || !res.CancelledPauseAfterTask {
		t.Fatalf("clear processes: %+v %v", res, err)
	}
	if pending, err := r.eng().PauseAfterTaskPending(ctx, r.execID); err != nil || pending {
		t.Fatalf("pause after task still pending: %v %v", pending, err)
	}
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE unit_key = 'A' AND status = 'READY' AND project_id = '"+r.pid.String()+"'"); n != 1 {
		t.Fatal("task A was not reset to READY")
	}
	if st := r.state(t); st != "ACCEPTANCE" {
		t.Fatalf("state after clearing %s, want ACCEPTANCE", st)
	}
	if _, err := r.eng().Run(ctx, r.execID, 200); err != nil {
		t.Fatal(err)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
	// A finished execution has nothing to clear.
	if _, err := r.eng().ClearProcesses(ctx, r.pid, r.execID, "operator", "again"); err == nil {
		t.Fatal("clear processes of a completed execution accepted")
	}
}

// TestP19ClearProcessesConsole — the project Configuration page offers
// Clear processes; it requires a reason and confirmation and requeues the
// task, returning to the Configuration page.
func TestP19ClearProcessesConsole(t *testing.T) {
	r := p17NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r.p16Rig)
	r.g.Responder = requeueAware(team, nil)
	for i := 0; i < 50 && r.state(t) != "TEST"; i++ {
		r.run(t, 1)
	}
	b := r.b
	p := "/projects/" + r.pid.String()
	page := b.mustPage(p + "/config")
	if !strings.Contains(page, `action="`+p+`/clear-processes"`) || !strings.Contains(textOf(page), "Task A Task A restarts from the beginning") {
		t.Fatalf("configuration page lacks Clear processes:\n%s", textOf(page))
	}
	if code, _, _ := b.form(p+"/clear-processes", url.Values{"reason": {"stuck"}}); code != http.StatusBadRequest {
		t.Fatalf("clear processes without confirmation: %d", code)
	}
	code, body, final := b.form(p+"/clear-processes", url.Values{"reason": {"stuck"}, "confirm": {"yes"}})
	if code != 200 || final != p+"/config" || !strings.Contains(textOf(body), "Processes cleared: task A restarts from the beginning.") {
		t.Fatalf("clear processes: %d %s %s", code, final, textOf(body))
	}
	if st := r.state(t); st != "ACCEPTANCE" {
		t.Fatalf("state after clearing %s, want ACCEPTANCE", st)
	}
}
