package integration

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
)

// p18FirstID returns the first id of a query ("" when none).
func p18FirstID(t *testing.T, h *Harness, sql string) string {
	t.Helper()
	var s string
	_ = h.DBScan(context.Background(), sql, &s)
	return s
}

// T18.96 / T18.165 / T18.89 — with maximum-size artifacts seeded (1 MiB
// WBS, near-limit template and role prompt, multi-MB validation output and
// worker response), every GET API route answers without a server error and
// with a bounded response; oversized or malformed edits are rejected as
// invalid input, never 500.
func TestT1896LargeArtifactsBounded(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18ReplaceCheck(t, r.p16Rig, "head -c 3000000 /dev/zero | tr '\\0' v; echo; exit 0")
	r.runUntil(t, execution.StepCompleted, 100)

	// Edits at and beyond the limits.
	wbs := "# Huge phase\n" + strings.Repeat("- requirement line that is long enough to fill the document\n", (1<<20)/62-10)
	// A second, not yet executed project carries the 1 MiB WBS.
	r2 := p15Build(t, r.h, r.g, r.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})
	pid2 := r2.pid.String()
	ph := struct{ ID string }{r2.phases[1].String()}
	wbsPath := "/api/v1/projects/" + pid2 + "/phases/" + ph.ID + "/wbs"
	// Three near-limit versions: an unbounded version list would carry
	// ~3 MiB of content.
	for v := 1; v <= 3; v++ {
		if code, raw := r.b.api(http.MethodPut, wbsPath, map[string]any{"content": wbs + strings.Repeat("#", v), "expected_version": v}); code >= 300 {
			t.Fatalf("near-limit WBS v%d rejected: %d %.200s", v+1, code, raw)
		}
	}
	for _, c := range []struct {
		name    string
		content string
	}{
		{"oversized WBS", strings.Repeat("x", (1<<20)+10)},
		{"NUL byte in WBS", "bad \x00 byte"},
	} {
		if code, raw := r.b.api(http.MethodPut, wbsPath, map[string]any{"content": c.content, "expected_version": 4}); code < 400 || code >= 500 {
			t.Fatalf("%s: %d %.200s", c.name, code, raw)
		}
	}
	tmpl := p18FirstID(t, r.h, "SELECT id::text FROM prompt_templates WHERE project_id = '"+pid+"' LIMIT 1")
	if tmpl != "" {
		var cur map[string]any
		r.b.getJSON("/api/v1/projects/"+pid+"/prompt-templates/"+tmpl, &cur)
		code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/prompt-templates/"+tmpl+"/versions",
			map[string]any{"content": strings.Repeat("t", 300<<10), "expected_version": cur["current_version"]})
		if code < 400 || code >= 500 {
			t.Fatalf("oversized template: %d %.200s", code, raw)
		}
	}
	// A multi-MB raw worker response (bypassing the immutability guard).
	r.exec(t, "SET session_replication_role = replica; UPDATE worker_responses SET content = repeat('w', 3000000) WHERE dispatch_id = (SELECT id FROM worker_dispatches WHERE project_id = '"+
		pid+"' AND work_unit_id IS NOT NULL LIMIT 1)")

	vals := map[string]string{
		"project": pid, "id": pid, "execution": r.execID.String(), "phase": r.phases[1].String(), "phaseID": r.phases[1].String(),
		"unit":          p18FirstID(t, r.h, "SELECT id::text FROM work_units WHERE project_id = '"+pid+"' LIMIT 1"),
		"dispatch":      p18FirstID(t, r.h, "SELECT id::text FROM worker_dispatches WHERE project_id = '"+pid+"' AND work_unit_id IS NOT NULL LIMIT 1"),
		"result":        p18FirstID(t, r.h, "SELECT id::text FROM validation_command_results r JOIN validation_runs v ON v.id = r.run_id WHERE v.project_id = '"+pid+"' LIMIT 1"),
		"run":           p18FirstID(t, r.h, "SELECT id::text FROM validation_runs WHERE project_id = '"+pid+"' LIMIT 1"),
		"checkpoint":    p18FirstID(t, r.h, "SELECT id::text FROM repository_checkpoints WHERE project_id = '"+pid+"' LIMIT 1"),
		"entry":         p18FirstID(t, r.h, "SELECT id::text FROM prompt_cache_entries WHERE project_id = '"+pid+"' LIMIT 1"),
		"event":         p18FirstID(t, r.h, "SELECT id::text FROM execution_events WHERE project_id = '"+pid+"' LIMIT 1"),
		"file":          p18FirstID(t, r.h, "SELECT id::text FROM state_files WHERE project_id = '"+pid+"' LIMIT 1"),
		"prompt":        p18FirstID(t, r.h, "SELECT id::text FROM worker_prompts WHERE project_id = '"+pid+"' LIMIT 1"),
		"role":          p18FirstID(t, r.h, "SELECT id::text FROM roles WHERE project_id = '"+pid+"' LIMIT 1"),
		"roleID":        p18FirstID(t, r.h, "SELECT id::text FROM roles WHERE project_id = '"+pid+"' LIMIT 1"),
		"session":       p18FirstID(t, r.h, "SELECT id::text FROM worker_sessions WHERE project_id = '"+pid+"' LIMIT 1"),
		"snapshot":      p18FirstID(t, r.h, "SELECT id::text FROM context_snapshots WHERE project_id = '"+pid+"' LIMIT 1"),
		"template":      tmpl,
		"job":           p18FirstID(t, r.h, "SELECT id::text FROM precompute_jobs WHERE project_id = '"+pid+"' LIMIT 1"),
		"key":           p18FirstID(t, r.h, "SELECT cache_key FROM prompt_cache_entries WHERE project_id = '"+pid+"' LIMIT 1"),
		"instructionID": p18FirstID(t, r.h, "SELECT id::text FROM instructions LIMIT 1"),
		"version":       "1", "attempt": "1", "worker": "hermes", "continuation": "00000000-0000-0000-0000-000000000000",
	}
	const bound = 3 << 19 // 1.5 MiB: the largest artifact (1 MiB WBS) plus JSON overhead
	type sized struct {
		route string
		n     int
	}
	var big []sized
	routes := 0
	crawl := func(vals map[string]string) {
		for route := range r.h.App.Router().Routes() {
			if !strings.HasPrefix(route, "GET /api/v1/") || strings.HasSuffix(route, "/stream") { // SSE: P18-82
				continue
			}
			routes++
			path := regexp.MustCompile(`\{([a-zA-Z]+)\}`).ReplaceAllStringFunc(strings.TrimPrefix(route, "GET "), func(m string) string {
				if v := vals[strings.Trim(m, "{}")]; v != "" {
					return v
				}
				return "00000000-0000-0000-0000-000000000000"
			})
			start := time.Now()
			code, body := r.b.api(http.MethodGet, path, nil)
			if d := time.Since(start); d > 2*time.Second {
				t.Logf("slow: %s %s", route, d)
			}
			if code >= 500 {
				t.Errorf("%s → %d", path, code)
			}
			if len(body) > 256<<10 {
				big = append(big, sized{route, len(body)})
			}
			if len(body) > bound {
				t.Errorf("%s returned %d bytes (bound %d)", path, len(body), bound)
			}
		}
	}
	crawl(vals)
	vals2 := map[string]string{"project": pid2, "id": pid2, "phase": ph.ID, "phaseID": ph.ID, "version": "1", "attempt": "1"}
	crawl(vals2)
	sort.Slice(big, func(i, j int) bool { return big[i].n > big[j].n })
	for _, b := range big {
		t.Logf("large response: %s %d bytes", b.route, b.n)
	}
	t.Logf("checked %d GET routes", routes)
}
