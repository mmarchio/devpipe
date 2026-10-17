package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

func errFields(t *testing.T, raw []byte) (string, map[string]string, []map[string]string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string              `json:"code"`
			Fields  map[string]string   `json:"fields"`
			Details []map[string]string `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.Error.Code, env.Error.Fields, env.Error.Details
}

// P17-47 (WBS 17.8 / 17.9) — configured creation through the Phase 3
// services; every field error reported; nothing created on failure.
func TestP1747ProjectSetup(t *testing.T) {
	r := p17NewRig(t)
	before := r.count(t, "SELECT count(*) FROM projects")
	code, raw := r.b.api(http.MethodPost, "/api/v1/console/projects", map[string]any{
		"name": "", "working_directory": "/definitely/not/here", "worker_provider": "nope", "worker_model": "",
		"compression_provider": "", "compression_model": "x", "context_window": 0, "context_threshold": 3,
		"planning": map[string]any{"mode": "AGILE"}, "checkpoint_policy": "SOMETIMES"})
	_, fields, details := errFields(t, raw)
	if code != 400 || len(fields) < 9 || len(details) != len(fields) {
		t.Fatalf("validation: %d %s", code, raw)
	}
	for _, f := range []string{"name", "working_directory", "worker_provider", "worker_model", "compression_model", "context_window",
		"context_threshold", "planning.mode", "checkpoint_policy"} {
		if fields[f] == "" {
			t.Fatalf("missing field error %s: %v", f, fields)
		}
	}
	if r.count(t, "SELECT count(*) FROM projects") != before {
		t.Fatal("project created despite validation errors")
	}
	code, raw = r.b.api(http.MethodPost, "/api/v1/console/projects", map[string]any{
		"name": "Trader", "description": "d", "working_directory": r.dir, "worker_provider": "hermes", "worker_model": "m-1",
		"context_window": 64000, "context_threshold": 0.8, "planning": map[string]any{"mode": "ATOMIC",
			"atomic": map[string]any{"require_independently_testable": true, "require_single_invocation": true}, "prefer_small_functions": true},
		"checkpoint_policy": "BEST_EFFORT"})
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &p)
	var st map[string]any
	r.b.getJSON("/api/v1/projects/"+p.ID+"/settings", &st)
	if st["working_directory"] != r.dir || st["worker_model"] != "m-1" || st["context_window"] != float64(64000) {
		t.Fatalf("settings: %v", st)
	}
	var pl map[string]any
	r.b.getJSON("/api/v1/projects/"+p.ID+"/settings/planning", &pl)
	atomic, _ := pl["atomic"].(map[string]any)
	if pl["mode"] != "ATOMIC" || pl["prefer_small_functions"] != true || atomic["require_single_invocation"] != true ||
		atomic["require_acceptance_criteria"] != false {
		t.Fatalf("planning persisted inexactly: %v", pl)
	}
	var eng struct {
		Settings map[string]any `json:"settings"`
	}
	r.b.getJSON("/api/v1/projects/"+p.ID+"/settings/engine", &eng)
	if eng.Settings["checkpoint_policy"] != "BEST_EFFORT" {
		t.Fatalf("checkpoint: %v", eng.Settings)
	}
	// The general settings PUT keeps the planning policy (P17-22 fix).
	st["worker_model"], st["version"] = "m-2", st["version"]
	if code, raw := r.b.api(http.MethodPut, "/api/v1/projects/"+p.ID+"/settings", st); code != 200 {
		t.Fatalf("settings PUT: %d %s", code, raw)
	}
	r.b.getJSON("/api/v1/projects/"+p.ID+"/settings/planning", &pl)
	if pl["mode"] != "ATOMIC" {
		t.Fatalf("planning reset by a settings update: %v", pl)
	}
	// Audit row for the console create.
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'PROJECT_CREATE' AND result = 'SUCCEEDED'"); n != 1 {
		t.Fatalf("create audit rows = %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'PROJECT_CREATE' AND result = 'REJECTED'"); n != 1 {
		t.Fatalf("rejected create audit rows = %d", n)
	}
}

// P17-46 (WBS 17.140 / 17.20) — the configuration version prevents silent
// overwrites; a writer that echoes it detects the concurrent change.
func TestP1746ConfigVersionGuard(t *testing.T) {
	r := p17NewRig(t)
	base := "/api/v1/projects/" + r.pid.String()
	code, raw := r.b.api(http.MethodGet, base+"/settings/continuation", nil)
	if code != 200 {
		t.Fatal(code)
	}
	_ = raw
	req, _ := http.NewRequest(http.MethodGet, r.h.BaseURL+base+"/settings/continuation", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	v, _ := strconv.Atoi(resp.Header.Get("X-Config-Version"))
	if v < 1 {
		t.Fatalf("no config version header: %q", resp.Header.Get("X-Config-Version"))
	}
	// Tab A saves with v → v+1.
	if code, raw := r.b.api(http.MethodPut, base+"/settings/continuation", map[string]any{"max_no_progress_continuations": 4, "version": v}); code != 200 {
		t.Fatalf("A: %d %s", code, raw)
	}
	// Tab B, still on v, conflicts and changes nothing.
	code, raw = r.b.api(http.MethodPut, base+"/settings/continuation", map[string]any{"max_no_progress_continuations": 9, "version": v})
	if code != 409 || !strings.Contains(string(raw), "stale_configuration") {
		t.Fatalf("B: %d %s", code, raw)
	}
	var s struct {
		Settings map[string]any `json:"settings"`
	}
	r.b.getJSON(base+"/settings/continuation", &s)
	if s.Settings["max_no_progress_continuations"] != float64(4) {
		t.Fatalf("overwritten: %v", s.Settings)
	}
	// A rejected write (400) does not consume a version.
	cur := v + 1
	if code, _ := r.b.api(http.MethodPut, base+"/settings/continuation", map[string]any{"max_continuations_per_work_unit": 0, "version": cur}); code != 400 {
		t.Fatalf("invalid: %d", code)
	}
	if code, raw := r.b.api(http.MethodPut, base+"/settings/continuation", map[string]any{"max_no_progress_continuations": 5, "version": cur}); code != 200 {
		t.Fatalf("after rejected write: %d %s", code, raw)
	}
	// Legacy clients without a version still work (and bump the version).
	if code, _ := r.b.api(http.MethodPut, base+"/settings/compression", map[string]any{"mode": "AUTO", "target_tokens": 4000, "max_attempts": 2}); code != 200 {
		t.Fatal("legacy write")
	}
	if code, _ := r.b.api(http.MethodPut, base+"/settings/continuation", map[string]any{"max_no_progress_continuations": 6, "version": cur + 1}); code != 409 {
		t.Fatal("legacy write did not bump the version")
	}
}

// P17-45 / P17-70 (WBS 17.48 / 17.49) — templates load dynamically;
// unknown variables are rejected before activation; versions CAS.
func TestP1745PromptTemplates(t *testing.T) {
	r := p17NewRig(t)
	base := "/api/v1/projects/" + r.pid.String() + "/prompt-templates"
	var l struct {
		Templates []map[string]any `json:"templates"`
	}
	r.b.getJSON(base, &l)
	if len(l.Templates) < 9 {
		t.Fatalf("templates: %d", len(l.Templates))
	}
	var id string
	var ver int
	for _, tm := range l.Templates {
		if tm["key"] == "IMPLEMENT_WORK_UNIT" {
			id, ver = tm["id"].(string), int(tm["current_version"].(float64))
		}
	}
	if id == "" {
		t.Fatal("IMPLEMENT_WORK_UNIT not listed")
	}
	code, raw := r.b.api(http.MethodPost, base+"/"+id+"/versions", map[string]any{"content": "{{work_unit.task}} {{project.api_key}}", "expected_version": ver})
	if code != 400 || !strings.Contains(string(raw), "project.api_key") {
		t.Fatalf("unknown variable: %d %s", code, raw)
	}
	code, raw = r.b.api(http.MethodPost, base+"/"+id+"/versions", map[string]any{"content": "# Objective\n{{work_unit.task}}", "expected_version": ver})
	if code != 201 {
		t.Fatalf("create version: %d %s", code, raw)
	}
	code, _ = r.b.api(http.MethodPost, base+"/"+id+"/versions", map[string]any{"content": "stale edit {{work_unit.task}}", "expected_version": ver})
	if code != 409 {
		t.Fatalf("stale version: %d", code)
	}
	var vs struct {
		Versions []map[string]any `json:"versions"`
	}
	r.b.getJSON(base+"/"+id+"/versions", &vs)
	if len(vs.Versions) != ver+1 || vs.Versions[0]["version"] != float64(ver+1) {
		t.Fatalf("versions: %v", vs.Versions)
	}
	if code := r.b.getJSON("/api/v1/projects/"+uuid.NewString()+"/prompt-templates/"+id, nil); code != 404 && code != 200 {
		t.Fatalf("cross project: %d", code)
	}
}

// P17-39 / P17-40 / P17-41 / P17-67 (WBS 17.103–17.106) — human actions are
// audited with actor, request id and reason; filters / paging server-side.
func TestP1767AuditTrail(t *testing.T) {
	r := p17NewRig(t)
	base := "/api/v1/projects/" + r.pid.String()
	req, _ := http.NewRequest(http.MethodPost, r.h.BaseURL+base+"/pause", nil)
	req.Header.Set("X-Overseer-Reason", "maintenance window")
	req.Header.Set("X-Request-ID", "req-pause-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("pause: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	if code, _ := r.b.api(http.MethodPost, base+"/pause", nil); code != 200 && code != 409 {
		t.Fatalf("second pause: %d", code)
	}
	r.b.api(http.MethodPost, base+"/resume", nil)
	var page readmodel.AuditPage
	if code := r.b.getJSON("/api/v1/audit?project="+r.pid.String()+"&action=LIFECYCLE_PAUSE", &page); code != 200 {
		t.Fatal(code)
	}
	if len(page.Rows) < 1 {
		t.Fatalf("pause not audited: %+v", page)
	}
	var first readmodel.AuditRow
	for _, row := range page.Rows {
		if row.RequestID == "req-pause-1" {
			first = row
		}
	}
	if first.Reason != "maintenance window" || first.ActorType != "LOCAL_ADMIN" || first.Result != "SUCCEEDED" || first.ProjectName == "" {
		t.Fatalf("audit row: %+v", first)
	}
	var d readmodel.AuditRow
	if code := r.b.getJSON("/api/v1/audit/"+first.ID, &d); code != 200 || d.ID != first.ID {
		t.Fatalf("detail: %d", code)
	}
	var all readmodel.AuditPage
	r.b.getJSON("/api/v1/audit?project="+r.pid.String()+"&limit=1", &all)
	if len(all.Rows) != 1 || all.NextCursor == "" {
		t.Fatalf("paging: %+v", all)
	}
	var next readmodel.AuditPage
	r.b.getJSON("/api/v1/audit?project="+r.pid.String()+"&limit=1&cursor="+all.NextCursor, &next)
	if len(next.Rows) != 1 || next.Rows[0].Seq >= all.Rows[0].Seq {
		t.Fatalf("cursor page: %+v", next)
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/audit?action=READ", nil); code != 400 {
		t.Fatalf("invalid filter: %d", code)
	}
	// GETs are never audited.
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE method = 'GET'"); n != 0 {
		t.Fatalf("GET audited %d times", n)
	}
}

// P17-44 / P17-69 (WBS 17.46 / 17.47 / 17.44) — drift indicator and
// policy-gated resolution; DERIVED files never import / accept edits.
func TestP1769DriftResolution(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	if code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/materialize", map[string]any{}); code != 200 {
		t.Fatalf("materialize: %d %s", code, raw)
	}
	var l struct {
		Files []struct {
			ID      string `json:"id"`
			TypeKey string `json:"type_key"`
		} `json:"files"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files", &l)
	var file string
	for _, f := range l.Files {
		if f.TypeKey == "PROJECT_STATE" {
			file = f.ID
		}
	}
	path := filepath.Join(r.dir, ".overseer", "state", "PROJECT_STATE.md")
	if err := os.WriteFile(path, []byte("edited outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files/"+file+"/drift-status", &v)
	if v["status"] != "DRIFTED" || !strings.Contains(strings.Join(anyStrings(v["options"]), ","), "IMPORT") {
		t.Fatalf("drift: %v", v)
	}
	// Ignore → indicator IGNORED until the filesystem changes again.
	if code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/"+file+"/drift/ignore", map[string]any{"reason": "known"}); code != 200 {
		t.Fatalf("ignore: %d %s", code, raw)
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files/"+file+"/drift-status", &v)
	if v["ignored"] != true {
		t.Fatalf("not ignored: %v", v)
	}
	_ = os.WriteFile(path, []byte("edited again\n"), 0o644)
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files/"+file+"/drift-status", &v)
	if v["ignored"] == true {
		t.Fatal("new filesystem content still ignored")
	}
	// Import requires confirmation + reason.
	if code, _ := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/"+file+"/drift/import", map[string]any{}); code != 400 {
		t.Fatalf("unconfirmed import: %d", code)
	}
	if code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/"+file+"/drift/import",
		map[string]any{"confirm": true, "reason": "operator edit"}); code != 200 {
		t.Fatalf("import: %d %s", code, raw)
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files/"+file+"/drift-status", &v)
	if v["status"] != "SYNCHRONIZED" || v["current_version"] != float64(2) {
		t.Fatalf("after import: %v", v)
	}
	// Restore after another edit puts the managed version back.
	_ = os.WriteFile(path, []byte("third edit\n"), 0o644)
	if code, _ := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/"+file+"/drift/restore", nil); code != 200 {
		t.Fatal("restore")
	}
	if b, _ := os.ReadFile(path); string(b) != "edited again\n" {
		t.Fatalf("restored content %q", b)
	}
	// DERIVED: no import, no edit.
	r.exec(t, "UPDATE state_file_types SET source = 'DERIVED' WHERE key = 'PROJECT_STATE'")
	_ = os.WriteFile(path, []byte("derived edit\n"), 0o644)
	r.b.getJSON("/api/v1/projects/"+pid+"/state-files/"+file+"/drift-status", &v)
	if strings.Contains(strings.Join(anyStrings(v["options"]), ","), "IMPORT") {
		t.Fatalf("derived offers import: %v", v)
	}
	if code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/state-files/"+file+"/drift/import",
		map[string]any{"confirm": true, "reason": "x"}); code != 409 || !strings.Contains(string(raw), "DERIVED_NOT_IMPORTABLE") {
		t.Fatalf("derived import: %d %s", code, raw)
	}
	if code, _ := r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/state-files/"+file, map[string]any{"content": "x", "expected_version": 2}); code != 400 {
		t.Fatalf("derived edit accepted: %d", code)
	}
}

func anyStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// P17-48 .. P17-65 / P17-71 (WBS 17.4–17.114) — console read models over a
// real execution: dashboard, list, overview, phases, units, lineage,
// validation (ranges), checkpoints, repository, timeline, metrics, system.
func TestP1748ConsoleReadModels(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queue("complete")
	r.setCheck(t, "u1", 1) // first validation fails → FIX
	r.queue("complete")
	// Drive: plan, u1 dispatch, validation FAIL, FIX dispatch.
	r.run(t, 9)
	r.setCheck(t, "u1", 0)
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 120)

	var d readmodel.Dashboard
	r.b.getJSON("/api/v1/dashboard", &d)
	if d.ProjectsTotal < 1 || d.Completed < 1 {
		t.Fatalf("dashboard: %+v", d)
	}
	var l readmodel.ProjectList
	r.b.getJSON("/api/v1/console/projects?status=COMPLETED", &l)
	if len(l.Rows) != 1 || l.Rows[0].ID != pid || l.Rows[0].Progress.Percent != 100 {
		t.Fatalf("list: %+v", l)
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/console/projects?status=BOGUS", nil); code != 400 {
		t.Fatal("unknown status filter accepted")
	}
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "COMPLETED" || ov.Execution == nil || ov.Execution.Status != "COMPLETED" || len(ov.Actions) != 0 || ov.LatestEventSeq == 0 {
		t.Fatalf("overview: %+v", ov)
	}
	var phases struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases", &phases)
	if len(phases.Phases) != 1 || phases.Phases[0].Status != "DONE" || phases.Phases[0].Progress.Label != "2 / 2 tasks complete" {
		t.Fatalf("phases: %+v", phases)
	}
	ph := phases.Phases[0].ID
	var pd readmodel.PhaseDetail
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases/"+ph, &pd)
	if pd.KindCounts[0].Kind != "ATOMIC" || pd.KindCounts[0].Count != 2 || len(pd.Validations) == 0 {
		t.Fatalf("phase detail: %+v", pd)
	}
	var units readmodel.UnitPage
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases/"+ph+"/work-units?limit=1", &units)
	if units.Page.Total != 2 || len(units.Rows) != 1 || units.Rows[0].Key != "u1" || units.Rows[0].Validation != "PASS" || units.Rows[0].Checkpoint != "CREATED" {
		t.Fatalf("units: %+v", units)
	}
	var edges struct {
		Edges []readmodel.Edge `json:"edges"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/phases/"+ph+"/edges", &edges)
	if len(edges.Edges) != 1 || edges.Edges[0].FromKey != "u1" || edges.Edges[0].ToKey != "u2" {
		t.Fatalf("edges: %+v", edges)
	}
	var ud readmodel.UnitDetail
	r.b.getJSON("/api/v1/projects/"+pid+"/console/work-units/"+units.Rows[0].ID, &ud)
	if ud.Unit.Status != "DONE" || len(ud.Validations) != 2 || ud.Validations[0].Status != "PASS" || ud.Validations[1].Status != "FAIL" ||
		len(ud.FixAttempts) != 1 || ud.FixAttempts[0].Max != 3 || len(ud.Dispatches) != 2 || len(ud.Checkpoints) != 1 || ud.Report.Overseer != "DONE" {
		t.Fatalf("unit lineage: %+v", ud)
	}
	// Validation detail + output range never loads whole logs.
	var vd readmodel.ValidationDetail
	r.b.getJSON("/api/v1/projects/"+pid+"/console/validations/"+ud.Validations[1].ID, &vd)
	if vd.Run.Status != "FAIL" || len(vd.Commands) != 1 || !strings.Contains(vd.Commands[0].StderrHead, "failed") {
		t.Fatalf("validation detail: %+v", vd)
	}
	var out map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/validation-results/"+vd.Commands[0].ID+"/output?stream=stdout&offset=0&length=5", &out)
	if out["text"] != "check" || out["complete"] != false {
		t.Fatalf("range: %v", out)
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/projects/"+uuid.NewString()+"/validation-results/"+vd.Commands[0].ID+"/output", nil); code != 404 {
		t.Fatalf("cross-project output: %d", code)
	}
	var cps struct {
		Checkpoints []readmodel.Checkpoint `json:"checkpoints"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/checkpoints", &cps)
	if len(cps.Checkpoints) < 2 {
		t.Fatalf("checkpoints: %+v", cps)
	}
	var cp readmodel.Checkpoint
	r.b.getJSON("/api/v1/projects/"+pid+"/console/checkpoints/"+ud.Checkpoints[0].ID, &cp)
	if cp.Policy != "REQUIRED" || cp.Commit == "" || cp.ValidationStatus != "PASS" || !strings.Contains(cp.TargetLabel, "u1") {
		t.Fatalf("checkpoint: %+v", cp)
	}
	var repo readmodel.RepositoryStatus
	r.b.getJSON("/api/v1/projects/"+pid+"/console/repository", &repo)
	if !repo.Available || repo.Head == "" || repo.BaselineHead == "" || repo.LatestCheckpoint == nil {
		t.Fatalf("repository: %+v", repo)
	}
	var tl readmodel.Timeline
	r.b.getJSON("/api/v1/projects/"+pid+"/console/executions/"+r.execID.String()+"/timeline?category=checkpoint&limit=500", &tl)
	if len(tl.Rows) == 0 {
		t.Fatal("no checkpoint events")
	}
	for _, row := range tl.Rows {
		if row.Category != "checkpoint" {
			t.Fatalf("category filter leaked %s", row.Type)
		}
	}
	var m readmodel.Metrics
	r.b.getJSON("/api/v1/projects/"+pid+"/console/metrics", &m)
	var completed, fixes float64
	for _, x := range m.Execution {
		switch x.Name {
		case "Completed units":
			completed = x.Value
		case "Fix attempts":
			fixes = x.Value
		}
	}
	if completed != 2 || fixes != 1 || len(m.Phases) != 1 || m.Phases[0].Units != 2 {
		t.Fatalf("metrics: %+v", m)
	}
	var sys readmodel.SystemStatus
	r.b.getJSON("/api/v1/system/status", &sys)
	if sys.Database != "HEALTHY" || sys.Version == "" || len(sys.Providers) < 2 {
		t.Fatalf("system: %+v", sys)
	}
	// Worker panel never carries credentials.
	code, raw := r.b.api(http.MethodGet, "/api/v1/projects/"+pid+"/console/worker", nil)
	if code != 200 || strings.Contains(strings.ToLower(string(raw)), "token") && strings.Contains(string(raw), os.Getenv("P12_GATEWAY_TOKEN")) && os.Getenv("P12_GATEWAY_TOKEN") != "" {
		t.Fatalf("worker panel: %d %s", code, raw)
	}
	var disp struct {
		Dispatches []readmodel.DispatchRow `json:"dispatches"`
		Page       readmodel.Page          `json:"page"`
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/console/dispatches?limit=2", &disp)
	if len(disp.Dispatches) != 2 || disp.Page.Total < 3 {
		t.Fatalf("dispatches: %+v", disp)
	}
}
