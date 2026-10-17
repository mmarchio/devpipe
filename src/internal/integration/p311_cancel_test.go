package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

type p311Rig struct {
	h         *Harness
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP311Rig(t *testing.T) *p311Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "cancel project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p311Rig{h: h, p: p, lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool)}
}

func (r *p311Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func (r *p311Rig) status(t *testing.T) project.ProjectStatus {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.p.ID.String()+"'", &s); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return project.ProjectStatus(s)
}

// seedHistory builds the execution history that cancellation must preserve:
// two phases (one DONE, one EXECUTING), a WBS document, an execution plan
// and work units in mixed states.
func (r *p311Rig) seedHistory(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	phases := dbrepo.NewPhaseRepository(raw)
	docs := dbrepo.NewDocumentRepository(raw)
	plans := dbrepo.NewPlanRepository(raw)
	units := dbrepo.NewWorkUnitRepository(raw)
	ph1, err := phases.Append(ctx, r.p.ID, "foundation", "")
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	ph2, err := phases.Append(ctx, r.p.ID, "persistence", "")
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if err := phases.MarkCompleted(ctx, ph1.ID); err != nil {
		t.Fatalf("complete 1: %v", err)
	}
	if err := phases.MarkStarted(ctx, ph2.ID); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if _, err := docs.Create(ctx, ph1.ID.String(), "WBS", "foundation wbs"); err != nil {
		t.Fatalf("doc 1: %v", err)
	}
	if _, err := docs.Create(ctx, ph2.ID.String(), "WBS", "persistence wbs"); err != nil {
		t.Fatalf("doc 2: %v", err)
	}
	p1, err := wbs.NewExecutionPlan(ph1.ID, 1)
	if err != nil {
		t.Fatalf("new plan 1: %v", err)
	}
	if err := plans.Create(ctx, p1); err != nil {
		t.Fatalf("plan 1: %v", err)
	}
	p2, err := wbs.NewExecutionPlan(ph2.ID, 1)
	if err != nil {
		t.Fatalf("new plan 2: %v", err)
	}
	if err := plans.Create(ctx, p2); err != nil {
		t.Fatalf("plan 2: %v", err)
	}
	u1, err := wbs.NewWorkUnit(p1.ID, 1, "unit a", wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit 1: %v", err)
	}
	if err := units.Create(ctx, u1); err != nil {
		t.Fatalf("unit 1: %v", err)
	}
	if err := units.MarkDone(ctx, u1.ID); err != nil {
		t.Fatalf("mark unit 1: %v", err)
	}
	u2, err := wbs.NewWorkUnit(p2.ID, 1, "unit b", wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit 2: %v", err)
	}
	if err := units.Create(ctx, u2); err != nil {
		t.Fatalf("unit 2: %v", err)
	}
}

type p311counts struct{ phases, docs, plans, units int }

func (r *p311Rig) counts(t *testing.T) p311counts {
	t.Helper()
	ctx := context.Background()
	var c p311counts
	if err := r.h.DBScan(ctx, "SELECT COUNT(*) FROM phases WHERE project_id = '"+r.p.ID.String()+"'", &c.phases); err != nil {
		t.Fatalf("count phases: %v", err)
	}
	if err := r.h.DBScan(ctx, "SELECT COUNT(*) FROM phase_documents WHERE phase_id IN (SELECT id FROM phases WHERE project_id = '"+r.p.ID.String()+"')", &c.docs); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if err := r.h.DBScan(ctx, "SELECT COUNT(*) FROM execution_plans WHERE phase_id IN (SELECT id FROM phases WHERE project_id = '"+r.p.ID.String()+"')", &c.plans); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if err := r.h.DBScan(ctx, "SELECT COUNT(*) FROM work_units WHERE execution_plan_id IN (SELECT id FROM execution_plans WHERE phase_id IN (SELECT id FROM phases WHERE project_id = '"+r.p.ID.String()+"'))", &c.units); err != nil {
		t.Fatalf("count units: %v", err)
	}
	return c
}

// T3.11.1 — RUNNING project cancelled.
func TestT3111RunningCancelled(t *testing.T) {
	r := newP311Rig(t)
	r.setStatus(t, "RUNNING")
	if _, err := r.lifecycle.Cancel(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := r.status(t); got != project.StatusCANCELLED {
		t.Fatalf("status = %s, want CANCELLED", got)
	}
}

// T3.11.2 — PAUSED project cancelled.
func TestT3112PausedCancelled(t *testing.T) {
	r := newP311Rig(t)
	r.setStatus(t, "PAUSED")
	if _, err := r.lifecycle.Cancel(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := r.status(t); got != project.StatusCANCELLED {
		t.Fatalf("status = %s, want CANCELLED", got)
	}
}

// Only RUNNING/PAUSED cancel; the rest are rejected.
func TestT311xOtherStatesRejected(t *testing.T) {
	for _, s := range []string{"DRAFT", "READY", "COMPLETED", "FAILED"} {
		t.Run(s, func(t *testing.T) {
			r := newP311Rig(t)
			r.setStatus(t, s)
			_, err := r.lifecycle.Cancel(context.Background(), r.p.ID.String())
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
			if got := r.status(t); got != project.ProjectStatus(s) {
				t.Fatalf("status = %s, want %s", got, s)
			}
		})
	}
}

// T3.11.3–T3.11.5 — Cancellation preserves phases, WBS, execution plan
// history (and work units), row counts and statuses included.
func TestT3113To5HistoryPreserved(t *testing.T) {
	r := newP311Rig(t)
	ctx := context.Background()
	r.seedHistory(t)
	before := r.counts(t)
	if before.phases != 2 || before.docs != 2 || before.plans != 2 || before.units != 2 {
		t.Fatalf("seed counts = %+v", before)
	}
	r.setStatus(t, "RUNNING")
	if _, err := r.lifecycle.Cancel(ctx, r.p.ID.String()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	after := r.counts(t)
	if after != before {
		t.Fatalf("history lost: before=%+v after=%+v", before, after)
	}
	// Phase statuses themselves are preserved, not reset.
	var status1, status2 string
	if err := r.h.DBScan(ctx, "SELECT status FROM phases WHERE project_id = '"+r.p.ID.String()+"' ORDER BY phase_order LIMIT 1", &status1); err != nil {
		t.Fatal(err)
	}
	if err := r.h.DBScan(ctx, "SELECT status FROM phases WHERE project_id = '"+r.p.ID.String()+"' ORDER BY phase_order DESC LIMIT 1", &status2); err != nil {
		t.Fatal(err)
	}
	if status1 != "DONE" || status2 != "EXECUTING" {
		t.Fatalf("phase statuses altered: %s / %s", status1, status2)
	}
	// Cancel event persisted.
	var from, to, reason string
	if err := r.h.DBScan(ctx, "SELECT from_status, to_status, reason FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'", &from, &to, &reason); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "RUNNING" || to != "CANCELLED" || reason != "cancel" {
		t.Fatalf("event = %s -> %s reason=%q, want RUNNING -> CANCELLED reason=cancel", from, to, reason)
	}
}

// T3.11.6 — Cancelled project cannot resume (terminal state).
func TestT3116CancelledCannotResume(t *testing.T) {
	r := newP311Rig(t)
	ctx := context.Background()
	r.setStatus(t, "PAUSED")
	if _, err := r.lifecycle.Cancel(ctx, r.p.ID.String()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := r.lifecycle.Resume(ctx, r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("resume of CANCELLED: expected ErrInvalidInput, got %v", err)
	}
	if _, err := r.lifecycle.Start(ctx, r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("start of CANCELLED: expected ErrInvalidInput, got %v", err)
	}
	if _, err := r.lifecycle.Pause(ctx, r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("pause of CANCELLED: expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusCANCELLED {
		t.Fatalf("status = %s, want CANCELLED", got)
	}
}
