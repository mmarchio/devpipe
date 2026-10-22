package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// taskOpsRig drives a statefile execution into recovery on task A: the
// tester never records its result, so the test command repeats; the
// overseer's automatic analysis and requeue does not help (ops answer
// only from recovery on), and the repeat after it needs recovery.
func taskOpsRig(t *testing.T, ops func(m hermestest.Message) *hermestest.Behavior) (*p17Rig, *sfTeam) {
	t.Helper()
	r := p17NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, silentTests: 10}
	var armed atomic.Bool
	r.g.Responder = requeueAware(team, func(m hermestest.Message) *hermestest.Behavior {
		if armed.Load() {
			return ops(m)
		}
		if strings.HasPrefix(m.Content, "analyze task ") {
			return replyWith("nothing to change") // no analysis written: nothing to fix
		}
		return nil
	})
	waitNotPaused(t, r, func() bool { return r.execStatus(t) == string(execution.ExecutionRecovery) })
	armed.Store(true)
	if r.execStatus(t) != string(execution.ExecutionRecovery) {
		t.Fatalf("never reached recovery: %s (%s)", r.state(t), team.commandLines())
	}
	team.mu.Lock()
	team.silentTests = 0 // the tester records results from now on
	team.mu.Unlock()
	return r, team
}

// waitNotPaused runs the execution one step at a time until done reports
// true, waiting out the overseer's automatic repeat interventions (the
// execution is paused while they run).
func waitNotPaused(t *testing.T, r *p17Rig, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !done() && time.Now().Before(deadline) {
		if r.execStatus(t) == string(execution.ExecutionPaused) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		r.run(t, 1)
	}
}

func taskOpsOf(t *testing.T, r *p17Rig) service.TaskOpsView {
	t.Helper()
	var v service.TaskOpsView
	if code := r.b.getJSON("/api/v1/projects/"+r.pid.String()+"/executions/"+r.execID.String()+"/task-ops", &v); code != 200 {
		t.Fatalf("task-ops: %d", code)
	}
	return v
}

// waitTaskOps polls until no analysis or fix is running.
func waitTaskOps(t *testing.T, r *p17Rig) service.TaskOpsView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if v := taskOpsOf(t, r); v.Running == "" {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task operation never finished")
	return service.TaskOpsView{}
}

func replyWith(s string) *hermestest.Behavior { return &hermestest.Behavior{Content: &s} }

// TestP19TaskAnalyzeAndFix — on the recovery screen the architect analyzes
// task A (Fix stays disabled until optimizations are found), then fixes
// it: the rewritten definition is adopted, the attempt counters reset, the
// screen shows the success, and the resumed execution completes.
func TestP19TaskAnalyzeAndFix(t *testing.T) {
	var analyses, fixes []string
	slow := make(chan struct{})
	r, _ := taskOpsRig(t, func(m hermestest.Message) *hermestest.Behavior {
		switch {
		case strings.HasPrefix(m.Content, "analyze task A"):
			analyses = append(analyses, m.Content)
			<-slow // held until the test checked the running state
			_ = os.WriteFile(filepath.Join(m.Dir, wbs.AnalysisFile), []byte("# Task analysis — A\n\nTask: A\nFixable: yes\n\n"+
				"## Findings\n- The tester never records the result.\n\n## Optimizations\n- Narrow the objective so the test result is recorded.\n"), 0o644)
			return replyWith("analysis written")
		case strings.HasPrefix(m.Content, "fix task A"):
			fixes = append(fixes, m.Content)
			for _, rel := range []string{wbs.BacklogFile, wbs.ActiveTaskFile} {
				p := filepath.Join(m.Dir, rel)
				b, _ := os.ReadFile(p)
				_ = os.WriteFile(p, []byte(strings.Replace(string(b), "Deliver A.", "Deliver A, with the test result recorded.", 1)), 0o644)
			}
			return replyWith("objective narrowed")
		}
		return nil
	})
	ctx := context.Background()
	b := r.b
	page := "/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/recovery"

	// Only the overseer's automatic analysis (no analysis written) so far:
	// Analyze enabled, Fix disabled.
	v := taskOpsOf(t, r)
	if !v.Available || v.Task != "A" || v.FixEnabled || v.Analysis == nil || v.Analysis.Error == "" {
		t.Fatalf("initial panel %+v", v)
	}
	html := b.mustPage(page)
	if !strings.Contains(html, ">Analyze task</button>") || !strings.Contains(html, " disabled>Fix task</button>") {
		t.Fatalf("recovery screen buttons:\n%s", textOf(html))
	}

	// Analyze: runs in the background; recovery actions wait for it.
	if code, body, _ := b.form(page+"/analyze", nil); code != 200 || !strings.Contains(textOf(body), "Task analysis started") {
		t.Fatalf("analyze: %d %s", code, textOf(body))
	}
	if v := taskOpsOf(t, r); v.Running != service.TaskOpAnalyze || v.FixEnabled {
		t.Fatalf("while analyzing %+v", v)
	}
	if _, err := r.eng().ResolveRecovery(ctx, r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryResume}); !errors.Is(err, service.ErrTaskOpRunning) {
		t.Fatalf("resolve during analysis: %v", err)
	}
	close(slow)
	v = waitTaskOps(t, r)
	if v.Analysis == nil || !v.Analysis.Fixable || v.Analysis.Error != "" || !v.FixEnabled ||
		len(v.Analysis.Optimizations) != 1 || v.Analysis.Findings[0] != "The tester never records the result." {
		t.Fatalf("after analysis %+v %+v", v, v.Analysis)
	}
	if len(analyses) != 1 || !strings.Contains(analyses[0], "Recovery reason: ") || !strings.Contains(analyses[0], wbs.PlanFile) ||
		!strings.Contains(analyses[0], wbs.WBSFile(1)) || !strings.Contains(analyses[0], wbs.AnalysisFile) {
		t.Fatalf("analysis command: %q", analyses)
	}
	if html := b.mustPage(page); !strings.Contains(html, "Optimizations found") || strings.Contains(html, " disabled>Fix task</button>") {
		t.Fatalf("Fix not enabled after a fixable analysis:\n%s", textOf(html))
	}

	// Fix: the rewritten definition is adopted; the screen notifies.
	if code, body, _ := b.form(page+"/fix", nil); code != 200 || !strings.Contains(textOf(body), "Task fix started") {
		t.Fatalf("fix: %d %s", code, textOf(body))
	}
	v = waitTaskOps(t, r)
	if v.Fix == nil || !v.Fix.Success || !v.Fix.DefinitionChanged || v.FixEnabled {
		t.Fatalf("after fix %+v %+v", v, v.Fix)
	}
	if len(fixes) != 1 || !strings.Contains(fixes[0], wbs.AnalysisFile) {
		t.Fatalf("fix command: %q", fixes)
	}
	if html := b.mustPage(page); !strings.Contains(textOf(html), "Fix succeeded") {
		t.Fatalf("no success notification:\n%s", textOf(html))
	}
	var objective string
	if err := r.h.DBScan(ctx, "SELECT objective FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE p.phase_id = '"+
		r.phases[1].String()+"' AND w.sequence = 1", &objective); err != nil || objective != "Deliver A, with the test result recorded." {
		t.Fatalf("stored objective %q %v", objective, err)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'WORK_UNIT_FIXED' AND payload->'resets' ? 'STATEFILE_RESENDS'"); n != 1 {
		t.Fatal("the fix did not reset the attempt counters")
	}
	// A second fix needs a new analysis.
	if code, _, _ := b.form(page+"/fix", nil); code != http.StatusConflict {
		t.Fatalf("second fix without analysis: %d", code)
	}

	// The execution stays in recovery; resuming continues to completion.
	if r.execStatus(t) != string(execution.ExecutionRecovery) {
		t.Fatal("the fix left recovery by itself")
	}
	if _, err := r.eng().ResolveRecovery(ctx, r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryResume}); err != nil {
		t.Fatal(err)
	}
	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace %s", st, r.recoveryReason(t), p16Trace(res))
	}
}

// TestP19TaskAnalysisNotFixable — an analysis without optimizations (or a
// missing analysis file) leaves Fix disabled; the API refuses a fix.
func TestP19TaskAnalysisNotFixable(t *testing.T) {
	writeFile := true
	r, _ := taskOpsRig(t, func(m hermestest.Message) *hermestest.Behavior {
		if strings.HasPrefix(m.Content, "analyze task A") {
			if writeFile {
				_ = os.WriteFile(filepath.Join(m.Dir, wbs.AnalysisFile), []byte("# Task analysis — A\n\nTask: A\nFixable: no\n\n## Findings\n- Needs a human decision.\n"), 0o644)
			}
			return replyWith("analysis written")
		}
		return nil
	})
	api := "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/task-ops/"
	if code, raw := r.b.api(http.MethodPost, api+"fix", nil); code != http.StatusConflict {
		t.Fatalf("fix before analysis: %d %s", code, raw)
	}
	if code, raw := r.b.api(http.MethodPost, api+"analyze", nil); code != http.StatusAccepted {
		t.Fatalf("analyze: %d %s", code, raw)
	}
	v := waitTaskOps(t, r)
	if v.Analysis == nil || v.Analysis.Fixable || v.FixEnabled || v.Analysis.Findings[0] != "Needs a human decision." {
		t.Fatalf("not fixable analysis %+v %+v", v, v.Analysis)
	}
	if code, raw := r.b.api(http.MethodPost, api+"fix", nil); code != http.StatusConflict {
		t.Fatalf("fix of a not fixable analysis: %d %s", code, raw)
	}
	// The architect writes nothing: the analysis reports why.
	writeFile = false
	if code, raw := r.b.api(http.MethodPost, api+"analyze", nil); code != http.StatusAccepted {
		t.Fatalf("analyze again: %d %s", code, raw)
	}
	v = waitTaskOps(t, r)
	if v.Analysis == nil || v.FixEnabled || !strings.Contains(v.Analysis.Error, "did not write the analysis") {
		raw, _ := json.Marshal(v)
		t.Fatalf("missing analysis file: %s", raw)
	}
}

// TestP19RepeatedActionIntervention — the tester's result is not recorded
// three times in a row, so the overseer would send TEST a fourth time:
// instead it pauses, has the architect analyze and fix task A, requeues it
// and resumes, and the execution completes. The overview offers Analyze
// and Fix task, disabled while the execution runs.
func TestP19RepeatedActionIntervention(t *testing.T) {
	r := p17NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, silentTests: 3}
	var analyses, fixes []string
	var mu sync.Mutex
	r.g.Responder = requeueAware(team, func(m hermestest.Message) *hermestest.Behavior {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(m.Content, "analyze task A"):
			analyses = append(analyses, m.Content)
			_ = os.WriteFile(filepath.Join(m.Dir, wbs.AnalysisFile), []byte("# Task analysis — A\n\nTask: A\nFixable: yes\n\n"+
				"## Findings\n- The tester never records the result.\n\n## Optimizations\n- Narrow the objective so the test result is recorded.\n"), 0o644)
			return replyWith("analysis written")
		case strings.HasPrefix(m.Content, "fix task A"):
			fixes = append(fixes, m.Content)
			return replyWith("tests adjusted")
		}
		return nil
	})
	overview := "/projects/" + r.pid.String()
	r.run(t, 1)
	if html := r.b.mustPage(overview); !strings.Contains(html, " disabled>Analyze task</button>") || !strings.Contains(html, " disabled>Fix task</button>") {
		t.Fatalf("overview task buttons while running:\n%s", textOf(html))
	}
	waitNotPaused(t, r, func() bool {
		st := r.execStatus(t)
		return st == string(execution.ExecutionCompleted) || st == string(execution.ExecutionRecovery)
	})
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q (%s)", st, r.recoveryReason(t), team.commandLines())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(analyses) != 1 || !strings.Contains(analyses[0], "The overseer paused the execution") ||
		!strings.Contains(analyses[0], "the TEST command 3 times in a row") || len(fixes) != 1 {
		t.Fatalf("analyses %q fixes %q", analyses, fixes)
	}
	for typ, want := range map[string]int{"WORK_UNIT_REPEAT_DETECTED": 1, "WORK_UNIT_REQUEUED": 1, "EXECUTION_PAUSED": 1, "EXECUTION_RESUMED": 1} {
		if n := r.count(t, "SELECT count(*) FROM execution_events WHERE execution_id = '"+r.execID.String()+"' AND type = '"+typ+"'"); n != want {
			t.Errorf("%s events: %d, want %d", typ, n, want)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'WORK_UNIT_FIXED' AND (payload->>'success')::boolean"); n != 1 {
		t.Errorf("successful fixes: %d", n)
	}
	if got := team.commandLines(); strings.Count(got, "test task A | test task A | test task A | test task A") != 0 {
		t.Errorf("TEST was sent a fourth time in a row: %s", got)
	}
}
