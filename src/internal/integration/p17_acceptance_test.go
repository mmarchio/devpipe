package integration

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// formFields scrapes the current values of the form posting to action, as
// a browser would submit it unchanged (text / hidden / number inputs,
// checked checkboxes and radios, selected options, textareas).
func formFields(body, action string) url.Values {
	out := url.Values{}
	i := strings.Index(body, `action="`+action+`"`)
	if i < 0 {
		return out
	}
	form := body[i:]
	if j := strings.Index(form, "</form>"); j >= 0 {
		form = form[:j]
	}
	attr := func(tag, name string) (string, bool) {
		m := regexp.MustCompile(`\s` + name + `="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil {
			return "", false
		}
		return htmlUnescape(m[1]), true
	}
	for _, tag := range regexp.MustCompile(`<input[^>]*>`).FindAllString(form, -1) {
		name, ok := attr(tag, "name")
		if !ok || name == "csrf_token" {
			continue
		}
		typ, _ := attr(tag, "type")
		val, _ := attr(tag, "value")
		switch typ {
		case "checkbox", "radio":
			if strings.Contains(tag, " checked") {
				if val == "" {
					val = "on"
				}
				out.Add(name, val)
			}
		case "submit", "button":
		default:
			out.Add(name, val)
		}
	}
	for _, m := range regexp.MustCompile(`(?s)<select[^>]*name="([^"]+)"[^>]*>(.*?)</select>`).FindAllStringSubmatch(form, -1) {
		if s := regexp.MustCompile(`<option[^>]*value="([^"]*)"[^>]*selected`).FindStringSubmatch(m[2]); s != nil {
			out.Set(m[1], htmlUnescape(s[1]))
		}
	}
	for _, m := range regexp.MustCompile(`(?s)<textarea[^>]*name="([^"]+)"[^>]*>(.*?)</textarea>`).FindAllStringSubmatch(form, -1) {
		out.Set(m[1], htmlUnescape(strings.TrimPrefix(m[2], "\n")))
	}
	return out
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&#34;", `"`, "&quot;", `"`, "&#39;", "'", "&amp;", "&").Replace(s)
}

// save submits a configuration page's form with the given overrides,
// keeping every other rendered value (and the version tokens).
func (b *browser) save(path string, overrides url.Values) (int, string) {
	b.t.Helper()
	vals := formFields(b.mustPage(path), path)
	for k, v := range overrides {
		vals[k] = v
	}
	code, body, _ := b.form(path, vals)
	return code, body
}

// p17Overview reads the overview read model.
func (r *p17Rig) overview(t *testing.T) readmodel.Overview {
	t.Helper()
	var ov readmodel.Overview
	if code := r.b.getJSON("/api/v1/projects/"+r.pid.String()+"/overview", &ov); code != 200 {
		t.Fatalf("overview: %d", code)
	}
	return ov
}

// TestP17147Acceptance — WBS 17.200: the Phase 17 final acceptance
// scenario, driven only through the web console and the normal /api/v1
// routes (the engine is driven step-by-step as the scheduler would; the
// browser never drives it), with every "verify throughout" item asserted.
func TestP17147Acceptance(t *testing.T) {
	base, restart := p16RestartRig(t, p16Options{noBootstrap: true, config: p17Config(nil)})
	r := &p17Rig{p16Rig: base, b: newBrowser(t, base.h.BaseURL)}
	b := r.b
	ctx := context.Background()

	// 1–7. Create the project and configure it through the web.
	dir := p15Repo(t)
	code, body, final := b.form("/projects", url.Values{"name": {"Acceptance <Web>"}, "working_directory": {dir}, "worker_provider": {"hermes"},
		"worker_model": {"hermes-worker"}, "compression_provider": {"fake"}, "compression_model": {"model-A"}, "context_window": {"64000"},
		"context_threshold": {"80"}, "planning_mode": {"ATOMIC"}, "use_git": {"yes"}})
	if code != 200 || !strings.Contains(body, "Project created.") {
		t.Fatalf("1 create: %d %s", code, textOf(body))
	}
	pid := strings.TrimPrefix(final, "/projects/")
	r.pid, r.dir, r.phases = uuid.MustParse(pid), dir, map[int]uuid.UUID{}
	p := "/projects/" + pid
	var st map[string]any
	b.getJSON("/api/v1"+p+"/settings", &st)
	if st["working_directory"] != dir { // 2
		t.Fatalf("2 working directory: %v", st["working_directory"])
	}
	if code, body := b.save(p+"/config/worker", url.Values{"worker_model": {"hermes-worker"}}); code != 200 { // 3
		t.Fatalf("3 worker: %d %s", code, textOf(body))
	}
	if code, body := b.save(p+"/config/compression", url.Values{"mode": {"AUTO"}}); code != 200 { // 4
		t.Fatalf("4 compression: %d %s", code, textOf(body))
	}
	if code, body := b.save(p+"/config/planning", url.Values{"mode": {"ATOMIC"}, "acceptance_criteria": {"on"}, "dependencies": {"on"}}); code != 200 { // 5
		t.Fatalf("5 planning: %d %s", code, textOf(body))
	}
	if code, body := b.save(p+"/config/continuation", url.Values{"context_threshold": {"80"}}); code != 200 { // 6
		t.Fatalf("6 continuation: %d %s", code, textOf(body))
	}
	if code, body := b.save(p+"/config/checkpoint", url.Values{"use_git": {"yes"}}); code != 200 { // 7
		t.Fatalf("7 checkpoint: %d %s", code, textOf(body))
	}
	// Deterministic validation commands (normal engine-settings API).
	if code, raw := b.api(http.MethodPut, "/api/v1"+p+"/settings/engine", map[string]any{"phase_validation": r.settings.PhaseValidation,
		"final_validation": r.settings.FinalValidation, "validation_timeout_seconds": 30, "final_hardening": r.hard}); code != 200 {
		t.Fatalf("engine settings: %d %s", code, raw)
	}
	var pl map[string]any
	b.getJSON("/api/v1"+p+"/settings/planning", &pl)
	var eng struct {
		Settings map[string]any `json:"settings"`
	}
	b.getJSON("/api/v1"+p+"/settings/engine", &eng)
	b.getJSON("/api/v1"+p+"/settings", &st)
	if pl["mode"] != "ATOMIC" || eng.Settings["checkpoint_policy"] != "REQUIRED" || st["context_threshold"] != 0.8 || st["worker_provider"] != "hermes" {
		t.Fatalf("configuration: planning %v checkpoint %v threshold %v", pl["mode"], eng.Settings["checkpoint_policy"], st["context_threshold"])
	}

	// 8–9. Three phases (append-only) and their WBS documents.
	for _, n := range []string{"Foundation", "API", "UI"} {
		if code, _, _ := b.form(p+"/phases", url.Values{"name": {n}}); code != 200 {
			t.Fatalf("8 append %s: %d", n, code)
		}
	}
	var phases struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	b.getJSON("/api/v1"+p+"/console/phases", &phases)
	if len(phases.Phases) != 3 {
		t.Fatalf("8 phases: %+v", phases.Phases)
	}
	for i, ph := range phases.Phases {
		r.phases[i+1] = uuid.MustParse(ph.ID)
		page := b.mustPage(p + "/phases/" + ph.ID + "/wbs")
		if code, _, _ := b.form(p+"/phases/"+ph.ID+"/wbs", url.Values{"content": {"# " + ph.Name + "\n- deliver"},
			"expected_version": {hidden(page, "expected_version")}}); code != 200 {
			t.Fatalf("9 WBS %s: %d", ph.Name, code)
		}
	}

	// 10–11. Review roles and the state machine (read-only pages).
	body = b.mustPage(p + "/roles")
	for _, k := range []string{"PLANNER", "ENGINEER", "TESTER"} {
		if !strings.Contains(body, k) {
			t.Fatalf("10 role %s missing", k)
		}
	}
	if body = b.mustPage(p + "/workflow"); !strings.Contains(body, "<svg") || !strings.Contains(body, "IMPLEMENT") {
		t.Fatal("11 workflow")
	}
	if code, body, _ := b.form(p+"/workflow/validate", nil); code != 200 || !strings.Contains(body, "Validation") {
		t.Fatalf("11 workflow validation: %d", code)
	}

	// 12–14. Readiness, bootstrap preview, start.
	if code, body, _ := b.form(p+"/ready", nil); code != 200 || !strings.Contains(textOf(body), "Project marked READY.") {
		t.Fatalf("12 ready: %d %s", code, textOf(body))
	}
	if text := textOf(b.mustPage(p + "/readiness")); !strings.Contains(text, "Ready to Start") {
		t.Fatalf("12 readiness: %s", text)
	}
	if text := textOf(b.mustPage(p + "/start")); !strings.Contains(text, "PREVIEW — NOTHING STARTED YET") || !strings.Contains(text, "REQUIRED") {
		t.Fatalf("13 preview: %s", text)
	}
	if r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()) != 0 {
		t.Fatal("13 preview created an execution")
	}
	// The SSE stream is opened before the start and kept for the run.
	ch, cancel, code := b.stream("/api/v1"+p+"/events/stream", "0")
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	if code, _, final := b.form(p+"/start", url.Values{"confirm": {"yes"}}); code != 200 || !strings.HasSuffix(final, "/execution") {
		t.Fatalf("14 start: %d %s", code, final)
	}
	ov := r.overview(t)
	if ov.Execution == nil || ov.Status != "RUNNING" {
		t.Fatalf("14 started: %+v", ov)
	}
	r.execID = uuid.MustParse(ov.Execution.ID)
	eid := ov.Execution.ID

	// Worker script. Phase 1: u1 INCOMPLETE → same-session continuation →
	// INCOMPLETE above the threshold → context reset → COMPLETE → PASS;
	// u2 COMPLETE (hostile summary) → validation FAIL → FIX → PASS.
	// Phase 2: v1; v2 carries a manual acceptance criterion (recovery).
	// Phase 3 and the appended Phase 4: one unit each.
	evil := `<script>alert(1)</script><div role=\"alert\">RECOVERY REQUIRED</div>`
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queueRaw(p16Reply("incomplete", "Half way.", ""), 1000)
	r.queueRaw(p16Reply("incomplete", "Almost there.", ""), 60000)
	r.queue("complete")
	r.queueRaw(p16Reply("complete", evil, ""), 2000)
	r.queue("complete") // FIX
	r.setCheck(t, "u2", 1)
	r.queuePlan(r.planJSON(p16Unit{key: "v1"}, p16Unit{key: "v2", deps: []string{"v1"}, manual: true}))
	r.queue("complete")
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "w1"}))
	r.queue("complete")
	r.queuePlan(r.planJSON(p16Unit{key: "x1"}))
	r.queue("complete")

	var full readmodel.Overview // overview at phase-1 validation (100 %)
	var contextSeen readmodel.ContextUsage
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" && r.state(t) == "FIX" {
			r.setCheck(t, "u2", 0) // the worker's fix works
		}
		if a == execution.ActionPhaseValidation && stage == "validation" && full.Plan == nil {
			b.getJSON("/api/v1"+p+"/overview", &full)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)

	// checkProgress: displayed progress is DONE / total of the current
	// plan, computed by the backend.
	checkProgress := func(ov readmodel.Overview) {
		if ov.Plan == nil {
			return
		}
		var done, total int
		_ = r.h.DBScan(ctx, "SELECT count(*) FILTER (WHERE status = 'DONE'), count(*) FROM work_units WHERE execution_plan_id = '"+ov.Plan.ID+"'", &done, &total)
		if ov.Progress.Done != done || ov.Progress.Total != total || ov.Progress != readmodel.NewProgress(done, total) {
			t.Fatalf("progress %+v, want %d / %d", ov.Progress, done, total)
		}
		if ov.Context.Known && ov.Context.Used > contextSeen.Used {
			contextSeen = ov.Context
		}
	}

	// 15–24. Observe planning, the plan, execution, continuation, reset,
	// validation FAIL → FIX → PASS, checkpoints and WBS progress.
	var appended bool
	for i := 0; i < 300 && r.phaseStatus(t, 1) != "DONE"; i++ {
		r.run(t, 1)
		ov := r.overview(t)
		checkProgress(ov)
		// 25–26. While Phase 1 executes (u1 DONE), append Phase 4 and
		// edit future WBS documents; the executing phase stays protected.
		if !appended && strings.Contains(r.unitStatuses(t, 1), "u1:DONE") {
			appended = true
			before := ov
			if code, body, _ := b.form(p+"/phases", url.Values{"name": {"Hardening (appended)"}}); code != 200 {
				t.Fatalf("25 append: %d %s", code, textOf(body))
			}
			b.getJSON("/api/v1"+p+"/console/phases", &phases)
			if len(phases.Phases) != 4 || phases.Phases[3].Order != 4 || !phases.Phases[0].Current || phases.Phases[3].Current {
				t.Fatalf("25 appended: %+v", phases.Phases)
			}
			r.phases[4] = uuid.MustParse(phases.Phases[3].ID)
			for _, i := range []int{2, 3} { // Phase 3 (v2) and Phase 4 (v1)
				id := phases.Phases[i].ID
				page := b.mustPage(p + "/phases/" + id + "/wbs")
				if code, body, _ := b.form(p+"/phases/"+id+"/wbs", url.Values{"content": {"# " + phases.Phases[i].Name + " (edited)\n- deliver more"},
					"expected_version": {hidden(page, "expected_version")}}); code != 200 {
					t.Fatalf("26 WBS edit: %d %s", code, textOf(body))
				}
			}
			// Optimistic concurrency: a stale expected version is refused.
			if code, body, _ := b.form(p+"/phases/"+phases.Phases[2].ID+"/wbs", url.Values{"content": {"# stale"}, "expected_version": {"1"}}); code != 409 ||
				!strings.Contains(body, "# stale") {
				t.Fatalf("stale WBS save: %d", code)
			}
			// The executing phase's WBS is protected.
			page := b.mustPage(p + "/phases/" + phases.Phases[0].ID + "/wbs")
			if code, _, _ := b.form(p+"/phases/"+phases.Phases[0].ID+"/wbs", url.Values{"content": {"# rewrite"},
				"expected_version": {hidden(page, "expected_version")}}); code != 409 {
				t.Fatalf("executing WBS edit: %d", code)
			}
			// Future WBS edits do not disrupt current execution.
			after := r.overview(t)
			if after.Plan == nil || before.Plan == nil || after.Plan.ID != before.Plan.ID || after.Staleness.Stale || after.Phase.ID != before.Phase.ID {
				t.Fatalf("future edit disrupted execution: %+v → %+v", before.Plan, after.Plan)
			}
		}
	}
	if !appended {
		t.Fatal("25 append never happened")
	}
	if full.Plan == nil || full.Progress.Percent != 100 || full.Phase == nil || full.Phase.Status == "DONE" {
		t.Fatalf("100%% shown as phase DONE before validation: %+v %+v", full.Progress, full.Phase)
	}
	if !contextSeen.Known || contextSeen.Window != 64000 || contextSeen.ResetThreshold != 80 {
		t.Fatalf("worker context not shown separately: %+v", contextSeen)
	}
	hist := r.statesSeq(t)
	for _, w := range []string{"CONTINUE_DECISION→SAME_SESSION→CONTINUE", "CONTINUE_DECISION→RESET_SESSION→CONTINUE", "VALIDATION_RESULT→FAIL→FIX"} {
		if !strings.Contains(hist, w) {
			t.Fatalf("history lacks %s", w)
		}
	}
	var units readmodel.UnitPage
	b.getJSON("/api/v1"+p+"/console/phases/"+r.phases[1].String()+"/work-units", &units)
	u2 := ""
	for _, u := range units.Rows {
		if u.Key == "u2" {
			u2 = u.ID
		}
	}
	text := textOf(b.mustPage(p + "/work-units/" + u2))
	for _, want := range []string{"COMPLETE (worker report)", "DONE (validated, checkpointed)", "Fix Attempt 1 / 3", "FAIL", "PASS"} {
		if !strings.Contains(text, want) {
			t.Fatalf("23 unit page lacks %q", want)
		}
	}
	var cps struct {
		Checkpoints []map[string]any `json:"checkpoints"`
	}
	b.getJSON("/api/v1"+p+"/console/checkpoints?execution="+eid, &cps)
	if len(cps.Checkpoints) < 2 {
		t.Fatalf("23 checkpoints: %d", len(cps.Checkpoints))
	}
	b.mustPage(p + "/checkpoints/" + cps.Checkpoints[0]["id"].(string))

	// Browser disconnection does not stop execution: the stream is closed
	// while the backend keeps working.
	got := collect(ch, 100000, 2*time.Second)
	cancel()
	lastSeen := got[len(got)-1].ID

	// 27. Phase advancement: Phase 2 becomes current.
	for i := 0; i < 50; i++ {
		ov = r.overview(t)
		if ov.Phase != nil && ov.Phase.Order == 2 && strings.Contains(r.unitStatuses(t, 2), "v1:DONE") {
			break
		}
		r.run(t, 1)
	}
	if ov.Phase == nil || ov.Phase.Order != 2 || r.phaseStatus(t, 1) != "DONE" {
		t.Fatalf("27 phase advancement: %+v", ov.Phase)
	}

	// 28–30. Pause, refresh (a fresh browser), resume.
	if code := b.rawForm(p+"/pause", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("pause without CSRF token: %d", code)
	}
	if code, body := b.page(p + "/pause"); code == 200 && strings.Contains(body, "Project paused.") {
		t.Fatal("GET paused the project")
	}
	if r.execStatus(t) != "RUNNING" {
		t.Fatal("rejected requests changed the execution")
	}
	if code, body, _ := b.form(p+"/pause", url.Values{"reason": {"lunch"}}); code != 200 || !strings.Contains(body, "Project paused.") {
		t.Fatalf("28 pause: %d", code)
	}
	paused := r.overview(t)
	sends := len(r.g.Messages())
	r.eng().Run(ctx, r.execID, 20)
	if len(r.g.Messages()) != sends || r.execStatus(t) != "PAUSED" {
		t.Fatal("28 work dispatched while paused")
	}
	b2 := newBrowser(t, r.h.BaseURL) // 29: refresh — no local state needed
	for i := 0; i < 3; i++ {
		b2.mustPage(p)
		b2.mustPage(p + "/execution")
	}
	var refreshed readmodel.Overview
	b2.getJSON("/api/v1"+p+"/overview", &refreshed)
	if refreshed.Status != "PAUSED" || refreshed.Runtime.StateKey != paused.Runtime.StateKey || refreshed.LatestEventSeq != paused.LatestEventSeq || !refreshed.HasAction("RESUME") {
		t.Fatalf("29 refresh altered execution: %+v vs %+v", refreshed, paused)
	}
	r.b, b = b2, b2
	if code, _, _ := b.form(p+"/resume", url.Values{"reason": {"back"}}); code != 200 {
		t.Fatal("30 resume")
	}
	if ov = r.overview(t); ov.Status != "RUNNING" || ov.Runtime.StateKey != paused.Runtime.StateKey {
		t.Fatalf("30 resume did not restore the exact state: %s %s", ov.Status, ov.Runtime.StateKey)
	}
	// Live-event gap: reconnecting with the last seen id replays exactly
	// the missed events (no duplicates, no gaps).
	ch, cancel, _ = b.stream("/api/v1"+p+"/events/stream", lastSeen)
	replay := collect(ch, 100000, 2*time.Second)
	prev, _ := strconv.ParseInt(lastSeen, 10, 64)
	for _, e := range replay {
		id, _ := strconv.ParseInt(e.ID, 10, 64)
		if id != prev+1 {
			t.Fatalf("replay gap / duplicate: %d after %d", id, prev)
		}
		prev = id
	}
	if ov = r.overview(t); prev != ov.LatestEventSeq {
		t.Fatalf("replay ends at %d, overview at %d", prev, ov.LatestEventSeq)
	}

	// 31–34. v2 stops for review: RECOVERY REQUIRED with evidence and only
	// backend-approved actions; the approved recovery resumes execution.
	r.runUntil(t, execution.StepRecoveryRequired, 100)
	ov = r.overview(t)
	if !ov.Recovery || ov.HasAction("START") || ov.HasAction("RESUME") || ov.HasAction("STEP") || ov.HasAction("PAUSE") || !ov.HasAction("RESOLVE_RECOVERY") {
		t.Fatalf("31 recovery controls: %v", ov.Actions)
	}
	if !strings.Contains(b.mustPage(p), "RECOVERY REQUIRED") {
		t.Fatal("31 recovery banner")
	}
	body = b.mustPage(p + "/executions/" + eid + "/recovery")
	text = textOf(body)
	if !strings.Contains(text, "Evidence") || !strings.Contains(text, "manual review") { // 32
		t.Fatalf("32 evidence: %s", text)
	}
	for _, m := range regexp.MustCompile(`name="action" value="([A-Z_]+)"`).FindAllStringSubmatch(body, -1) {
		if m[1] != "APPROVE_CRITERIA" && m[1] != "MANUAL_RESUME" {
			t.Fatalf("31 unapproved action form %s", m[1])
		}
	}
	if code, raw := b.api(http.MethodPost, "/api/v1"+p+"/executions/"+eid+"/recovery/actions", map[string]any{"action": "RECONCILE_DISPATCH", "reason": "x"}); code != 409 {
		t.Fatalf("31 unoffered action: %d %s", code, raw)
	}
	sends = len(r.g.Messages())
	if code, _, _ := b.form(p+"/executions/"+eid+"/recovery", url.Values{"action": {"APPROVE_CRITERIA"}, "confirm": {"yes"},
		"idempotency_key": {"p17147-approve-v2"},
		"reason":          {"reviewed v2 by hand"}}); code != 200 { // 33
		t.Fatalf("33 approve: %d", code)
	}
	if len(r.g.Messages()) != sends {
		t.Fatal("33 recovery dispatched")
	}

	// 34–36. Continue: Phase 3, the appended Phase 4, final validation,
	// completion. The edited future WBS is what was planned.
	ch2, cancel2, _ := b.stream("/api/v1"+p+"/events/stream", strconv.FormatInt(prev, 10))
	r.runUntil(t, execution.StepCompleted, 400)
	tail := collect(ch2, 100000, 2*time.Second)
	cancel()
	cancel2()
	evs := eventTypes(append(append(append([]sseEvent{}, got...), replay...), tail...))
	for _, want := range []string{"PLAN_ACTIVATED", "VALIDATION_FAILED", "FIX_ATTEMPT", "CHECKPOINT_CREATED", "WORK_UNIT_DONE",
		"PHASE_DONE", "EXECUTION_RECOVERY_RESOLVED", "FINAL_VALIDATION_PASSED", "PROJECT_COMPLETED"} {
		if indexOf(evs, want, 0) < 0 {
			t.Fatalf("stream lacks %s", want)
		}
	}
	b.getJSON("/api/v1"+p+"/console/phases", &phases)
	for _, ph := range phases.Phases {
		if ph.Status != "DONE" {
			t.Fatalf("35 phase %d %s", ph.Order, ph.Status)
		}
	}
	if phases.Phases[2].Plan == nil || phases.Phases[2].Plan.SourceWBS != 2 || phases.Phases[3].Plan == nil || phases.Phases[3].Plan.SourceWBS != 1 {
		t.Fatal("26 edited WBS not used for planning")
	}
	text = textOf(b.mustPage(p))
	for _, want := range []string{"COMPLETED", "All Phases DONE (4 / 4)", "Final Validation PASS", "Final Checkpoint CREATED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("36 final view lacks %q", want)
		}
	}

	// 37. Full audit history (page + API).
	var audit readmodel.AuditPage
	b.getJSON("/api/v1/audit?project="+pid+"&limit=200", &audit)
	actions := map[string]int{}
	for _, row := range audit.Rows {
		actions[row.Action+":"+row.Result]++
	}
	for _, want := range []string{"PROJECT_CREATE:SUCCEEDED", "CONFIG_EDIT:SUCCEEDED", "PHASE_APPEND:SUCCEEDED", "WBS_EDIT:SUCCEEDED", "WBS_EDIT:REJECTED",
		"PROJECT_UPDATE:SUCCEEDED", "LIFECYCLE_START:SUCCEEDED", "LIFECYCLE_PAUSE:SUCCEEDED", "LIFECYCLE_RESUME:SUCCEEDED", "RECOVERY_ACTION:SUCCEEDED",
		"RECOVERY_ACTION:REJECTED"} {
		if actions[want] == 0 {
			t.Fatalf("37 audit lacks %s: %v", want, actions)
		}
	}
	// The browser never drove workflow state: no engine step, no generic
	// state write — only the typed human actions above.
	for k := range actions {
		switch strings.SplitN(k, ":", 2)[0] {
		case "PROJECT_CREATE", "PROJECT_UPDATE", "CONFIG_EDIT", "PHASE_APPEND", "WBS_EDIT", "LIFECYCLE_START", "LIFECYCLE_PAUSE",
			"LIFECYCLE_RESUME", "RECOVERY_ACTION", "PREVIEW":
		default:
			t.Fatalf("unexpected human action %s", k)
		}
	}
	if text := textOf(b.mustPage("/audit?project=" + pid)); !strings.Contains(text, "LIFECYCLE_PAUSE") || !strings.Contains(text, "RECOVERY_ACTION") {
		t.Fatalf("37 audit page: %s", text)
	}
	approved := ""
	for _, row := range audit.Rows {
		if row.Action == "RECOVERY_ACTION" && row.Result == "SUCCEEDED" && row.Reason == "reviewed v2 by hand" {
			approved = row.ID
		}
	}
	if approved == "" || !strings.Contains(textOf(b.mustPage("/audit/"+approved)), "reviewed v2 by hand") {
		t.Fatal("37 recovery reason not audited")
	}

	// 38–40. Restart overseer; reopen with a new browser; historical
	// lineage from the database alone.
	snapshotBefore := func(b *browser) string {
		_, raw := b.api(http.MethodGet, "/api/v1"+p+"/executions/"+eid+"/configuration", nil)
		return string(raw)
	}
	snap := snapshotBefore(b)
	h := restart()
	b = newBrowser(t, h.BaseURL)
	r.b = b
	ov = readmodel.Overview{}
	b.getJSON("/api/v1"+p+"/overview", &ov)
	if ov.Status != "COMPLETED" || ov.Execution == nil || ov.Execution.Status != "COMPLETED" || ov.CurrentTask != nil || len(ov.Actions) != 0 {
		t.Fatalf("39 reopened: %+v", ov)
	}
	if snapshotBefore(b) != snap || !strings.Contains(snap, "hermes-worker") {
		t.Fatal("40 configuration snapshot changed")
	}
	var tl readmodel.Timeline
	b.getJSON("/api/v1"+p+"/console/executions/"+eid+"/timeline?limit=500", &tl)
	if len(tl.Rows) < 50 {
		t.Fatalf("40 timeline: %d rows", len(tl.Rows))
	}
	b.mustPage(p + "/executions/" + eid)
	pages := crawl(t, b, []string{p}, 600, pid)

	// Verify throughout (database facts + every rendered page).
	q := func(sql string) int { return r.count(t, sql) }
	if n := q("SELECT count(*) FROM work_units w WHERE w." + r.where() + " AND w.status = 'DONE' AND NOT EXISTS (SELECT 1 FROM validation_runs v WHERE v.id = w.done_validation_run_id AND v.status = 'PASS')"); n != 0 {
		t.Fatalf("%d DONE units without a passing deterministic validation", n)
	}
	if n := q("SELECT count(*) FROM validation_runs v WHERE v.execution_id = '" + eid + "' AND v.status IN ('PASS','FAIL') AND NOT EXISTS (SELECT 1 FROM validation_command_results c WHERE c.run_id = v.id)"); n != 0 {
		t.Fatal("validation result without executed commands")
	}
	if q("SELECT count(*) FROM phases WHERE "+r.where()) != 4 || q("SELECT count(DISTINCT phase_order) FROM phases WHERE "+r.where()) != 4 {
		t.Fatal("appended phases not append-only")
	}
	if n := q("SELECT count(*) FROM (SELECT dispatch_id FROM execution_steps WHERE execution_id = '" + eid + "' AND dispatch_id IS NOT NULL AND action IN ('DISPATCH','PLAN_DISPATCH') AND detail->>'adopted' IS NULL GROUP BY dispatch_id HAVING count(*) > 1) x"); n != 0 {
		t.Fatal("duplicate dispatch")
	}
	secrets := []string{p12Token, p9SentinelKey}
	for path, body := range pages {
		if strings.Contains(body, "<script>alert") || strings.Contains(body, `<div role="alert">RECOVERY`) {
			t.Fatalf("%s renders worker markup", path)
		}
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Fatalf("%s leaks a secret", path)
			}
		}
		// The browser never offers generic state mutation: every form
		// posts to a typed console route.
		for _, m := range regexp.MustCompile(`<form[^>]*method="post"[^>]*action="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			if strings.Contains(m[1], "/status") || strings.Contains(m[1], "/step") {
				t.Fatalf("%s offers a generic state form %s", path, m[1])
			}
		}
	}
	for _, e := range append(append(got, replay...), tail...) {
		for _, s := range secrets {
			if strings.Contains(e.Data, s) {
				t.Fatal("secret in the event stream")
			}
		}
	}
	t.Logf("acceptance: %d pages, %d events, %d audit rows, history %d states", len(pages), len(evs), len(audit.Rows), len(r.history(t)))
}
