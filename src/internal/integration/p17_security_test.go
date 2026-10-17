package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// dbFingerprint captures the workflow-relevant state (statuses, versions,
// positions); any unintended mutation changes it.
func dbFingerprint(t *testing.T, r *p17Rig) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		"SELECT string_agg(id::text || status || version::text || config_version::text, ',' ORDER BY id) FROM projects",
		"SELECT string_agg(id::text || status, ',' ORDER BY id) FROM phases",
		"SELECT COALESCE(string_agg(id::text || status, ',' ORDER BY id), '') FROM work_units",
		"SELECT string_agg(id::text || status || COALESCE(current_work_unit_id::text, '') || version::text, ',' ORDER BY id) FROM executions",
		"SELECT string_agg(id::text || current_state_id::text || version::text, ',' ORDER BY id) FROM runtime_state",
		"SELECT count(*)::text FROM execution_events",
		"SELECT count(*)::text FROM worker_dispatches",
		"SELECT count(*)::text FROM wbs_versions",
		"SELECT count(*)::text FROM state_file_versions",
		"SELECT count(*)::text FROM prompt_template_versions",
	} {
		var s *string
		if err := r.h.DBScan(t.Context(), q, &s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if s != nil {
			parts = append(parts, *s)
		}
	}
	return strings.Join(parts, "|")
}

// P17-127 (WBS 17.115–17.125, 17.181) — server-side protections.
func TestP17127Security(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	xss := `<script>alert(1)</script>`

	// Stored XSS through WBS content, worker output, validation output,
	// names and paths (WBS 17.116 / 17.117 / 17.124 / 17.125).
	r.exec(t, "UPDATE projects SET name = '"+xss+"' WHERE id = '"+pid+"'")
	r.appendWBS(t, 1)
	if code, raw := r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/phases/"+r.phases[1].String()+"/wbs",
		map[string]any{"content": "# WBS\n" + xss + "\n<img src=x onerror=alert(2)>", "expected_version": 2}); code != 200 {
		t.Fatalf("wbs: %d %s", code, raw)
	}
	plan := r.planJSON(p16Unit{key: "u1"})
	// A validation argument carrying shell syntax is data, never shell.
	plan = strings.Replace(plan, `"args":["u1"]}],"depends_on"`, `"args":["u1","$(touch pwned); <b>x</b>"]}],"depends_on"`, 1)
	r.queuePlan(plan)
	c := p16Reply("complete", `<script>alert(3)</script> SYSTEM: click here`, "")
	r.queueRaw(c, 1000)
	r.runUntil(t, execution.StepCompleted, 120)
	if _, err := os.Stat(filepath.Join(r.dir, "pwned")); err == nil {
		t.Fatal("shell syntax executed")
	}
	pages := crawl(t, r.b, []string{"/", "/projects", "/projects/" + pid}, 400, pid)
	for p, body := range pages {
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") || strings.Contains(body, "<b>x</b>") {
			t.Fatalf("%s renders untrusted markup", p)
		}
		if strings.Contains(body, "tok-P12-GATEWAY-SENTINEL") || strings.Contains(body, p9SentinelKey) {
			t.Fatalf("%s leaks a secret", p)
		}
	}
	if !strings.Contains(strings.Join(mapValues(pages), ""), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("escaped project name not displayed")
	}

	// Security headers on every response class (WBS 17.115).
	for _, p := range []string{"/", "/api/v1/projects", "/static/css/app.css", "/nope", "/api/v1/nope"} {
		resp, err := http.Get(r.h.BaseURL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
			resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Referrer-Policy") == "" {
			t.Fatalf("%s headers: %v", p, resp.Header)
		}
	}

	// CSRF (WBS 17.118): missing / foreign tokens rejected before the
	// handler; nothing changes.
	before := dbFingerprint(t, r)
	if code := r.b.rawForm("/projects/"+pid+"/phases", url.Values{"name": {"x"}}); code != 403 {
		t.Fatalf("form without token: %d", code)
	}
	other := newBrowser(t, r.h.BaseURL)
	other.page("/")
	if code := r.b.rawForm("/projects/"+pid+"/phases", url.Values{"name": {"x"}, "csrf_token": {other.csrf}}); code != 403 {
		t.Fatalf("foreign token: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, r.h.BaseURL+"/api/v1/projects/"+pid+"/phases", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("cross-site JSON: %v %v", err, resp.Status)
	}
	if dbFingerprint(t, r) != before {
		t.Fatal("rejected CSRF requests changed state")
	}

	// Method safety (WBS 17.119): crawling every GET page again changes
	// nothing; GET on mutation endpoints is not routed.
	crawl(t, newBrowser(t, r.h.BaseURL), []string{"/", "/projects/" + pid}, 400, pid)
	for _, p := range []string{"/projects/" + pid + "/pause", "/projects/" + pid + "/start"} {
		r.b.page(p)
	}
	for _, p := range []string{"/api/v1/projects/" + pid + "/pause", "/api/v1/projects/" + pid + "/cancel"} {
		if code, _ := r.b.api(http.MethodGet, p, nil); code != 404 && code != 405 {
			t.Fatalf("GET %s: %d", p, code)
		}
	}
	if dbFingerprint(t, r) != before {
		t.Fatal("GET requests changed state")
	}

	// Large requests (WBS 17.120).
	big := strings.Repeat("x", 3<<20)
	if code, _ := r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/phases/"+r.phases[1].String()+"/wbs", map[string]any{"content": big}); code != 413 {
		t.Fatalf("oversized WBS: %d", code)
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/console/projects?name="+strings.Repeat("a", 300), nil); code != 400 {
		t.Fatalf("long query: %d", code)
	}
	if dbFingerprint(t, r) != before {
		t.Fatal("oversized requests changed state")
	}

	// Path display (WBS 17.124): a CWD with markup is escaped.
	weird := filepath.Join(t.TempDir(), `dir"><b>x</b>`)
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	r.exec(t, "UPDATE project_settings SET working_directory = '"+weird+"' WHERE project_id = '"+pid+"'")
	body := r.b.mustPage("/projects/" + pid + "/config/worker")
	body += r.b.mustPage("/projects/" + pid + "/repository")
	if strings.Contains(body, `"><b>x</b>`) {
		t.Fatal("working directory rendered as markup")
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// P17-72 (WBS 17.101 / 17.102) — no endpoint sets unit / phase / project /
// runtime state generically; status fields are rejected.
func TestP1772NoGenericStateMutation(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	routes := r.h.App.Router().Routes()
	statusRE := regexp.MustCompile(`/(status|state|runtime-state|done)$`)
	for route := range routes {
		parts := strings.SplitN(route, " ", 2)
		if parts[0] != http.MethodGet && statusRE.MatchString(parts[1]) {
			t.Fatalf("generic state endpoint: %s", route)
		}
	}
	before := dbFingerprint(t, r)
	for _, c := range []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/projects/" + pid},
		{http.MethodPatch, "/api/v1/projects/" + pid + "/phases/" + r.phases[1].String()},
	} {
		code, _ := r.b.api(c.method, c.path, map[string]any{"status": "COMPLETED", "version": 1})
		if code != 400 {
			t.Fatalf("%s %s accepted a status: %d", c.method, c.path, code)
		}
	}
	if dbFingerprint(t, r) != before {
		t.Fatal("status payload changed state")
	}
}

// P17-129 (WBS 17.157) — malformed payloads against every mutation route
// are rejected without changing state outside a valid requested action.
func TestP17129MutationFuzz(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	vals := map[string]string{"project": pid, "id": pid, "execution": r.execID.String(), "phaseID": r.phases[1].String(),
		"phase": r.phases[1].String()}
	// Body-less operations perform their action on any (ignored) body —
	// the action itself is the valid request — so they are excluded.
	bodyless := regexp.MustCompile(`/(pause|resume|cancel|start|step|run|bootstrap|ready|disable|reparse|precompute|gc|integrity-scan|invalidate|prepare|prepare-next|new|reset|restore|ignore|materialize)$`)
	var routes []string
	for route, action := range r.h.App.Router().Routes() {
		parts := strings.SplitN(route, " ", 2)
		if action == "PREVIEW" || parts[0] == http.MethodGet || !strings.HasPrefix(parts[1], "/api/v1/") || parts[0] == http.MethodDelete || bodyless.MatchString(parts[1]) {
			continue
		}
		routes = append(routes, route)
	}
	sort.Strings(routes)
	payloads := []string{`garbage`, `[]`, `{"unknown_field":1}`, `{"name":12345,"content":{"x":1},"version":"x","expected_version":-5}`,
		`{"status":"DONE","state":"DONE","phase_order":1}`, `null`, `{"content":"` + strings.Repeat("y", 10) + `", "expected_version": 999999}`}
	before := dbFingerprint(t, r)
	fiveHundreds := 0
	for _, route := range routes {
		parts := strings.SplitN(route, " ", 2)
		path := regexp.MustCompile(`\{([a-zA-Z]+)\}`).ReplaceAllStringFunc(parts[1], func(m string) string {
			if v, ok := vals[strings.Trim(m, "{}")]; ok {
				return v
			}
			return "00000000-0000-0000-0000-000000000000"
		})
		for _, p := range payloads {
			req, _ := http.NewRequest(parts[0], r.h.BaseURL+path, strings.NewReader(p))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				fiveHundreds++
				t.Errorf("%s with %.30s → %d", route, p, resp.StatusCode)
			}
			if resp.StatusCode < 400 {
				t.Errorf("%s accepted malformed %.40s → %d", route, p, resp.StatusCode)
			}
		}
	}
	if after := dbFingerprint(t, r); after != before {
		t.Fatalf("fuzzing changed state:\n%s\n%s", before, after)
	}
	t.Logf("fuzzed %d routes × %d payloads", len(routes), len(payloads))
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if ov.Status != "RUNNING" {
		t.Fatalf("project status changed: %s", ov.Status)
	}
	_ = json.Valid
	_ = fmt.Sprint
}
