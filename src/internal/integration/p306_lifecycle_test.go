package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p306Rig struct {
	h         *Harness
	proj      *service.ProjectService
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP306Rig(t *testing.T) *p306Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "lifecycle project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p306Rig{
		h: h, proj: projSvc, p: p,
		lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool),
	}
}

func (r *p306Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.h.DBExec(ctx, "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func (r *p306Rig) current(t *testing.T) *project.Project {
	t.Helper()
	p, err := r.proj.GetProject(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return p
}

// T3.6.12 — Invalid transition does not mutate persisted state.
func TestT3612InvalidTransitionNoMutation(t *testing.T) {
	r := newP306Rig(t)
	ctx := context.Background()
	before := r.current(t)
	// DRAFT -> RUNNING is invalid.
	_, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "skip", project.StatusRUNNING)
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	after := r.current(t)
	if after.Status != before.Status {
		t.Fatalf("status mutated: %s -> %s", before.Status, after.Status)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at mutated: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// A valid transition persists the new status.
func TestT36ValidTransitionPersists(t *testing.T) {
	r := newP306Rig(t)
	ctx := context.Background()
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "readiness met", project.StatusREADY); err != nil {
		t.Fatalf("DRAFT->READY: %v", err)
	}
	if got := r.current(t).Status; got != project.StatusREADY {
		t.Fatalf("status = %s, want READY", got)
	}
	// READY -> RUNNING, then a terminal transition, then a rejected one.
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "skip", project.StatusRUNNING); err != nil {
		t.Fatalf("READY->RUNNING: %v", err)
	}
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "complete", project.StatusCOMPLETED); err != nil {
		t.Fatalf("RUNNING->COMPLETED: %v", err)
	}
	if got := r.current(t).Status; got != project.StatusCOMPLETED {
		t.Fatalf("status = %s, want COMPLETED", got)
	}
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "skip", project.StatusRUNNING); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("terminal transition: expected ErrInvalidInput, got %v", err)
	}
	if got := r.current(t).Status; got != project.StatusCOMPLETED {
		t.Fatalf("terminal status mutated to %s", got)
	}
}

// Unknown project is not found.
func TestT36xUnknownProject(t *testing.T) {
	r := newP306Rig(t)
	if _, err := r.lifecycle.Transition(context.Background(), "22222222-2222-2222-2222-222222222222", "x", project.StatusREADY); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
