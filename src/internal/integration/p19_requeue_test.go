package integration

import (
	"context"
	"encoding/json"
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

// requeueAware wraps the fake team: like a project manager following the
// requeue hint, it counts a requeued task as not done before acceptance.
func requeueAware(team *sfTeam, before func(hermestest.Message) *hermestest.Behavior) func(hermestest.Message) *hermestest.Behavior {
	return func(m hermestest.Message) *hermestest.Behavior {
		if strings.HasPrefix(m.Content, "have the project manager perform acceptance tasks") && strings.Contains(m.Content, " was requeued") {
			team.mu.Lock()
			for i := range team.tasks {
				if strings.Contains(m.Content, "Task "+team.tasks[i].key+" was requeued") {
					team.tasks[i].status = "TODO"
				}
			}
			team.mu.Unlock()
		}
		if before != nil {
			if b := before(m); b != nil {
				return b
			}
		}
		return team.respond(m)
	}
}

// TestP19RequeueTask — the operator requeues task A while it is being
// tested: A is READY again, the context is cleared (new session) and the
// workflow returns to ACCEPTANCE; the overseer reloads A, which restarts
// from IMPLEMENT (no fix attempt carried over) and the execution
// completes.
func TestP19RequeueTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	r.g.Responder = requeueAware(team, nil)
	ctx := context.Background()

	for i := 0; i < 50 && r.state(t) != "TEST"; i++ {
		r.run(t, 1)
	}
	if r.state(t) != "TEST" {
		t.Fatalf("never reached TEST: %s (%s)", r.state(t), team.commandLines())
	}
	// The overview offers REQUEUE next to CANCEL.
	code, raw, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+r.pid.String()+"/overview", "")
	var ov struct {
		Actions []string `json:"allowed_actions"`
	}
	if err := json.Unmarshal([]byte(raw), &ov); code != 200 || err != nil {
		t.Fatalf("overview: %d %v", code, err)
	}
	if got := strings.Join(ov.Actions, ","); !strings.Contains(got, "CANCEL,REQUEUE") {
		t.Fatalf("allowed actions %s, want REQUEUE next to CANCEL", got)
	}
	var sessBefore string
	if err := r.h.DBScan(ctx, "SELECT id::text FROM worker_sessions WHERE status = 'ACTIVE' AND project_id = '"+r.pid.String()+"'", &sessBefore); err != nil {
		t.Fatal(err)
	}

	// The API requires a reason.
	url := r.h.BaseURL + "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/requeue"
	if code := p19Post(t, url, ""); code != http.StatusBadRequest {
		t.Fatalf("requeue without a reason: %d", code)
	}
	if code := p19Post(t, url, "the task went off track"); code != 200 {
		t.Fatalf("requeue: %d", code)
	}

	// Applied at once (the execution was idle).
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE unit_key = 'A' AND status = 'READY' AND project_id = '"+r.pid.String()+"'"); n != 1 {
		t.Fatal("task A was not reset to READY")
	}
	if st := r.state(t); st != "ACCEPTANCE" {
		t.Fatalf("state after requeue %s, want ACCEPTANCE", st)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE id = '"+r.execID.String()+"' AND current_work_unit_id IS NULL"); n != 1 {
		t.Fatal("the execution still points at the requeued task")
	}
	var sessAfter string
	if err := r.h.DBScan(ctx, "SELECT id::text FROM worker_sessions WHERE status = 'ACTIVE' AND project_id = '"+r.pid.String()+"'", &sessAfter); err != nil {
		t.Fatal(err)
	}
	if sessAfter == sessBefore {
		t.Fatal("the worker context was not cleared (same session)")
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'WORK_UNIT_REQUEUED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d requeue events", n)
	}

	// The overseer reloads A; it restarts from IMPLEMENT and the execution
	// completes.
	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{
		"create the output-format skill", "plan phase 1", "have the project manager perform acceptance tasks",
		"implement task A", // requeued in TEST, before the test command
		"have the project manager perform acceptance tasks", // after the requeue: A reloaded
		"implement task A", "test task A",
		"have the project manager perform acceptance tasks",
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks",
	}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery %q\ntrace %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	team.mu.Lock()
	hinted := strings.Contains(team.cmds[4], "Task A was requeued")
	team.mu.Unlock()
	if !hinted {
		t.Fatal("the acceptance command after the requeue does not name the requeued task")
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'FIX_ATTEMPT' AND execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d fix attempts: the restarted task's first implementation counted as a fix", n)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
}

// TestP19RequeueWhileRunning — a requeue requested while a worker command
// is in flight cannot interrupt it: it is recorded and applied at the next
// step boundary, after which task A restarts from acceptance.
func TestP19RequeueWhileRunning(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	var once sync.Once
	inFlight := make(chan struct{})
	r.g.Responder = requeueAware(team, func(m hermestest.Message) *hermestest.Behavior {
		if first, _, _ := strings.Cut(m.Content, "\n"); first == "implement task A" {
			var slow *hermestest.Behavior
			once.Do(func() {
				close(inFlight)
				b := team.respond(m)
				b.Delay = 800 * time.Millisecond
				slow = b
			})
			return slow
		}
		return nil
	})
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
	res, err := r.eng().Requeue(ctx, r.pid, r.execID, "operator", "restart A")
	if err != nil || res.Status != service.RequeueRequested || res.Task != "A" {
		t.Fatalf("requeue while running: %+v %v", res, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	want := strings.Join([]string{
		"create the output-format skill", "plan phase 1", "have the project manager perform acceptance tasks",
		"implement task A", // in flight when the requeue was requested
		"have the project manager perform acceptance tasks", // applied at the next step: A reloaded
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
	// The request is consumed: a later requeue of a finished execution is
	// refused.
	if _, err := r.eng().Requeue(ctx, r.pid, r.execID, "operator", "again"); err == nil {
		t.Fatal("requeue of a completed execution accepted")
	}
}

// p19Post posts to an API URL with an optional reason header and returns
// the status code.
func p19Post(t *testing.T, url, reason string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, http.NoBody)
	if reason != "" {
		req.Header.Set("X-Overseer-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestP19RequeueConsole — the overview shows "Requeue task…" next to
// Cancel; the confirmation page names the task and requires a reason and
// explicit confirmation; confirming requeues the task.
func TestP19RequeueConsole(t *testing.T) {
	r := p17NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r.p16Rig)
	r.g.Responder = requeueAware(team, nil)
	for i := 0; i < 50 && r.state(t) != "TEST"; i++ {
		r.run(t, 1)
	}
	b := r.b
	p := "/projects/" + r.pid.String()
	ov := b.mustPage(p)
	cancel, requeue := strings.Index(ov, p+"/cancel"), strings.Index(ov, `href="`+p+`/requeue">Requeue task…</a>`)
	if cancel < 0 || requeue < 0 || requeue < cancel {
		t.Fatalf("overview lacks the Requeue button next to Cancel:\n%s", textOf(ov))
	}
	page := b.mustPage(p + "/requeue")
	if text := textOf(page); !strings.Contains(text, "Task A Task A restarts from the beginning") || !strings.Contains(text, "the overseer loads it again") {
		t.Fatalf("requeue page: %s", text)
	}
	if code, _, _ := b.form(p+"/requeue", url.Values{"reason": {"off track"}}); code != http.StatusBadRequest {
		t.Fatalf("requeue without confirmation: %d", code)
	}
	code, body, final := b.form(p+"/requeue", url.Values{"reason": {"off track"}, "confirm": {"yes"}})
	if code != 200 || final != p || !strings.Contains(textOf(body), "Task A requeued") {
		t.Fatalf("requeue: %d %s %s", code, final, textOf(body))
	}
	if st := r.state(t); st != "ACCEPTANCE" {
		t.Fatalf("state %s, want ACCEPTANCE", st)
	}
	// With the task requeued there is no current task: no Requeue button.
	if strings.Contains(b.mustPage(p), "Requeue task…") {
		t.Fatal("Requeue offered without a current task")
	}
}

// TestP19PauseAfterTask — "Pause after done" on the overview (between
// Pause and Cancel): task A finishes, B is loaded and the project pauses
// before "implement task B" is sent; Cancel withdraws a request, and
// resuming starts B and completes the execution.
func TestP19PauseAfterTask(t *testing.T) {
	r := p17NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r.p16Rig)
	r.g.Responder = requeueAware(team, nil)
	for i := 0; i < 50 && r.state(t) != "TEST"; i++ {
		r.run(t, 1)
	}
	b := r.b
	p := "/projects/" + r.pid.String()
	ov := b.mustPage(p)
	pause, after, cancel := strings.Index(ov, `action="`+p+`/pause"`), strings.Index(ov, `action="`+p+`/pause-after-task"`), strings.Index(ov, p+"/cancel")
	if pause < 0 || after < 0 || cancel < 0 || !(pause < after && after < cancel) || !strings.Contains(ov, ">Pause after done</button>") {
		t.Fatalf("overview lacks Pause after done between Pause and Cancel:\n%s", textOf(ov))
	}
	// Request, cancel, request again.
	for _, step := range []struct{ path, notice, button string }{
		{"/pause-after-task", "Pause after task requested", ">Cancel pause after done</button>"},
		{"/pause-after-task/cancel", "Pause after task cancelled", ">Pause after done</button>"},
		{"/pause-after-task", "Pause after task requested", ">Cancel pause after done</button>"},
	} {
		code, body, final := b.form(p+step.path, nil)
		if code != 200 || final != p || !strings.Contains(textOf(body), step.notice) || !strings.Contains(body, step.button) {
			t.Fatalf("%s: %d %s %s", step.path, code, final, textOf(body))
		}
	}
	for i := 0; i < 50 && r.execStatus(t) != string(execution.ExecutionPaused); i++ {
		r.run(t, 1)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionPaused) {
		t.Fatalf("execution %s (%s)", st, team.commandLines())
	}
	cmds := team.commandLines()
	if !strings.HasSuffix(cmds, "test task A | have the project manager perform acceptance tasks") || strings.Contains(cmds, "task B") {
		t.Fatalf("commands before the pause: %s", cmds)
	}
	if n := r.count(t, "SELECT count(*) FROM executions x JOIN work_units w ON w.id = x.current_work_unit_id WHERE w.unit_key = 'B' AND x.id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatal("the paused execution's current task is not B")
	}
	ov = b.mustPage(p)
	if !strings.Contains(ov, ">Resume</button>") || strings.Contains(ov, "pause after done</button>") || strings.Contains(ov, ">Pause after done</button>") {
		t.Fatalf("paused overview controls:\n%s", textOf(ov))
	}
	if code, _, _ := b.form(p+"/resume", nil); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q (%s)", st, r.recoveryReason(t), team.commandLines())
	}
	if cmds := team.commandLines(); !strings.Contains(cmds, "have the project manager perform acceptance tasks | implement task B") {
		t.Fatalf("B did not start after the resume: %s", cmds)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE execution_id = '"+r.execID.String()+"' AND type = 'PAUSE_AFTER_TASK_APPLIED'"); n != 1 {
		t.Fatalf("applied events: %d", n)
	}
}
