package integration

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"overseer/internal/readmodel"
)

func hidden(body, name string) string {
	m := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// P17-82 / P17-131 (WBS 17.8 / 17.9 / 17.183) — project creation through
// the web: backend field errors beside the fields, submitted values
// preserved and escaped, then a configured project → phases → WBS →
// readiness READY without database manipulation.
func TestP17131ProjectCreationThroughWeb(t *testing.T) {
	r := p17NewRig(t)
	b := r.b
	body := b.mustPage("/projects/new")
	if !strings.Contains(body, `<label for="name">Name</label>`) || !strings.Contains(body, `name="worker_provider"`) {
		t.Fatal("create form incomplete")
	}
	code, body, _ := b.form("/projects", url.Values{"name": {`<i>x</i>`}, "working_directory": {"/no/such/dir"}, "worker_provider": {"hermes"},
		"context_window": {"0"}, "context_threshold": {"80"}, "planning_mode": {"ATOMIC"}, "use_git": {"yes"}})
	if code != 422 {
		t.Fatalf("invalid form: %d", code)
	}
	text := textOf(body)
	for _, want := range []string{"must be an existing, accessible directory", "must be a positive token count", "required"} {
		if !strings.Contains(text, want) {
			t.Fatalf("field error %q missing", want)
		}
	}
	if strings.Contains(body, "<i>x</i>") || !strings.Contains(body, "&lt;i&gt;x&lt;/i&gt;") {
		t.Fatal("submitted value not preserved escaped")
	}
	// Valid submission → overview.
	dir := p15Repo(t)
	code, body, final := b.form("/projects", url.Values{"name": {"Web Project"}, "working_directory": {dir}, "worker_provider": {"hermes"},
		"worker_model": {"m-1"}, "compression_provider": {"fake"}, "compression_model": {"c-1"}, "context_window": {"64000"},
		"context_threshold": {"80"}, "planning_mode": {"ATOMIC"}, "use_git": {"yes"}})
	if code != 200 || !strings.HasPrefix(final, "/projects/") || !strings.Contains(body, "Project created.") {
		t.Fatalf("create: %d %s", code, final)
	}
	newID := strings.TrimPrefix(final, "/projects/")
	// Append three phases: order assigned by the server; no order field.
	for _, n := range []string{"Foundation", "API", "UI"} {
		if code, _, _ := b.form("/projects/"+newID+"/phases", url.Values{"name": {n}, "phase_order": {"1"}}); code != 200 {
			t.Fatalf("append %s: %d", n, code)
		}
	}
	var phases struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	b.getJSON("/api/v1/projects/"+newID+"/console/phases", &phases)
	if len(phases.Phases) != 3 || phases.Phases[0].Name != "Foundation" || phases.Phases[2].Order != 3 {
		t.Fatalf("phases: %+v", phases.Phases)
	}
	if body := b.mustPage("/projects/" + newID + "/phases"); !strings.Contains(body, "New phases are appended to the end of the project.") ||
		strings.Contains(body, `name="phase_order"`) {
		t.Fatal("append UI must state append-only and offer no order field")
	}
	// WBS for each phase through the editor.
	for _, p := range phases.Phases {
		page := b.mustPage("/projects/" + newID + "/phases/" + p.ID + "/wbs")
		if code, _, _ := b.form("/projects/"+newID+"/phases/"+p.ID+"/wbs", url.Values{"content": {"# " + p.Name + "\n- deliver"},
			"expected_version": {hidden(page, "expected_version")}}); code != 200 {
			t.Fatalf("WBS %s: %d", p.Name, code)
		}
	}
	// Readiness page: backend-derived checks, then the project can be
	// marked READY through the normal API (Phase 3).
	body = b.mustPage("/projects/" + newID + "/readiness")
	if !strings.Contains(body, "Execution Readiness") || !strings.Contains(body, "Working directory") {
		t.Fatal("readiness page")
	}
	if code, body, _ := b.form("/projects/"+newID+"/ready", nil); code != 200 || !strings.Contains(body, "Project marked READY.") {
		t.Fatalf("ready: %d %s", code, textOf(body))
	}
	var ov readmodel.Overview
	b.getJSON("/api/v1/projects/"+newID+"/overview", &ov)
	if ov.Status != "READY" || !ov.HasAction("START") {
		t.Fatalf("status %s actions %v", ov.Status, ov.Actions)
	}
	// Execution readiness (Phase 15) is satisfied for the web-configured
	// project: roles, workflow and protocol came from the defaults.
	if text := textOf(b.mustPage("/projects/" + newID + "/readiness")); !strings.Contains(text, "Ready to Start") {
		t.Fatalf("execution readiness: %s", text)
	}
}

// P17-86 / P17-128 (WBS 17.20 / 17.133 / 17.139 / 17.140 / 17.182) — two
// tabs edit the same WBS / configuration: one wins, the stale one gets a
// conflict with its text preserved and nothing overwritten.
func TestP17128ConcurrentEdits(t *testing.T) {
	r := p17NewRig(t)
	pid, ph := r.pid.String(), r.phases[1].String()
	tabA, tabB := newBrowser(t, r.h.BaseURL), newBrowser(t, r.h.BaseURL)
	pageA := tabA.mustPage("/projects/" + pid + "/phases/" + ph + "/wbs")
	pageB := tabB.mustPage("/projects/" + pid + "/phases/" + ph + "/wbs")
	if code, _, _ := tabA.form("/projects/"+pid+"/phases/"+ph+"/wbs", url.Values{"content": {"# A"}, "expected_version": {hidden(pageA, "expected_version")}}); code != 200 {
		t.Fatalf("A: %d", code)
	}
	code, body, _ := tabB.form("/projects/"+pid+"/phases/"+ph+"/wbs", url.Values{"content": {"# B edits"}, "expected_version": {hidden(pageB, "expected_version")}})
	if code != 409 || !strings.Contains(body, "Conflict — not saved") || !strings.Contains(body, "# B edits") || !strings.Contains(body, "Reload the current version") {
		t.Fatalf("B: %d %s", code, textOf(body))
	}
	var doc map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/phases/"+ph+"/wbs", &doc)
	if doc["content"] != "# A" {
		t.Fatalf("overwritten: %v", doc["content"])
	}
	// Configuration: same pattern on the planning page.
	pageA = tabA.mustPage("/projects/" + pid + "/config/planning")
	pageB = tabB.mustPage("/projects/" + pid + "/config/planning")
	if code, _, _ := tabA.form("/projects/"+pid+"/config/planning", url.Values{"mode": {"FEATURE"}, "config_version": {hidden(pageA, "config_version")}}); code != 200 {
		t.Fatalf("A config: %d", code)
	}
	if code, body, _ := tabB.form("/projects/"+pid+"/config/planning", url.Values{"mode": {"COMPONENT"}, "config_version": {hidden(pageB, "config_version")}}); code != 409 ||
		!strings.Contains(body, "Conflict — not saved") {
		t.Fatalf("B config: %d", code)
	}
	var pl map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/settings/planning", &pl)
	if pl["mode"] != "FEATURE" {
		t.Fatalf("config overwritten: %v", pl)
	}
	// Both tabs converge on the backend state after a refresh.
	if !strings.Contains(tabB.mustPage("/projects/"+pid+"/config/planning"), `value="FEATURE" checked`) {
		t.Fatal("tab B does not converge after reload")
	}
}

// P17-108 .. P17-111 / P17-132 (WBS 17.64–17.72, 17.165, 17.166, 17.184) —
// readiness → preview → start through the web; a double submit yields one
// execution; pause / resume / cancel through lifecycle with confirmation.
func TestP17132StartThroughWeb(t *testing.T) {
	r := p17NewRigWith(t, p16Options{noBootstrap: true})
	pid := r.pid.String()
	b := r.b
	body := b.mustPage("/projects/" + pid + "/readiness")
	if !strings.Contains(textOf(body), "Ready to Start") {
		t.Fatalf("readiness: %s", textOf(body))
	}
	body = b.mustPage("/projects/" + pid + "/start")
	text := textOf(body)
	for _, want := range []string{"PREVIEW — NOTHING STARTED YET", "Working Directory", "Current Phase", "Worker", "Model", "Checkpoint Policy", "Planning Policy", "REQUIRED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("confirmation lacks %q: %s", want, text)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE project_id = '"+pid+"'"); n != 0 {
		t.Fatal("preview created an execution")
	}
	// Start without confirmation is refused.
	if code, _, _ := b.form("/projects/"+pid+"/start", url.Values{}); code != 400 {
		t.Fatalf("unconfirmed start: %d", code)
	}
	// Double submit (rapid repeat): exactly one execution.
	code1, _, final := b.form("/projects/"+pid+"/start", url.Values{"confirm": {"yes"}})
	code2, _, final2 := b.form("/projects/"+pid+"/start", url.Values{"confirm": {"yes"}})
	if code1 != 200 || !strings.HasSuffix(final, "/execution") || code2 != 200 || !strings.HasSuffix(final2, "/execution") {
		t.Fatalf("start: %d %s / %d %s", code1, final, code2, final2)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE project_id = '"+pid+"'"); n != 1 {
		t.Fatalf("executions = %d", n)
	}
	var ov readmodel.Overview
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "RUNNING" || ov.Execution == nil || ov.Execution.Status != "RUNNING" || !ov.HasAction("PAUSE") || ov.HasAction("START") {
		t.Fatalf("after start: %+v", ov)
	}
	// Pause → only Resume / Cancel offered; resume → exact state.
	state := ov.Runtime.StateKey
	if code, body, _ := b.form("/projects/"+pid+"/pause", nil); code != 200 || !strings.Contains(body, "Project paused.") {
		t.Fatalf("pause: %d", code)
	}
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "PAUSED" || !ov.HasAction("RESUME") || ov.HasAction("PAUSE") {
		t.Fatalf("paused: %+v", ov.Actions)
	}
	if body := b.mustPage("/projects/" + pid); !strings.Contains(body, `action="/projects/`+pid+`/resume"`) || strings.Contains(body, `action="/projects/`+pid+`/pause"`) {
		t.Fatal("controls not rendered from backend actions")
	}
	if code, _, _ := b.form("/projects/"+pid+"/resume", nil); code != 200 {
		t.Fatal("resume")
	}
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "RUNNING" || ov.Runtime.StateKey != state {
		t.Fatalf("resume did not restore the exact state: %s / %s", ov.Status, ov.Runtime.StateKey)
	}
	// Cancel requires confirmation + reason, and explains reconciliation.
	if !strings.Contains(textOf(b.mustPage("/projects/"+pid+"/cancel")), "already delivered may still need reconciliation") {
		t.Fatal("cancel page lacks the reconciliation notice")
	}
	if code, _, _ := b.form("/projects/"+pid+"/cancel", url.Values{"confirm": {"yes"}}); code != 400 {
		t.Fatal("cancel without reason accepted")
	}
	if code, _, _ := b.form("/projects/"+pid+"/cancel", url.Values{"confirm": {"yes"}, "reason": {"scope dropped"}}); code != 200 {
		t.Fatal("cancel")
	}
	b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "CANCELLED" || len(ov.Actions) != 0 {
		t.Fatalf("cancelled: %+v", ov)
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE project_id = '"+pid+"' AND action IN ('LIFECYCLE_START','LIFECYCLE_PAUSE','LIFECYCLE_RESUME','LIFECYCLE_CANCEL') AND result = 'SUCCEEDED'"); n < 4 {
		t.Fatalf("lifecycle audit rows = %d", n)
	}
}

// P17-90 .. P17-107 (WBS 17.32–17.63) — configuration pages save through
// the backend services, which reject invalid input.
func TestP17ConfigurationForms(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	b := r.b
	// Roles: list from the service; edit creates versions; reset needs
	// confirmation.
	body := b.mustPage("/projects/" + pid + "/roles")
	for _, k := range []string{"ARCHITECT", "PLANNER", "ENGINEER", "TESTER", "PROJECT_MANAGER"} {
		if !strings.Contains(body, k) {
			t.Fatalf("role %s missing", k)
		}
	}
	roleID := regexp.MustCompile(`/projects/` + pid + `/roles/([0-9a-f-]{36})"`).FindStringSubmatch(body)[1]
	page := b.mustPage("/projects/" + pid + "/roles/" + roleID)
	vals := url.Values{"name": {"Engineer X"}, "description": {"d"}, "enabled": {"on"}, "system_prompt": {"You build things carefully."},
		"prompt_version": {hidden(page, "prompt_version")}, "allowed_states": {inputValue(page, "allowed_states")},
		"state_files": {inputValue(page, "state_files")}}
	if code, body, _ := b.form("/projects/"+pid+"/roles/"+roleID, vals); code != 200 {
		t.Fatalf("role save: %d %s", code, textOf(body))
	}
	vals.Set("system_prompt", "stale edit")
	if code, body, _ := b.form("/projects/"+pid+"/roles/"+roleID, vals); code != 409 || !strings.Contains(body, "Conflict") {
		t.Fatalf("stale role prompt: %d", code)
	}
	// The conflict left the prompt untouched.
	var cur struct {
		Content string `json:"content"`
		Version int    `json:"version"`
	}
	b.getJSON("/api/v1/projects/"+pid+"/roles/"+roleID+"/system-prompt", &cur)
	if cur.Content != "You build things carefully." || cur.Version != 2 {
		t.Fatalf("stale prompt edit was stored: %+v", cur)
	}
	// Set as default: the global default takes the saved role (so a reset
	// copies it back unchanged).
	if code, _, _ := b.form("/projects/"+pid+"/roles/"+roleID+"/set-default", url.Values{"confirm": {"yes"}}); code != 400 {
		t.Fatal("set as default without reason accepted")
	}
	if code, body, _ := b.form("/projects/"+pid+"/roles/"+roleID+"/set-default", url.Values{"confirm": {"yes"}, "reason": {"keep this engineer"}}); code != 200 ||
		!strings.Contains(body, "Role saved as the global default.") {
		t.Fatalf("set as default: %d", code)
	}
	if code, _, _ := b.form("/projects/"+pid+"/roles/"+roleID+"/reset", url.Values{"confirm": {"yes"}}); code != 400 {
		t.Fatal("reset without reason accepted")
	}
	if code, _, _ := b.form("/projects/"+pid+"/roles/"+roleID+"/reset", url.Values{"confirm": {"yes"}, "reason": {"back to defaults"}}); code != 200 {
		t.Fatal("reset")
	}
	var role struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var prompt struct {
		Content string `json:"content"`
	}
	b.getJSON("/api/v1/projects/"+pid+"/roles/"+roleID, &role)
	b.getJSON("/api/v1/projects/"+pid+"/roles/"+roleID+"/system-prompt", &prompt)
	if role.Name != "Engineer X" || role.Description != "d" || prompt.Content != "You build things carefully." {
		t.Fatalf("reset after set as default: %+v %q", role, prompt.Content)
	}
	// Workflow: diagram from records, validation lists issues.
	body = b.mustPage("/projects/" + pid + "/workflow")
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "PHASE_PLAN") {
		t.Fatal("workflow diagram")
	}
	if code, body, _ := b.form("/projects/"+pid+"/workflow/validate", nil); code != 200 || !strings.Contains(body, "Validation") {
		t.Fatalf("validate: %d", code)
	}
	if code, body, _ := b.form("/projects/"+pid+"/workflow/states/PHASE_PLAN", url.Values{"name": {"Plan"}, "kind": {"BOGUS"}}); code != 400 || !strings.Contains(body, "Not saved") {
		t.Fatalf("invalid state kind: %d", code)
	}
	// Templates: unknown variable rejected before activation.
	var tl struct {
		Templates []map[string]any `json:"templates"`
	}
	b.getJSON("/api/v1/projects/"+pid+"/prompt-templates", &tl)
	tid := tl.Templates[0]["id"].(string)
	page = b.mustPage("/projects/" + pid + "/templates/" + tid)
	if code, body, _ := b.form("/projects/"+pid+"/templates/"+tid, url.Values{"content": {"{{secret.value}}"}, "expected_version": {hidden(page, "expected_version")}}); code != 400 ||
		!strings.Contains(body, "UNKNOWN_VARIABLE") {
		t.Fatalf("unknown variable form: %d", code)
	}
	// Prompt preview never dispatches. At PHASE_PLAN the position is not
	// composable without captured WBS inputs: a clear 409, not a 500.
	if code, body, _ := b.form("/projects/"+pid+"/prompts/preview", nil); code != 409 || !strings.Contains(body, "not composable") {
		t.Fatalf("preview at PHASE_PLAN: %d", code)
	}
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queue("complete")
	r.run(t, 5)
	before := r.count(t, "SELECT count(*) FROM worker_dispatches")
	if code, body, _ := b.form("/projects/"+pid+"/prompts/preview", nil); code != 200 || !strings.Contains(body, "PREVIEW — NOT DISPATCHED") {
		t.Fatalf("preview: %d %s", code, textOf(body))
	}
	if r.count(t, "SELECT count(*) FROM worker_dispatches") != before {
		t.Fatal("preview dispatched")
	}
	// Worker configuration: secret shown as configured, never the value;
	// no secret input exists.
	body = b.mustPage("/projects/" + pid + "/config/worker")
	if !strings.Contains(textOf(body), "configured") || strings.Contains(body, "tok-P12-GATEWAY-SENTINEL") || strings.Contains(body, `type="password"`) {
		t.Fatal("worker config secret handling")
	}
	// Compression: invalid combination rejected with the field shown.
	page = b.mustPage("/projects/" + pid + "/config/compression")
	if code, body, _ := b.form("/projects/"+pid+"/config/compression", url.Values{"mode": {"AUTO"}, "target_tokens": {"0"}, "target_ratio": {"0"},
		"max_attempts": {"0"}, "validation_mode": {"DETERMINISTIC"}, "config_version": {hidden(page, "config_version")}}); code != 400 || !strings.Contains(body, "Not saved") {
		t.Fatalf("invalid compression: %d", code)
	}
	// "Use git" sets both checkpoint policies: on → REQUIRED, off → DISABLED.
	for _, tc := range []struct {
		useGit bool
		want   string
	}{{true, "REQUIRED"}, {false, "DISABLED"}} {
		page = b.mustPage("/projects/" + pid + "/config/checkpoint")
		form := url.Values{"config_version": {hidden(page, "config_version")}}
		if tc.useGit {
			form.Set("use_git", "yes")
		}
		if code, _, _ := b.form("/projects/"+pid+"/config/checkpoint", form); code != 200 {
			t.Fatalf("checkpoint save (use git %v)", tc.useGit)
		}
		var eng struct {
			Settings struct {
				CheckpointPolicy string `json:"checkpoint_policy"`
				UseGit           bool   `json:"use_git"`
				FinalHardening   struct {
					GroupCheckpointPolicy string `json:"group_checkpoint_policy"`
				} `json:"final_hardening"`
			} `json:"settings"`
		}
		b.getJSON("/api/v1/projects/"+pid+"/settings/engine", &eng)
		if s := eng.Settings; s.CheckpointPolicy != tc.want || s.UseGit != tc.useGit || s.FinalHardening.GroupCheckpointPolicy != tc.want {
			t.Fatalf("use git %v: %+v", tc.useGit, s)
		}
	}
	// Continuation: Phase 14 validation enforced.
	page = b.mustPage("/projects/" + pid + "/config/continuation")
	if code, _, _ := b.form("/projects/"+pid+"/config/continuation", url.Values{"max_continuations_per_work_unit": {"0"}, "max_session_resets_per_work_unit": {"1"},
		"max_no_progress_continuations": {"1"}, "unknown_context_policy": {"fresh_session"}, "context_threshold": {"80"},
		"config_version": {hidden(page, "config_version")}, "project_version": {hidden(page, "project_version")}}); code != 400 {
		t.Fatalf("invalid continuation: %d", code)
	}
}

// inputValue reads a text input's rendered value (unescaping entities).
func inputValue(body, name string) string {
	m := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" type="text" value="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}
