package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// observe drives the engine one step at a time (as the scheduler would),
// reading the console's overview after each step — the browser only reads.
// It returns the progress Done values seen and checks that progress only
// moves when a work unit became DONE.
func observe(t *testing.T, r *p17Rig, maxSteps int, each func(ov readmodel.Overview)) []int {
	t.Helper()
	var seen []int
	lastDone := -1
	doneUnits := func() int {
		return r.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+r.pid.String()+"' AND status = 'DONE'")
	}
	for i := 0; i < maxSteps; i++ {
		res, err := r.eng().Run(context.Background(), r.execID, 1)
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		var ov readmodel.Overview
		r.b.getJSON("/api/v1/projects/"+r.pid.String()+"/overview", &ov)
		if each != nil {
			each(ov)
		}
		if ov.Progress.Done != lastDone {
			if lastDone >= 0 && ov.Progress.Done > lastDone && doneUnits() == 0 {
				t.Fatalf("progress moved without a DONE unit")
			}
			seen = append(seen, ov.Progress.Done)
			lastDone = ov.Progress.Done
		}
		if len(res) > 0 && res[len(res)-1].Outcome == execution.StepCompleted {
			break
		}
		if ov.Status == "COMPLETED" {
			break
		}
	}
	return seen
}

func eventTypes(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		var le readmodel.LiveEvent
		_ = json.Unmarshal([]byte(e.Data), &le)
		out = append(out, le.Type)
	}
	return out
}

func indexOf(xs []string, s string, from int) int {
	for i := from; i < len(xs); i++ {
		if xs[i] == s {
			return i
		}
	}
	return -1
}

// P17-133 / P17-134 (WBS 17.185 / 17.186 / 17.167–17.169) — the browser
// observes autonomous execution (planning → activation → unit → dispatch →
// continuation → validation FAIL → FIX → PASS → checkpoint → DONE →
// progress → next unit) through the stream and read models, without
// controlling any transition; progress changes only at DONE.
func TestP17133AutonomousExecutionObserved(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	ch, cancel, _ := r.b.stream("/api/v1/projects/"+pid+"/events/stream", strconv.FormatInt(ov.LatestEventSeq, 10))
	defer cancel()
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queueRaw(p16Reply("incomplete", "Half way.", ""), 1000) // same-session continuation
	r.queue("complete")
	r.setCheck(t, "u1", 1) // first validation of u1 fails
	r.queue("complete")    // FIX
	r.queue("complete")    // u2
	sawComplete := false
	fixed := false
	seen := observe(t, r, 200, func(ov readmodel.Overview) {
		if ov.CurrentUnit != nil && ov.CurrentUnit.Stage == readmodel.StageComplete {
			sawComplete = true
		}
		if !fixed && r.count(t, "SELECT count(*) FROM fix_attempts") == 1 {
			r.setCheck(t, "u1", 0)
			fixed = true
		}
	})
	if strings.Trim(strings.Join(strings.Fields(strings.Trim(fmtInts(seen), "[]")), ","), ",") != "0,1,2" {
		t.Fatalf("progress steps %v (only DONE may move it)", seen)
	}
	_ = sawComplete
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("exec %s", r.execStatus(t))
	}
	evs := eventTypes(collect(ch, 1000, 3*time.Second))
	order := []string{"PLAN_ACTIVATED", "WORK_UNIT_CLAIMED", "WORKER_DISPATCH_INTENT", "VALIDATION_FAILED", "FIX_ATTEMPT",
		"VALIDATION_PASSED", "CHECKPOINT_CREATED", "WORK_UNIT_DONE", "WORK_UNIT_CLAIMED", "WORK_UNIT_DONE", "PHASE_DONE", "PROJECT_COMPLETED"}
	at := 0
	for _, e := range order {
		i := indexOf(evs, e, at)
		if i < 0 {
			t.Fatalf("stream lacks %s after position %d: %v", e, at, evs)
		}
		at = i + 1
	}
	// The browser issued only reads during the run.
	if n := r.count(t, "SELECT count(*) FROM audit_events"); n != 0 {
		t.Fatalf("browser performed %d mutations", n)
	}
	// The unit page shows COMPLETE and DONE as distinct stages, failed
	// validation kept beside the later PASS.
	var units readmodel.UnitPage
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases/"+r.phases[1].String()+"/work-units", &units)
	body := r.b.mustPage("/projects/" + pid + "/work-units/" + units.Rows[0].ID)
	text := textOf(body)
	for _, want := range []string{"COMPLETE (worker report)", "DONE (validated, checkpointed)", "Fix Attempt 1 / 3", "FAIL", "PASS", "Continuation #1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("unit page lacks %q", want)
		}
	}
}

func fmtInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// P17-135 (WBS 17.187) — a context reset shows the old and new session and
// the same current task.
func TestP17135ContextResetUI(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queueRaw(p16Reply("incomplete", "Most of it.", ""), 60000)
	r.queue("complete")
	var unitsBefore, unitsAfter []string
	observe(t, r, 200, func(ov readmodel.Overview) {
		if ov.CurrentTask != nil {
			if len(unitsBefore) == 0 || unitsBefore[len(unitsBefore)-1] != ov.CurrentTask.Key {
				unitsBefore = append(unitsBefore, ov.CurrentTask.Key)
			}
		}
	})
	unitsAfter = unitsBefore
	if len(unitsAfter) != 1 || unitsAfter[0] != "u1" {
		t.Fatalf("current task changed across the reset: %v", unitsAfter)
	}
	var l struct {
		Continuations []readmodel.ContinuationRow `json:"continuations"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/continuations", &l)
	var reset *readmodel.ContinuationRow
	for i := range l.Continuations {
		if l.Continuations[i].Reset {
			reset = &l.Continuations[i]
		}
	}
	if reset == nil || reset.OldSession == "" || reset.NewSession == "" || reset.OldSession == reset.NewSession || reset.ContextBefore == nil {
		t.Fatalf("reset view: %+v", l.Continuations)
	}
	text := textOf(r.b.mustPage("/projects/" + pid + "/continuations"))
	if !strings.Contains(text, "Session Reset") || !strings.Contains(text, "not a task restart") || !strings.Contains(text, "context threshold exceeded") {
		t.Fatalf("continuations page: %s", text)
	}
}

// P17-136 (WBS 17.188) — pause between units: no dispatch while paused;
// resume continues with the exact next unit.
func TestP17136PauseBetweenUnits(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queue("complete")
	r.queue("complete")
	for i := 0; i < 200 && !strings.Contains(r.unitStatuses(t, 1), "u1:DONE"); i++ {
		r.run(t, 1)
	}
	if code, _, _ := r.b.form("/projects/"+pid+"/pause", nil); code != 200 {
		t.Fatal("pause")
	}
	sends := len(r.g.Messages())
	r.eng().Run(context.Background(), r.execID, 20)
	if len(r.g.Messages()) != sends || r.execStatus(t) != "PAUSED" {
		t.Fatalf("work dispatched while paused (%s)", r.execStatus(t))
	}
	if code, _, _ := r.b.form("/projects/"+pid+"/resume", nil); code != 200 {
		t.Fatal("resume")
	}
	var claimed string
	for i := 0; i < 50 && claimed == ""; i++ {
		r.run(t, 1)
		var ov readmodel.Overview
		r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
		if ov.CurrentTask != nil {
			claimed = ov.CurrentTask.Key
		}
	}
	if claimed != "u2" {
		t.Fatalf("resumed with %q, want u2", claimed)
	}
}

// P17-138 / P17-139 (WBS 17.190 / 17.191) — during execution the executing
// phase WBS stays protected while a later phase WBS is edited and planned
// from the new version; appending a phase keeps the current phase.
func TestP17138WBSEditAndAppendDuringExecution(t *testing.T) {
	r := p17NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}})
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	// Run until the first phase is DONE and phase 2 is being worked on.
	for i := 0; i < 200 && r.phaseStatus(t, 1) != "DONE"; i++ {
		r.run(t, 1)
	}
	r.queuePlan(r.planJSON(p16Unit{key: "b"}))
	for i := 0; i < 20; i++ {
		var ov readmodel.Overview
		r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
		if ov.Plan != nil && ov.Phase != nil && ov.Phase.Order == 2 {
			break
		}
		r.run(t, 1)
	}
	// Append Phase 3 through the web while phase 2 is current.
	// The rig seeds phases directly; align the append cursor (Phase 3).
	r.exec(t, "UPDATE projects SET max_phase_order = 2 WHERE id = '"+pid+"'")
	if code, body, _ := r.b.form("/projects/"+pid+"/phases", url.Values{"name": {"Three (appended)"}}); code != 200 {
		t.Fatalf("append: %d %s", code, textOf(body))
	}
	var phases struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases", &phases)
	if len(phases.Phases) != 3 || !phases.Phases[1].Current || phases.Phases[2].Name != "Three (appended)" || phases.Phases[2].Current {
		t.Fatalf("phase order / current: %+v", phases.Phases)
	}
	// The executing phase's WBS stays protected (P3-17): the editor shows a
	// conflict and the WBS is unchanged.
	cur := phases.Phases[1]
	page := r.b.mustPage("/projects/" + pid + "/phases/" + cur.ID + "/wbs")
	if code, body, _ := r.b.form("/projects/"+pid+"/phases/"+cur.ID+"/wbs", url.Values{"content": {"# Two v2\n- more"},
		"expected_version": {hidden(page, "expected_version")}}); code != http.StatusConflict || !strings.Contains(textOf(body), "Conflict — not saved") {
		t.Fatalf("executing WBS edit: %d %s", code, textOf(body))
	}
	// The appended (not yet executed) phase's WBS is edited during execution
	// through the editor: v1 then v2.
	next := phases.Phases[2]
	for i, content := range []string{"# Three\n- c", "# Three v2\n- c\n- d"} {
		page = r.b.mustPage("/projects/" + pid + "/phases/" + next.ID + "/wbs")
		if code, body, _ := r.b.form("/projects/"+pid+"/phases/"+next.ID+"/wbs", url.Values{"content": {content},
			"expected_version": {hidden(page, "expected_version")}}); code != 200 {
			t.Fatalf("wbs edit %d: %d %s", i+1, code, textOf(body))
		}
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases", &phases)
	if phases.Phases[1].WBSVersion != cur.WBSVersion || phases.Phases[2].WBSVersion != 2 {
		t.Fatalf("wbs versions: current %d→%d appended %d", cur.WBSVersion, phases.Phases[1].WBSVersion, phases.Phases[2].WBSVersion)
	}
	// Execution continues into the appended phase.
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "c"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 300)
	if n := countOf(r.events(t), "PLAN_ACTIVATED"); n < 3 {
		t.Fatalf("plans activated %d", n)
	}
	// Phase 3 was planned from the edited WBS (v2).
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases", &phases)
	if p3 := phases.Phases[2]; p3.Status != "DONE" || p3.Plan == nil || p3.Plan.SourceWBS != 2 {
		t.Fatalf("appended phase: %+v plan %+v", p3, p3.Plan)
	}
}

// P17-140 (WBS 17.192 / 17.12) — an amendment changes the plan version and
// the denominator (the percentage may fall).
func TestP17140PlanAmendmentUI(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b"}))
	r.queue("complete")
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "r1", deps: []string{"b"}}))
	r.queue("complete")
	r.setCheck(t, "phase", 1)
	var full, amended readmodel.Overview
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionPhaseValidation && stage == "validation" && full.Plan == nil {
			r.b.getJSON("/api/v1/projects/"+pid+"/overview", &full)
		}
		if a == execution.ActionSelectUnit && stage == "intent" && strings.Contains(r.unitStatuses(t, 1), "r1:PENDING") && amended.Plan == nil {
			r.b.getJSON("/api/v1/projects/"+pid+"/overview", &amended)
			r.setCheck(t, "phase", 0)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 250)
	if full.Progress.Percent != 100 || full.Plan == nil || full.Plan.Version != 1 {
		t.Fatalf("before amendment: %+v %+v", full.Progress, full.Plan)
	}
	if amended.Plan == nil || amended.Plan.Version != 2 || amended.Plan.Kind != "AMENDMENT" || amended.Progress.Label != "2 / 3 tasks complete" || amended.Progress.Percent != 66.7 {
		t.Fatalf("after amendment: %+v %+v", amended.Progress, amended.Plan)
	}
	if full.Phase.Status == "DONE" {
		t.Fatal("100 % shown as phase DONE before validation")
	}
}

// P17-141 / P17-144 / P17-145 (WBS 17.193–17.194, 17.197, 17.198) — full
// completion observed through the browser (also reopened after being
// offline), malicious worker output rendered as data, and no secret in any
// normal response.
func TestP17141FullCompletionView(t *testing.T) {
	r := p17NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}, {3, "Three", "PENDING", 1}}})
	pid := r.pid.String()
	evil := "<script>alert(1)</script> <div role=\\\"alert\\\">RECOVERY REQUIRED</div> " + strings.Repeat("Z", 3000)
	for _, k := range []string{"a", "b", "c"} {
		r.queuePlan(r.planJSON(p16Unit{key: k}))
		r.queueRaw(p16Reply("complete", evil, ""), 1000)
	}
	// Browser closed during the whole run.
	r.runUntil(t, execution.StepCompleted, 300)
	// Reopen.
	b := newBrowser(t, r.h.BaseURL)
	text := textOf(b.mustPage("/projects/" + pid))
	for _, want := range []string{"COMPLETED", "3 / 3 DONE", "100.0%", "All Phases DONE (3 / 3)", "Final Validation PASS", "Final Checkpoint CREATED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("final view lacks %q", want)
		}
	}
	if strings.Contains(text, "RECOVERY REQUIRED Automatic") {
		t.Fatal("recovery shown for a completed project")
	}
	var ov readmodel.Overview
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Execution.Status != "COMPLETED" || ov.CurrentTask != nil || ov.Recovery {
		t.Fatalf("final overview: %+v", ov)
	}
	phases := textOf(b.mustPage("/projects/" + pid + "/phases"))
	if strings.Count(phases, "✓ DONE") != 3 {
		t.Fatalf("phases: %s", phases)
	}
	// Complete history is available after the fact.
	var tl readmodel.Timeline
	b.getJSON("/api/v1/projects/"+pid+"/console/executions/"+r.execID.String()+"/timeline?limit=500", &tl)
	newest := []string{}
	for _, row := range tl.Rows[:5] {
		newest = append(newest, row.Type)
	}
	if len(tl.Rows) < 20 || indexOf(newest, "PROJECT_COMPLETED", 0) < 0 {
		t.Fatalf("history: %d events, newest %v", len(tl.Rows), newest)
	}
	// Malicious output: escaped, bounded, cannot add a real alert banner.
	pages := crawl(t, b, []string{"/projects/" + pid}, 400, pid)
	for p, body := range pages {
		if strings.Contains(body, "<script>alert") || strings.Contains(body, `<div role="alert">RECOVERY`) {
			t.Fatalf("%s renders worker markup", p)
		}
		if len(body) > 600<<10 {
			t.Fatalf("%s unbounded: %d bytes", p, len(body))
		}
	}
	// Secret scan over every normal response: pages, API reads, SSE.
	secrets := []string{p12Token, p9SentinelKey}
	scan := func(where, s string) {
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Fatalf("secret leaked in %s", where)
			}
		}
	}
	for p, body := range pages {
		scan(p, body)
	}
	for route, action := range r.h.App.Router().Routes() {
		parts := strings.SplitN(route, " ", 2)
		if parts[0] != http.MethodGet || action != "READ" || !strings.HasPrefix(parts[1], "/api/v1/") || strings.HasSuffix(parts[1], "/stream") {
			continue
		}
		path := regexp.MustCompile(`\{([a-zA-Z]+)\}`).ReplaceAllStringFunc(parts[1], func(m string) string {
			switch strings.Trim(m, "{}") {
			case "project", "id":
				return pid
			case "execution":
				return r.execID.String()
			case "phase", "phaseID":
				return r.phases[1].String()
			}
			return "00000000-0000-0000-0000-000000000000"
		})
		_, raw := b.api(http.MethodGet, path, nil)
		scan(route, string(raw))
	}
	ch, cancel, _ := b.stream("/api/v1/projects/"+pid+"/events/stream?after=0", "")
	all := collect(ch, 10000, 3*time.Second)
	cancel()
	for _, e := range all {
		scan("sse", e.Data)
	}
	if len(all) < 20 {
		t.Fatalf("replayed %d events", len(all))
	}
}

// P17-142 (WBS 17.195 / 17.173 / 17.174) — overseer restart mid-project:
// the browser reconnects (Last-Event-ID) and sees the reconstructed state
// without duplicated work.
func TestP17142RestartDuringBrowserSession(t *testing.T) {
	base, restart := p16RestartRig(t, p16Options{config: p17Config(nil)})
	r := &p17Rig{p16Rig: base, b: newBrowser(t, base.h.BaseURL)}
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	crashed := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionRunUnitValidation && stage == "validation" && !crashed {
			crashed = true
			return errP16Crash
		}
		return nil
	})
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	ch, cancel, _ := r.b.stream("/api/v1/projects/"+pid+"/events/stream", strconv.FormatInt(ov.LatestEventSeq, 10))
	r.eng().Run(context.Background(), r.execID, 100)
	got := collect(ch, 1000, 2*time.Second)
	cancel()
	last := ov.LatestEventSeq
	if len(got) > 0 {
		last, _ = strconv.ParseInt(got[len(got)-1].ID, 10, 64)
	}
	h := restart()
	b := newBrowser(t, h.BaseURL) // same browser, server came back (new port in tests)
	r.b = b
	r.runUntil(t, execution.StepCompleted, 100)
	ch2, cancel2, code := b.stream("/api/v1/projects/"+pid+"/events/stream", strconv.FormatInt(last, 10))
	defer cancel2()
	if code != 200 {
		t.Fatalf("reconnect: %d", code)
	}
	replay := collect(ch2, 1000, 3*time.Second)
	prev := last
	for _, e := range replay {
		id, _ := strconv.ParseInt(e.ID, 10, 64)
		if id != prev+1 {
			t.Fatalf("replay gap / duplicate: %d after %d", id, prev)
		}
		prev = id
	}
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "COMPLETED" || ov.LatestEventSeq != prev {
		t.Fatalf("reconstructed: %s seq %d / %d", ov.Status, ov.LatestEventSeq, prev)
	}
	if countOf(r.events(t), "WORK_UNIT_DONE") != 1 || len(r.g.Messages()) != 2 {
		t.Fatalf("duplicated work: done %d sends %d", countOf(r.events(t), "WORK_UNIT_DONE"), len(r.g.Messages()))
	}
}

// P17-146 (WBS 17.199) — after completion, changing roles, templates, the
// state machine, the worker model and the WBS leaves the historical
// execution's captured configuration and artifacts unchanged.
func TestP17146HistoricalReconstruction(t *testing.T) {
	r := p17NewRig(t)
	pid, eid := r.pid.String(), r.execID.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 100)
	snapshot := func() string {
		_, raw := r.b.api(http.MethodGet, "/api/v1/projects/"+pid+"/executions/"+eid+"/configuration", nil)
		return string(raw)
	}
	before := snapshot()
	var prompts struct {
		Prompts []map[string]any `json:"prompts"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/prompts?limit=100", &prompts)
	if len(prompts.Prompts) == 0 {
		t.Fatal("no prompts")
	}
	promptID := prompts.Prompts[0]["id"].(string)
	promptBefore := r.b.mustPage("/projects/" + pid + "/prompts/" + promptID)

	// Change everything current.
	var roles struct {
		Roles []map[string]any `json:"roles"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/roles", &roles)
	for _, ro := range roles.Roles {
		if ro["project_id"] != nil {
			var cur map[string]any
			r.b.getJSON("/api/v1/projects/"+pid+"/roles/"+ro["id"].(string)+"/system-prompt", &cur)
			r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/roles/"+ro["id"].(string)+"/system-prompt",
				map[string]any{"content": "changed", "expected_version": cur["version"]})
		}
	}
	var tl struct {
		Templates []map[string]any `json:"templates"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/prompt-templates", &tl)
	for _, tm := range tl.Templates {
		r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/prompt-templates/"+tm["id"].(string)+"/versions",
			map[string]any{"content": "changed {{work_unit.task}}", "expected_version": tm["current_version"]})
	}
	r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-machine/states", map[string]any{"key": "EXTRA_REVIEW", "name": "Extra", "kind": "ACTION"})
	var st map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/settings", &st)
	st["worker_model"] = "brand-new-model"
	r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/settings", st)
	r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/phases/"+r.phases[1].String()+"/wbs", map[string]any{"content": "# changed", "expected_version": 1})

	if after := snapshot(); after != before {
		t.Fatalf("captured configuration changed:\n%s\n%s", before, after)
	}
	if !strings.Contains(before, "hermes-worker") || strings.Contains(before, "brand-new-model") {
		t.Fatal("snapshot does not carry the captured worker model")
	}
	// The header's "Loaded <time>" stamp is per request, not history.
	loaded := regexp.MustCompile(`Loaded \d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC`)
	view := func(page string) string { return loaded.ReplaceAllString(textOf(page), "Loaded")[200:] }
	if promptAfter := r.b.mustPage("/projects/" + pid + "/prompts/" + promptID); view(promptAfter) != view(promptBefore) {
		t.Fatal("historical prompt view changed")
	}
	// The historical execution page still renders.
	r.b.mustPage("/projects/" + pid + "/executions/" + eid)
}

// P17-144 (WBS 17.197) — malformed Unicode and huge worker output: the
// backend refuses it as a protocol violation (recovery), and every page
// shows it as bounded, escaped data that cannot control the interface.
func TestP17144MalformedWorkerOutput(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	raw := `{"protocol_version":"1.0","status":"complete","summary":"<script>alert(9)</script>` + "\xff\xfe" + strings.Repeat("Q", 300000) +
		`","completed":[],"remaining":[],"blocking":[],"next_action":"<button>Approve</button>"}`
	r.queueRaw(raw, 1000)
	r.runUntil(t, execution.StepRecoveryRequired, 50)
	pages := crawl(t, r.b, []string{"/projects/" + pid}, 400, pid)
	for p, body := range pages {
		if strings.Contains(body, "<script>alert(9)") || strings.Contains(body, "<button>Approve") {
			t.Fatalf("%s renders worker markup", p)
		}
		if len(body) > 600<<10 {
			t.Fatalf("%s unbounded: %d bytes", p, len(body))
		}
	}
	if !strings.Contains(textOf(pages["/projects/"+pid]), "RECOVERY REQUIRED") {
		t.Fatal("protocol violation not surfaced")
	}
}
