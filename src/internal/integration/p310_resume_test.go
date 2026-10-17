package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p310Rig struct {
	h         *Harness
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP310Rig(t *testing.T) *p310Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "resume project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := dbrepo.NewPhaseRepository(pool).Append(ctx, p.ID, "exec", ""); err != nil {
		t.Fatalf("append phase: %v", err)
	}
	r := &p310Rig{
		h: h, p: p,
		lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool),
	}
	return r
}

func (r *p310Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func (r *p310Rig) status(t *testing.T) project.ProjectStatus {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.p.ID.String()+"'", &s); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return project.ProjectStatus(s)
}

func (r *p310Rig) eventRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// T3.10.1 — PAUSED project resumes.
func TestT3101PausedResumes(t *testing.T) {
	r := newP310Rig(t)
	r.setStatus(t, "PAUSED")
	if _, err := r.lifecycle.Resume(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := r.status(t); got != project.StatusRUNNING {
		t.Fatalf("status = %s, want RUNNING", got)
	}
}

// T3.10.2 — DRAFT cannot resume.
func TestT3102DraftCannotResume(t *testing.T) {
	r := newP310Rig(t)
	_, err := r.lifecycle.Resume(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

// T3.10.3 — COMPLETED cannot resume.
func TestT3103CompletedCannotResume(t *testing.T) {
	r := newP310Rig(t)
	r.setStatus(t, "COMPLETED")
	_, err := r.lifecycle.Resume(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

// T3.10.4 — CANCELLED cannot resume (T3.11.6 covers the same from cancel).
func TestT3104CancelledCannotResume(t *testing.T) {
	r := newP310Rig(t)
	r.setStatus(t, "CANCELLED")
	_, err := r.lifecycle.Resume(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

// READY and RUNNING also cannot resume (only PAUSED resumes).
func TestT310xOtherStatesCannotResume(t *testing.T) {
	for _, s := range []string{"READY", "RUNNING"} {
		t.Run(s, func(t *testing.T) {
			r := newP310Rig(t)
			r.setStatus(t, s)
			if _, err := r.lifecycle.Resume(context.Background(), r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// T3.10.5 — Resume does not alter current phase/work data.
func TestT3105NoWorkDataAlteration(t *testing.T) {
	r := newP310Rig(t)
	ctx := context.Background()
	// Baseline: capture phase state and settings before resume.
	var phaseStatus, phaseStarted string
	var startedAny bool
	err := r.h.DBScan(ctx, "SELECT status, COALESCE(to_char(started_at, 'YYYY-MM-DD\"T\"HH24:MI:SS'), 'NULL') FROM phases WHERE project_id = '"+r.p.ID.String()+"'", &phaseStatus, &phaseStarted)
	if err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	_ = startedAny
	stRepo := dbrepo.NewSettingsRepository(r.h.App.DB().Raw())
	before, err := stRepo.Get(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	var eventsBefore int
	if err := r.h.DBScan(ctx, "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'", &eventsBefore); err != nil {
		t.Fatalf("count: %v", err)
	}
	r.setStatus(t, "PAUSED")
	if _, err := r.lifecycle.Resume(ctx, r.p.ID.String()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Phase untouched: same status, no started_at appeared.
	var afterStatus, afterStarted string
	if err := r.h.DBScan(ctx, "SELECT status, COALESCE(to_char(started_at, 'YYYY-MM-DD\"T\"HH24:MI:SS'), 'NULL') FROM phases WHERE project_id = '"+r.p.ID.String()+"'", &afterStatus, &afterStarted); err != nil {
		t.Fatalf("scan phase after: %v", err)
	}
	if afterStatus != phaseStatus || afterStarted != phaseStarted {
		t.Fatalf("phase data altered: %s/%s -> %s/%s", phaseStatus, phaseStarted, afterStatus, afterStarted)
	}
	after, err := stRepo.Get(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get settings after: %v", err)
	}
	if *after != *before {
		t.Fatalf("settings altered by resume: %+v -> %+v", before, after)
	}
	// Exactly one new event: the resume itself.
	if n := r.eventRows(t); n != eventsBefore+1 {
		t.Fatalf("event rows = %d, want %d", n, eventsBefore+1)
	}
	var from, to, reason string
	if err := r.h.DBScan(ctx,
		"SELECT from_status, to_status, reason FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"' ORDER BY created_at DESC LIMIT 1",
		&from, &to, &reason); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "PAUSED" || to != "RUNNING" || reason != "resume" {
		t.Fatalf("event = %s -> %s reason=%q, want PAUSED -> RUNNING reason=resume", from, to, reason)
	}
}
