package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p308Rig struct {
	h         *Harness
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP308Rig(t *testing.T) *p308Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "start project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p308Rig{
		h: h, p: p,
		lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool),
	}
}

func (r *p308Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func (r *p308Rig) status(t *testing.T) project.ProjectStatus {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.p.ID.String()+"'", &s); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return project.ProjectStatus(s)
}

func (r *p308Rig) eventRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// T3.8.1 — READY project starts.
func TestT381ReadyStarts(t *testing.T) {
	r := newP308Rig(t)
	r.setStatus(t, "READY")
	if _, err := r.lifecycle.Start(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := r.status(t); got != project.StatusRUNNING {
		t.Fatalf("status = %s, want RUNNING", got)
	}
}

// T3.8.2 — DRAFT project cannot start.
func TestT382DraftCannotStart(t *testing.T) {
	r := newP308Rig(t)
	_, err := r.lifecycle.Start(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusDRAFT {
		t.Fatalf("status = %s, want DRAFT", got)
	}
	if r.eventRows(t) != 0 {
		t.Fatal("event written for rejected start")
	}
}

// T3.8.3 — PAUSED project uses resume rather than start.
func TestT383PausedCannotStart(t *testing.T) {
	r := newP308Rig(t)
	r.setStatus(t, "PAUSED")
	_, err := r.lifecycle.Start(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusPAUSED {
		t.Fatalf("status = %s, want PAUSED", got)
	}
}

// T3.8.4 — COMPLETED project cannot start.
func TestT384CompletedCannotStart(t *testing.T) {
	r := newP308Rig(t)
	r.setStatus(t, "COMPLETED")
	_, err := r.lifecycle.Start(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusCOMPLETED {
		t.Fatalf("status = %s, want COMPLETED", got)
	}
}

// T3.8.5 — Start event persisted (READY→RUNNING, reason "start").
func TestT385StartEventPersisted(t *testing.T) {
	r := newP308Rig(t)
	r.setStatus(t, "READY")
	if _, err := r.lifecycle.Start(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	var from, to, reason string
	if err := r.h.DBScan(context.Background(),
		"SELECT from_status, to_status, reason FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'",
		&from, &to, &reason); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "READY" || to != "RUNNING" {
		t.Fatalf("event = %s -> %s, want READY -> RUNNING", from, to)
	}
	if reason != "start" {
		t.Fatalf("reason = %q, want \"start\"", reason)
	}
	if r.eventRows(t) != 1 {
		t.Fatalf("event rows = %d, want 1", r.eventRows(t))
	}
}

// T3.8.6 — Start is lifecycle-only: no planner, no Hermes, no autonomous
// execution. A fully configured project with a phase is started; the phase
// must remain PENDING (execution never began) and exactly one status event
// must exist (nothing else was recorded or launched).
func TestT386LifecycleOnly(t *testing.T) {
	r := newP308Rig(t)
	ctx := context.Background()
	// Give the project a phase and a valid configuration so that, if start
	// had begun autonomous execution, the phase would have moved.
	stRepo := dbrepo.NewSettingsRepository(r.h.App.DB().Raw())
	st, err := stRepo.Get(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	st.WorkingDirectory = "/nonexistent-worker-dir"
	if err := stRepo.Update(ctx, st); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	ph, err := dbrepo.NewPhaseRepository(r.h.App.DB().Raw()).Append(ctx, r.p.ID, "exec", "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	r.setStatus(t, "READY")
	if _, err := r.lifecycle.Start(ctx, r.p.ID.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	var phaseStatus string
	if err := r.h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+ph.ID.String()+"'", &phaseStatus); err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	if phaseStatus != "PENDING" {
		t.Fatalf("phase status = %s after start, want PENDING (no execution began)", phaseStatus)
	}
	if r.eventRows(t) != 1 {
		t.Fatalf("event rows = %d, want exactly 1 (lifecycle event only)", r.eventRows(t))
	}
}
