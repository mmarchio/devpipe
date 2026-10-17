package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p312Rig struct {
	h         *Harness
	complete  *service.CompletionService
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP312Rig(t *testing.T) *p312Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "completion project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p312Rig{
		h: h, p: p,
		lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool),
		complete: service.NewCompletionService(dbrepo.NewProjectRepository(pool), dbrepo.NewPhaseRepository(pool),
			service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool)),
	}
}

// addPhaseWithStatus appends a phase and drives it to the given status via
// the repository marks (DONE) or SQL (the rest).
func (r *p312Rig) addPhaseWithStatus(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	phases := dbrepo.NewPhaseRepository(raw)
	ph, err := phases.Append(ctx, r.p.ID, "phase "+status, "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	switch status {
	case "DONE":
		if err := phases.MarkCompleted(ctx, ph.ID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	case "EXECUTING":
		if err := phases.MarkStarted(ctx, ph.ID); err != nil {
			t.Fatalf("mark started: %v", err)
		}
	default:
		if _, err := r.h.DBExec(ctx, "UPDATE phases SET status = '"+status+"' WHERE id = '"+ph.ID.String()+"'"); err != nil {
			t.Fatalf("set phase status: %v", err)
		}
	}
}

func (r *p312Rig) status(t *testing.T) project.ProjectStatus {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.p.ID.String()+"'", &s); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return project.ProjectStatus(s)
}

func (r *p312Rig) run(t *testing.T) {
	t.Helper()
	if _, err := r.lifecycle.Start(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// T3.12.1 — All phases DONE permits completion.
func TestT3121AllDonePermitsCompletion(t *testing.T) {
	r := newP312Rig(t)
	r.setStatusReady(t)
	r.addPhaseWithStatus(t, "DONE")
	r.run(t)
	if _, err := r.complete.Complete(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := r.status(t); got != project.StatusCOMPLETED {
		t.Fatalf("status = %s, want COMPLETED", got)
	}
}

func (r *p312Rig) setStatusReady(t *testing.T) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = 'READY' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set ready: %v", err)
	}
}

// T3.12.2 — A pending phase prevents completion.
func TestT3122PendingPhasePrevents(t *testing.T) {
	r := newP312Rig(t)
	r.setStatusReady(t)
	r.addPhaseWithStatus(t, "DONE")
	r.addPhaseWithStatus(t, "PENDING")
	r.run(t)
	if _, err := r.complete.Complete(context.Background(), r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusRUNNING {
		t.Fatalf("status = %s, want RUNNING", got)
	}
}

// T3.12.3 — An executing phase prevents completion.
func TestT3123ExecutingPhasePrevents(t *testing.T) {
	r := newP312Rig(t)
	r.setStatusReady(t)
	r.addPhaseWithStatus(t, "DONE")
	r.addPhaseWithStatus(t, "EXECUTING")
	r.run(t)
	if _, err := r.complete.Complete(context.Background(), r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusRUNNING {
		t.Fatalf("status = %s, want RUNNING", got)
	}
}

// T3.12.4 — A failed phase prevents automatic completion.
func TestT3124FailedPhasePrevents(t *testing.T) {
	r := newP312Rig(t)
	r.setStatusReady(t)
	r.addPhaseWithStatus(t, "DONE")
	r.addPhaseWithStatus(t, "FAILED")
	r.run(t)
	if _, err := r.complete.Complete(context.Background(), r.p.ID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusRUNNING {
		t.Fatalf("status = %s, want RUNNING", got)
	}
}

// T3.12.5 — Completed project status persisted (+ event, + no longer running).
func TestT3125CompletionPersisted(t *testing.T) {
	r := newP312Rig(t)
	ctx := context.Background()
	r.setStatusReady(t)
	r.addPhaseWithStatus(t, "DONE")
	r.run(t)
	if _, err := r.complete.Complete(ctx, r.p.ID.String()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := r.status(t); got != project.StatusCOMPLETED {
		t.Fatalf("status = %s, want COMPLETED", got)
	}
	var from, to, reason string
	if err := r.h.DBScan(ctx,
		"SELECT from_status, to_status, reason FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"' AND to_status = 'COMPLETED'",
		&from, &to, &reason); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "RUNNING" || to != "COMPLETED" || reason != "complete" {
		t.Fatalf("event = %s -> %s reason=%q, want RUNNING -> COMPLETED reason=complete", from, to, reason)
	}
	// Terminal: a blocked completion must not have written a COMPLETED event.
}

// Only RUNNING projects complete; DRAFT and READY are rejected.
func TestT312xOnlyRunningCompletes(t *testing.T) {
	for _, s := range []string{"DRAFT", "READY"} {
		t.Run(s, func(t *testing.T) {
			r := newP312Rig(t)
			if s == "READY" {
				r.setStatusReady(t)
			}
			r.addPhaseWithStatus(t, "DONE")
			_, err := r.complete.Complete(context.Background(), r.p.ID.String())
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}
