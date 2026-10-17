package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P17-23 / P17-27 (WBS 17.103–17.106) — audit events persist, filter and
// page server-side, and are immutable.
func TestP1727AuditRepository(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	repo := dbrepo.NewAuditRepository(h.App.DB().Raw())
	p1, p2 := uuid.New(), uuid.New()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 7; i++ {
		pid := p1
		action := access.ActionLifecyclePause
		if i%2 == 1 {
			pid, action = p2, access.ActionWBSEdit
		}
		e := &audit.Event{At: base.Add(time.Duration(i) * time.Minute), ProjectID: &pid, ProjectName: "P",
			Actor: audit.Actor{ID: "local-admin", Name: "Admin", Type: access.PrincipalLocalAdmin}, Action: action,
			ResourceKind: "project", ResourceID: pid.String(), Result: audit.ResultSucceeded, HTTPStatus: 200, Reason: "because",
			RequestID: "req-" + itoa(i), Method: "POST", Path: "/x", Metadata: map[string]any{"k": "v"}}
		if err := repo.AppendAudit(ctx, nil, e); err != nil {
			t.Fatal(err)
		}
		if e.Seq == 0 {
			t.Fatal("seq not assigned")
		}
	}
	f := audit.Filter{ProjectID: &p1, Limit: 2}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	page, err := repo.ListAudit(ctx, f)
	if err != nil || len(page) != 2 || page[0].Seq <= page[1].Seq || page[0].RequestID != "req-6" {
		t.Fatalf("page 1: %v %+v", err, page)
	}
	f.BeforeSeq = page[1].Seq
	page2, _ := repo.ListAudit(ctx, f)
	if len(page2) != 2 || page2[0].RequestID != "req-2" {
		t.Fatalf("page 2: %+v", page2)
	}
	all, _ := repo.ListAudit(ctx, audit.Filter{Action: access.ActionWBSEdit, Limit: 50})
	if len(all) != 3 {
		t.Fatalf("action filter: %d", len(all))
	}
	from := base.Add(150 * time.Second)
	ranged, _ := repo.ListAudit(ctx, audit.Filter{From: &from, Limit: 50})
	if len(ranged) != 4 {
		t.Fatalf("date filter: %d", len(ranged))
	}
	got, err := repo.GetAudit(ctx, page[0].ID)
	if err != nil || got.Actor.Type != access.PrincipalLocalAdmin || got.Metadata["k"] != "v" || got.Reason != "because" {
		t.Fatalf("get: %v %+v", err, got)
	}
	for _, q := range []string{"UPDATE audit_events SET reason = 'x'", "DELETE FROM audit_events"} {
		if _, err := h.DBExec(ctx, q); err == nil {
			t.Fatalf("%s accepted", q)
		}
	}
	if _, err := repo.GetAudit(ctx, uuid.New()); err == nil {
		t.Fatal("missing event found")
	}
}

// P17-24 / P17-28 / P17-29 (WBS 17.76) — concurrent appends get a unique,
// gapless per-project sequence; resume lists strictly after a sequence.
func TestP1728ProjectSequence(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	execRepo := dbrepo.NewExecutionRepository(raw)
	web := dbrepo.NewWebReadRepository(raw)
	start, err := web.MaxProjectSeq(ctx, r.pid)
	if err != nil || start == 0 {
		t.Fatalf("bootstrap events must have project_seq: %d %v", start, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := &execution.Event{ProjectID: r.pid, Type: execution.EventBootstrapRequested, Payload: map[string]any{"i": i}}
			if i%2 == 0 {
				id := r.execID
				e.ExecutionID = &id
				e.Type = execution.EventExecutionWaiting
			}
			if err := execRepo.AppendEvent(ctx, nil, e); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	evs, err := web.ListProjectEventsAfter(ctx, r.pid, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		if e.ProjectSeq != int64(i+1) {
			t.Fatalf("project_seq not gapless at %d: %d", i, e.ProjectSeq)
		}
	}
	if int64(len(evs)) != start+40 {
		t.Fatalf("events %d want %d", len(evs), start+40)
	}
	after, _ := web.ListProjectEventsAfter(ctx, r.pid, start+35, 1000)
	if len(after) != 5 || after[0].ProjectSeq != start+36 {
		t.Fatalf("resume: %d", len(after))
	}
	// Another project's events never appear.
	other, _ := web.ListProjectEventsAfter(ctx, uuid.New(), 0, 100)
	if len(other) != 0 {
		t.Fatal("cross-project events")
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE project_seq IS NULL"); n != 0 {
		t.Fatalf("%d events without project_seq", n)
	}
	if _, err := r.h.DBExec(ctx, "INSERT INTO execution_events (id, project_id, type, created_at, project_seq) VALUES (gen_random_uuid(), '"+
		r.pid.String()+"', 'EXECUTION_STARTED', now(), 1)"); err == nil {
		t.Fatal("duplicate project_seq accepted")
	}
}

// P17-30 / P17-31 / P17-37 (WBS 17.4–17.7, 17.109, 17.159) — dashboard,
// list page and scheduler counts come from bounded queries over persisted
// rows.
func TestP1730DashboardAndProjectList(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	web := dbrepo.NewWebReadRepository(r.h.App.DB().Raw())
	raw := r.h.App.DB().Raw()
	ps := service.NewProjectService(dbrepo.NewProjectRepository(raw), dbrepo.NewSettingsRepository(raw), raw)
	for i := 0; i < 5; i++ {
		if _, _, err := ps.CreateProject(ctx, "list-"+itoa(i)+"-alpha_%", ""); err != nil {
			t.Fatal(err)
		}
	}
	c, err := web.DashboardCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectsTotal < 6 || c.ProjectsByStatus["RUNNING"] < 1 || c.ActiveExecutions < 1 {
		t.Fatalf("dashboard: %+v", c)
	}
	rows, total, err := web.ProjectListPage(ctx, persistence.ProjectPageFilter{Name: "ALPHA_%", Limit: 2})
	if err != nil || total != 5 || len(rows) != 2 {
		t.Fatalf("name filter: %v total %d rows %d", err, total, len(rows))
	}
	if rows, _, _ := web.ProjectListPage(ctx, persistence.ProjectPageFilter{Name: "alpha_x"}); len(rows) != 0 {
		t.Fatal("LIKE metacharacters not escaped")
	}
	rows, total, _ = web.ProjectListPage(ctx, persistence.ProjectPageFilter{Name: "alpha", Limit: 2, Offset: 10})
	if len(rows) != 0 || total != 5 {
		t.Fatalf("past end: %d %d", len(rows), total)
	}
	yes := true
	act, _, err := web.ProjectListPage(ctx, persistence.ProjectPageFilter{Active: &yes, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var mine *persistence.ProjectListRow
	for i := range act {
		if act[i].ID == r.pid {
			mine = &act[i]
		}
	}
	if mine == nil || mine.Status != "RUNNING" || mine.PhaseOrder != 1 || mine.StateKey == "" || mine.ExecutionStatus == "" {
		t.Fatalf("active row: %+v", mine)
	}
	no := false
	inactive, _, _ := web.ProjectListPage(ctx, persistence.ProjectPageFilter{Active: &no, Limit: 200})
	for _, x := range inactive {
		if x.ID == r.pid {
			t.Fatal("active project listed as inactive")
		}
	}
	r.exec(t, "UPDATE executions SET status = 'RECOVERY_REQUIRED', recovery_reason = 'x' WHERE id = '"+r.execID.String()+"'")
	rec, _, _ := web.ProjectListPage(ctx, persistence.ProjectPageFilter{Recovery: true, Limit: 50})
	if len(rec) != 1 || rec[0].ID != r.pid || !rec[0].RecoveryRequired {
		t.Fatalf("recovery filter: %+v", rec)
	}
	sc, err := web.SchedulerCounts(ctx)
	if err != nil || sc.Runnable < 0 {
		t.Fatalf("scheduler counts: %v", err)
	}
}

// P17-32 (WBS 17.10) — overview row: execution, current phase / plan /
// unit / runtime.
func TestP1732ProjectOverviewRow(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	web := dbrepo.NewWebReadRepository(r.h.App.DB().Raw())
	plan := r.plan(t, 1, 1, "EXECUTING")
	u1 := r.unit(t, plan, 1, "one", "DONE")
	u2 := r.unit(t, plan, 2, "two", "EXECUTING")
	r.unit(t, plan, 3, "three", "PENDING")
	_ = u1
	r.exec(t, "UPDATE executions SET current_work_unit_id = '"+u2.String()+"', current_phase_id = '"+r.phases[1].String()+"' WHERE id = '"+r.execID.String()+"'")
	x, err := web.ProjectOverview(ctx, r.pid)
	if err != nil {
		t.Fatal(err)
	}
	if x.ExecutionID == nil || *x.ExecutionID != r.execID || !x.ExecutionActive || x.PhaseOrder != 1 || x.PlanID == nil || *x.PlanID != plan ||
		x.UnitsDone != 1 || x.UnitsTotal != 3 || x.UnitID == nil || *x.UnitID != u2 || x.StateKey == "" || x.PhasesTotal != 1 || x.WBSVersion < 1 {
		t.Fatalf("overview: %+v", x)
	}
	if _, err := web.ProjectOverview(ctx, uuid.New()); err == nil {
		t.Fatal("missing project found")
	}
}

// P17-33 / P17-34 (WBS 17.24–17.29) — paginated units with server-side
// readiness, kind counts and exact edges.
func TestP1733WorkUnitPageAndEdges(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	web := dbrepo.NewWebReadRepository(r.h.App.DB().Raw())
	plan := r.plan(t, 1, 1, "EXECUTING")
	a := r.unit(t, plan, 1, "a", "DONE")
	b := r.unit(t, plan, 2, "b", "READY")
	c := r.unit(t, plan, 3, "c", "PENDING")
	d := r.unit(t, plan, 4, "d", "PENDING")
	r.exec(t, "UPDATE work_units SET kind = 'COMPOSITION' WHERE id = '"+c.String()+"'")
	r.exec(t, "UPDATE work_units SET unit_key = 'U-'||sequence WHERE execution_plan_id = '"+plan.String()+"'")
	r.depend(t, c, a)
	r.depend(t, c, b)
	r.depend(t, d, c)
	rows, total, err := web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan, Limit: 2})
	if err != nil || total != 4 || len(rows) != 2 || rows[0].Key != "U-1" {
		t.Fatalf("page: %v %d %+v", err, total, rows)
	}
	all, _, _ := web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan})
	byKey := map[string]persistence.WorkUnitRow{}
	for _, x := range all {
		byKey[x.Key] = x
	}
	if strings.Join(byKey["U-3"].Deps, ",") != "U-1,U-2" || strings.Join(byKey["U-3"].Blocking, ",") != "U-2" {
		t.Fatalf("deps / blocking: %+v", byKey["U-3"])
	}
	ready, _, _ := web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan, Readiness: "READY"})
	if len(ready) != 1 || ready[0].Key != "U-2" {
		t.Fatalf("ready: %+v", ready)
	}
	blocked, _, _ := web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan, Readiness: "BLOCKED"})
	if len(blocked) != 2 {
		t.Fatalf("blocked: %+v", blocked)
	}
	comp, _, _ := web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan, Kind: "COMPOSITION"})
	if len(comp) != 1 {
		t.Fatal("kind filter")
	}
	before := r.count(t, "SELECT count(*) FROM work_units WHERE status = 'DONE'")
	_, _, _ = web.WorkUnitPage(ctx, persistence.WorkUnitPageFilter{PlanID: plan, Status: "DONE"})
	if r.count(t, "SELECT count(*) FROM work_units WHERE status = 'DONE'") != before {
		t.Fatal("filter changed state")
	}
	kinds, _ := web.PlanKindCounts(ctx, plan)
	if kinds["ATOMIC"] != 3 || kinds["COMPOSITION"] != 1 {
		t.Fatalf("kinds: %v", kinds)
	}
	edges, n, _ := web.PlanEdges(ctx, plan, 10, 0)
	if n != 3 || len(edges) != 3 || edges[0].ToID != c || edges[2].FromID != c || edges[2].ToID != d {
		t.Fatalf("edges: %d %+v", n, edges)
	}
}

// P17-35 / P17-36 (WBS 17.121, 17.151) — output ranges never load whole
// logs; timeline pages are keyset and filterable.
func TestP1735OutputRangeAndTimeline(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	web := dbrepo.NewWebReadRepository(r.h.App.DB().Raw())
	run, res := uuid.New(), uuid.New()
	r.exec(t, "INSERT INTO validation_runs (id, project_id, execution_id, scope, attempt, status, cwd, started_at, finished_at) VALUES ('"+
		run.String()+"', '"+r.pid.String()+"', '"+r.execID.String()+"', 'UNIT', 1, 'FAIL', '/tmp', now(), now())")
	r.exec(t, "INSERT INTO validation_command_results (id, run_id, ord, program, cwd, exit_code, stdout, stderr, duration_ms, timeout_ms, outcome, created_at) VALUES ('"+
		res.String()+"', '"+run.String()+"', 1, 'go', '/tmp', 1, repeat('a', 10485760), 'err<script>', 5, 1000, 'FAIL', now())")
	o, err := web.ValidationOutputRange(ctx, r.pid, res, "stdout", 100, 1000)
	if err != nil || len(o.Text) != 1000 || o.Total != 10485760 || o.Offset != 100 {
		t.Fatalf("range: %v %d %d", err, len(o.Text), o.Total)
	}
	if _, err := web.ValidationOutputRange(ctx, uuid.New(), res, "stdout", 0, 10); err == nil {
		t.Fatal("cross-project output readable")
	}
	if _, err := web.ValidationOutputRange(ctx, r.pid, res, "env", 0, 10); err == nil {
		t.Fatal("unknown stream accepted")
	}
	execRepo := dbrepo.NewExecutionRepository(r.h.App.DB().Raw())
	id := r.execID
	for i := 0; i < 30; i++ {
		typ := execution.EventValidationStarted
		if i%3 == 0 {
			typ = execution.EventCheckpointCreated
		}
		if err := execRepo.AppendEvent(ctx, nil, &execution.Event{ProjectID: r.pid, ExecutionID: &id, Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	p1, _ := web.TimelinePage(ctx, persistence.TimelineFilter{ExecutionID: id, Limit: 10})
	if len(p1) != 10 || p1[0].ExecSeq <= p1[9].ExecSeq {
		t.Fatalf("page: %d", len(p1))
	}
	p2, _ := web.TimelinePage(ctx, persistence.TimelineFilter{ExecutionID: id, Limit: 10, BeforeSeq: p1[9].ExecSeq})
	if len(p2) != 10 || p2[0].ExecSeq != p1[9].ExecSeq-1 {
		t.Fatalf("page 2: %+v", p2[0])
	}
	cps, _ := web.TimelinePage(ctx, persistence.TimelineFilter{ExecutionID: id, Limit: 100, Types: execution.TypesInCategories([]execution.EventCategory{execution.EventCatCheckpoint})})
	if len(cps) != 10 {
		t.Fatalf("category filter: %d", len(cps))
	}
}

// P17-38 (WBS 17.47) — drift acknowledgements are idempotent per hash.
func TestP1738DriftAckRepository(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	var file uuid.UUID
	if err := r.h.DBScan(ctx, "SELECT id FROM state_files WHERE project_id = '"+r.pid.String()+"' LIMIT 1", &file); err != nil {
		t.Skipf("no state file seeded: %v", err)
	}
	repo := dbrepo.NewDriftAckRepository(r.h.App.DB().Raw())
	a, err := repo.AcknowledgeDrift(ctx, &persistence.DriftAcknowledgement{ProjectID: r.pid, StateFileID: file, FSHash: "h1", AcknowledgedBy: "local-admin", Reason: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.AcknowledgeDrift(ctx, &persistence.DriftAcknowledgement{ProjectID: r.pid, StateFileID: file, FSHash: "h1", AcknowledgedBy: "someone"})
	if err != nil || b.ID != a.ID || b.AcknowledgedBy != "local-admin" {
		t.Fatalf("not idempotent: %v %+v", err, b)
	}
	if _, err := repo.GetDriftAck(ctx, file, "h2"); err == nil {
		t.Fatal("unacknowledged hash found")
	}
}
