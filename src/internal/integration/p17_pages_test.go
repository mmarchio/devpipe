package integration

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

var hrefRE = regexp.MustCompile(`href="(/[^"#]*)"`)

// crawl GETs every console page reachable from start (same project /
// global), asserting 200 + the shell, and returns the visited paths.
func crawl(t *testing.T, b *browser, start []string, limit int, project string) map[string]string {
	t.Helper()
	seen := map[string]string{}
	queue := append([]string(nil), start...)
	for len(queue) > 0 && len(seen) < limit {
		p := strings.ReplaceAll(queue[0], "&amp;", "&")
		queue = queue[1:]
		if _, ok := seen[p]; ok || strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/api/") {
			continue
		}
		// Other projects (e.g. the workflow-template system project) are
		// out of scope.
		if strings.HasPrefix(p, "/projects/") && !strings.HasPrefix(p, "/projects/"+project) && p != "/projects/new" {
			continue
		}
		code, body := b.page(p)
		if code != http.StatusOK {
			var order []string
			for k := range seen {
				order = append(order, k)
			}
			t.Fatalf("GET %s: %d after %d pages %v\n%s", p, code, len(seen), order, textOf(body))
		}
		if !strings.Contains(body, `<nav class="primary" aria-label="Primary">`) || !strings.Contains(body, "</html>") {
			t.Fatalf("GET %s: not a complete shell page\n%s", p, trimBody(body))
		}
		seen[p] = body
		for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
			queue = append(queue, m[1])
		}
	}
	return seen
}

// P17-73 / P17-123 / P17-177 (WBS 17.2, 17.3, 17.177) — every page renders
// through the shell with real data (in flight and completed), without
// client-side code, and every fragment renders.
func TestP17PagesRender(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}, p16Unit{key: "u2", deps: []string{"u1"}}))
	r.queue("complete")
	r.queue("complete")
	// Stop mid-way: after the first unit's DONE.
	r.run(t, 12)

	start := []string{"/", "/projects", "/projects/new", "/projects/" + pid, "/executions", "/workers", "/configuration", "/audit", "/system"}
	pages := crawl(t, r.b, start, 400, pid)
	t.Logf("crawled %d pages", len(pages))
	for _, must := range []string{"/projects/" + pid + "/phases", "/projects/" + pid + "/execution", "/projects/" + pid + "/worker",
		"/projects/" + pid + "/validations", "/projects/" + pid + "/roles", "/projects/" + pid + "/workflow",
		"/projects/" + pid + "/state-files", "/projects/" + pid + "/templates", "/projects/" + pid + "/prompts",
		"/projects/" + pid + "/config/worker", "/projects/" + pid + "/config/planning", "/projects/" + pid + "/readiness",
		"/projects/" + pid + "/metrics"} {
		if _, ok := pages[must]; !ok {
			t.Errorf("page %s not reachable by navigation", must)
		}
	}
	// No page relies on inline script (CSP script-src 'self').
	for p, body := range pages {
		if strings.Contains(body, "<script>") || strings.Contains(body, "onclick=") {
			t.Fatalf("%s carries inline script", p)
		}
	}
	// Fragments.
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	for _, f := range []string{"overview", "current-task", "actions", "flows", "units?phase=" + r.phases[1].String(),
		"timeline?execution=" + r.execID.String(), "recovery?execution=" + r.execID.String()} {
		r.b.mustPage("/projects/" + pid + "/fragments/" + f)
	}
	// Run to completion and crawl again (completed views).
	r.runUntil(t, execution.StepCompleted, 100)
	pages = crawl(t, r.b, start, 400, pid)
	t.Logf("crawled %d pages after completion", len(pages))
}
