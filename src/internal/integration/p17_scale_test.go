package integration

import (
	"context"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/readmodel"
	"overseer/internal/service"
)

// P17-130 / P17-143 (WBS 17.150–17.153, 17.158–17.160, 17.196) — read
// models keep a constant query count as data grows; a project with 50
// phases, 5,000 units, 100,000 events and large logs stays navigable: pages
// and timelines are paginated, logs lazily ranged, the stream responsive.
func TestP17130LargeProject(t *testing.T) {
	r := p17NewRig(t)
	ctx := context.Background()
	pid, eid := r.pid.String(), r.execID.String()
	console := r.h.App.Console().Console
	plan := r.plan(t, 1, 1, "EXECUTING")
	r.unit(t, plan, 1, "seed", "DONE")

	measure := func() (overview, dash, list, units, timeline int64) {
		overview = queries(func(c context.Context) { _, _ = console.Overview(c, r.pid) })
		dash = queries(func(c context.Context) { _, _ = console.Dashboard(c) })
		list = queries(func(c context.Context) { _, _ = console.Projects(c, service.ProjectQuery{Limit: 25}) })
		units = queries(func(c context.Context) { _, _ = console.Units(c, r.pid, r.phases[1], service.UnitQuery{Limit: 50}) })
		timeline = queries(func(c context.Context) {
			_, _ = console.Timeline(c, r.pid, r.execID, service.TimelineQuery{Limit: 100})
		})
		return
	}
	o1, d1, l1, u1, t1 := measure()

	// Grow everything.
	start := time.Now()
	r.exec(t, "INSERT INTO projects (id, name, status) SELECT gen_random_uuid(), 'bulk ' || g, 'DRAFT' FROM generate_series(1, 49) g")
	r.exec(t, "INSERT INTO phases (id, project_id, phase_order, name, status) SELECT gen_random_uuid(), '"+pid+"', g, 'Phase ' || g, 'PENDING' FROM generate_series(2, 50) g")
	r.exec(t, "INSERT INTO work_units (id, execution_plan_id, project_id, sequence, name, kind, status, objective, unit_key) "+
		"SELECT gen_random_uuid(), '"+plan.String()+"', '"+pid+"', g, 'Unit ' || g, 'ATOMIC', CASE WHEN g % 3 = 0 THEN 'DONE' ELSE 'PENDING' END, 'o', 'K-' || g "+
		"FROM generate_series(2, 5000) g")
	var maxSeq, maxExec int64
	_ = r.h.DBScan(ctx, "SELECT COALESCE(max(project_seq), 0) FROM execution_events WHERE project_id = '"+pid+"'", &maxSeq)
	_ = r.h.DBScan(ctx, "SELECT COALESCE(max(exec_seq), 0) FROM execution_events WHERE execution_id = '"+eid+"'", &maxExec)
	r.exec(t, "INSERT INTO execution_events (id, project_id, execution_id, type, payload, created_at, exec_seq, project_seq) "+
		"SELECT gen_random_uuid(), '"+pid+"', '"+eid+"', 'EXECUTION_WAITING', '{}', now(), "+strconv.FormatInt(maxExec, 10)+" + g, "+
		strconv.FormatInt(maxSeq, 10)+" + g FROM generate_series(1, 100000) g")
	run, res := uuid.New(), uuid.New()
	r.exec(t, "INSERT INTO validation_runs (id, project_id, execution_id, scope, attempt, status, cwd, started_at, finished_at) VALUES ('"+
		run.String()+"', '"+pid+"', '"+eid+"', 'UNIT', 1, 'FAIL', '/tmp', now(), now())")
	r.exec(t, "INSERT INTO validation_command_results (id, run_id, ord, program, cwd, exit_code, stdout, stderr, duration_ms, timeout_ms, outcome, created_at) VALUES ('"+
		res.String()+"', '"+run.String()+"', 1, 'go', '/tmp', 1, repeat('L', 10485760), repeat('E', 10485760), 5, 1000, 'FAIL', now())")
	r.exec(t, "ANALYZE")
	t.Logf("seeded in %s", time.Since(start))

	o2, d2, l2, u2, t2 := measure()
	if o1 != o2 || d1 != d2 || l1 != l2 || u1 != u2 || t1 != t2 {
		t.Fatalf("query counts grew: overview %d→%d dashboard %d→%d list %d→%d units %d→%d timeline %d→%d", o1, o2, d1, d2, l1, l2, u1, u2, t1, t2)
	}
	if o2 > 6 || d2 > 3 || l2 > 2 || u2 > 6 || t2 > 3 {
		t.Fatalf("query counts not bounded: overview %d dashboard %d list %d units %d timeline %d", o2, d2, l2, u2, t2)
	}
	t.Logf("queries: overview %d dashboard %d list %d units %d timeline %d", o2, d2, l2, u2, t2)

	// Current status never scans the event history (WBS 17.160).
	var plan1 string
	_ = r.h.DBScan(ctx, "EXPLAIN SELECT COALESCE(MAX(project_seq), 0) FROM execution_events WHERE project_id = '"+pid+"'", &plan1)
	if strings.Contains(plan1, "Seq Scan") {
		t.Fatalf("latest sequence scans events: %s", plan1)
	}

	// Pages stay bounded.
	t0 := time.Now()
	body := r.b.mustPage("/projects/" + pid)
	if len(body) > 200<<10 || strings.Contains(body, "LLLLLLLLLL") {
		t.Fatalf("overview page size %d", len(body))
	}
	body = r.b.mustPage("/projects/" + pid + "/phases/" + r.phases[1].String() + "/work-units")
	if n := strings.Count(body, `<a href="/projects/`+pid+`/work-units/`); n != 50 || !strings.Contains(body, "of 5000") {
		t.Fatalf("units page rows %d", n)
	}
	// Page 2: the live refresh reloads the same page, not the first one.
	unitLinks := regexp.MustCompile(`<a href="/projects/[^"]+/work-units/[^"]+"`)
	page2 := r.b.mustPage("/projects/" + pid + "/phases/" + r.phases[1].String() + "/work-units?offset=50&limit=50")
	m := regexp.MustCompile(`data-fragment-url="(/projects/[^"]+/fragments/units\?[^"]+)"`).FindStringSubmatch(page2)
	if m == nil || !strings.Contains(html.UnescapeString(m[1]), "offset=50") {
		t.Fatalf("units fragment URL %q", m)
	}
	rows2, refreshed := unitLinks.FindAllString(page2, -1), unitLinks.FindAllString(r.b.mustPage(html.UnescapeString(m[1])), -1)
	if len(rows2) != 50 || strings.Join(refreshed, "") != strings.Join(rows2, "") || strings.Contains(body, rows2[0]) {
		t.Fatalf("page 2 rows %d, refreshed %d (same as page 2: %t)", len(rows2), len(refreshed), strings.Join(refreshed, "") == strings.Join(rows2, ""))
	}
	body = r.b.mustPage("/projects/" + pid + "/phases")
	if strings.Count(body, "<tr") > 60 {
		t.Fatal("phase list unexpectedly large")
	}
	var tl readmodel.Timeline
	r.b.getJSON("/api/v1/projects/"+pid+"/console/executions/"+eid+"/timeline?limit=100", &tl)
	if len(tl.Rows) != 100 || tl.NextBefore == 0 {
		t.Fatalf("timeline page: %d", len(tl.Rows))
	}
	var older readmodel.Timeline
	r.b.getJSON("/api/v1/projects/"+pid+"/console/executions/"+eid+"/timeline?limit=100&before="+strconv.FormatInt(tl.NextBefore, 10), &older)
	if len(older.Rows) != 100 || older.Rows[0].Seq != tl.NextBefore-1 {
		t.Fatal("timeline keyset")
	}
	// Large logs: heads only, ranged on demand.
	var vd readmodel.ValidationDetail
	r.b.getJSON("/api/v1/projects/"+pid+"/console/validations/"+run.String(), &vd)
	if len(vd.Commands[0].StdoutHead) != service.HeadBytes || vd.Commands[0].StdoutLen != 10485760 || !vd.Commands[0].Truncated {
		t.Fatalf("log head: %d / %d", len(vd.Commands[0].StdoutHead), vd.Commands[0].StdoutLen)
	}
	body = r.b.mustPage("/projects/" + pid + "/validations/" + run.String())
	if len(body) > 100<<10 {
		t.Fatalf("validation page %d bytes", len(body))
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/projects/"+pid+"/validation-results/"+res.String()+"/output?stream=stderr&offset=5000000&length=65536", nil); code != 200 {
		t.Fatal("range read")
	}
	t.Logf("page checks in %s", time.Since(t0))

	// The stream stays responsive with 100k events of history.
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", new(readmodel.Overview))
	var latest int64
	_ = r.h.DBScan(ctx, "SELECT max(project_seq) FROM execution_events WHERE project_id = '"+pid+"'", &latest)
	ch, cancel, code := r.b.stream("/api/v1/projects/"+pid+"/events/stream", strconv.FormatInt(latest, 10))
	defer cancel()
	if code != 200 {
		t.Fatal(code)
	}
	r.exec(t, "INSERT INTO execution_events (id, project_id, execution_id, type, payload, created_at, exec_seq, project_seq) VALUES (gen_random_uuid(), '"+
		pid+"', '"+eid+"', 'EXECUTION_RESUMED', '{}', now(), "+strconv.FormatInt(maxExec+100001, 10)+", "+strconv.FormatInt(latest+1, 10)+")")
	t1x := time.Now()
	if got := collect(ch, 1, 5*time.Second); len(got) != 1 || got[0].ID != strconv.FormatInt(latest+1, 10) {
		t.Fatalf("stream after large history: %v", got)
	}
	t.Logf("stream latency %s", time.Since(t1x))
}
