package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p307Rig struct {
	h         *Harness
	proj      *service.ProjectService
	lifecycle *service.LifecycleService
	p         *project.Project
}

func newP307Rig(t *testing.T) *p307Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "events project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p307Rig{
		h: h, proj: projSvc, p: p,
		lifecycle: service.NewLifecycleService(
			dbrepo.NewProjectRepository(pool),
			dbrepo.NewStatusEventRepository(pool),
			pool,
		),
	}
}

// eventCount returns the number of status-event rows for the project.
func (r *p307Rig) eventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT COUNT(*) FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func (r *p307Rig) currentStatus(t *testing.T) project.ProjectStatus {
	t.Helper()
	var status string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.p.ID.String()+"'", &status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	return project.ProjectStatus(status)
}

// T3.7.1–T3.7.4 — a transition event is recorded with from, to, timestamp.
func TestT371To374EventRecorded(t *testing.T) {
	r := newP307Rig(t)
	ctx := context.Background()
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "readiness met", project.StatusREADY); err != nil {
		t.Fatalf("transition: %v", err)
	}
	var from, to, reason string
	var createdAt any
	q := "SELECT from_status, to_status, reason, created_at FROM project_status_events WHERE project_id = '" + r.p.ID.String() + "'"
	if err := r.h.DBScan(ctx, q, &from, &to, &reason, &createdAt); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "DRAFT" {
		t.Fatalf("from_status = %s, want DRAFT (T3.7.2)", from)
	}
	if to != "READY" {
		t.Fatalf("to_status = %s, want READY (T3.7.3)", to)
	}
	if reason != "readiness met" {
		t.Fatalf("reason = %q", reason)
	}
	if createdAt == nil {
		t.Fatal("created_at not recorded (T3.7.4)")
	}
	if r.eventCount(t) != 1 {
		t.Fatalf("event count = %d, want 1", r.eventCount(t))
	}
	// T3.7.1 — a second transition appends a second event.
	if _, err := r.lifecycle.Transition(ctx, r.p.ID.String(), "start", project.StatusRUNNING); err != nil {
		t.Fatalf("transition 2: %v", err)
	}
	if r.eventCount(t) != 2 {
		t.Fatalf("event count = %d, want 2", r.eventCount(t))
	}
}

// T3.7.5 — Invalid transition produces no event.
func TestT375InvalidTransitionNoEvent(t *testing.T) {
	r := newP307Rig(t)
	_, err := r.lifecycle.Transition(context.Background(), r.p.ID.String(), "skip", project.StatusRUNNING)
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if r.eventCount(t) != 0 {
		t.Fatalf("event count = %d, want 0", r.eventCount(t))
	}
	if r.currentStatus(t) != project.StatusDRAFT {
		t.Fatal("status changed on invalid transition")
	}
}

// T3.7.6 — Project transition and event persist atomically. A forced
// event insert failure must roll back the project update too.
func TestT376Atomicity(t *testing.T) {
	r := newP307Rig(t)
	ctx := context.Background()
	failing := &failingEventRepo{}
	svc := service.NewLifecycleService(
		dbrepo.NewProjectRepository(r.h.App.DB().Raw()),
		failing,
		r.h.App.DB().Raw(),
	)
	_, err := svc.Transition(ctx, r.p.ID.String(), "atomicity", project.StatusREADY)
	if err == nil {
		t.Fatal("expected failure from forced event insert")
	}
	if got := r.currentStatus(t); got != project.StatusDRAFT {
		t.Fatalf("project status = %s after rolled-back transition, want DRAFT", got)
	}
	if r.eventCount(t) != 0 {
		t.Fatalf("event count = %d after rolled-back transition, want 0", r.eventCount(t))
	}
}

type failingEventRepo struct{}

func (failingEventRepo) Record(ctx context.Context, e *persistence.StatusEvent) error {
	return persistence.ErrConflict
}
func (failingEventRepo) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.StatusEvent, error) {
	return nil, nil
}
func (failingEventRepo) WithTx(tx persistence.Transaction) persistence.StatusEventRepository {
	return failingEventRepo{}
}
