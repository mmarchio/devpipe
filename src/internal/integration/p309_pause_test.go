package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/project"
	"overseer/internal/service"
)

// p309Rig reuses the P3-08 rig shape (same lifecycle service).
type p309Rig = p308Rig

func newP309Rig(t *testing.T) *p309Rig { return newP308Rig(t) }

// T3.9.1 — RUNNING project pauses.
func TestT391RunningPauses(t *testing.T) {
	r := newP309Rig(t)
	r.setStatus(t, "RUNNING")
	if _, err := r.lifecycle.Pause(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := r.status(t); got != project.StatusPAUSED {
		t.Fatalf("status = %s, want PAUSED", got)
	}
}

// T3.9.2 — READY project cannot pause.
func TestT392ReadyCannotPause(t *testing.T) {
	r := newP309Rig(t)
	r.setStatus(t, "READY")
	_, err := r.lifecycle.Pause(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusREADY {
		t.Fatalf("status = %s, want READY", got)
	}
}

// T3.9.3 — DRAFT project cannot pause.
func TestT393DraftCannotPause(t *testing.T) {
	r := newP309Rig(t)
	_, err := r.lifecycle.Pause(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if got := r.status(t); got != project.StatusDRAFT {
		t.Fatalf("status = %s, want DRAFT", got)
	}
}

// T3.9.4 — Already PAUSED request: idempotent no-op (preferred API
// behavior per WBS 3.9). No error, no new event, no updated_at change.
func TestT394AlreadyPausedIdempotent(t *testing.T) {
	r := newP309Rig(t)
	r.setStatus(t, "RUNNING")
	if _, err := r.lifecycle.Pause(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	first := r.eventRows(t)
	var updatedBefore any
	if err := r.h.DBScan(context.Background(), "SELECT updated_at FROM projects WHERE id = '"+r.p.ID.String()+"'", &updatedBefore); err != nil {
		t.Fatalf("scan updated_at: %v", err)
	}
	if _, err := r.lifecycle.Pause(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("idempotent pause returned %v, want nil", err)
	}
	if got := r.status(t); got != project.StatusPAUSED {
		t.Fatalf("status = %s, want PAUSED", got)
	}
	if n := r.eventRows(t); n != first {
		t.Fatalf("event rows = %d, want %d (no duplicate event)", n, first)
	}
	var updatedAfter any
	if err := r.h.DBScan(context.Background(), "SELECT updated_at FROM projects WHERE id = '"+r.p.ID.String()+"'", &updatedAfter); err != nil {
		t.Fatalf("scan updated_at: %v", err)
	}
	if updatedBefore != updatedAfter {
		t.Fatalf("updated_at changed on idempotent pause: %v -> %v", updatedBefore, updatedAfter)
	}
}

// T3.9.5 — Pause persisted (event RUNNING→PAUSED, reason "pause").
func TestT395PausePersisted(t *testing.T) {
	r := newP309Rig(t)
	r.setStatus(t, "RUNNING")
	if _, err := r.lifecycle.Pause(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	var from, to, reason string
	if err := r.h.DBScan(context.Background(),
		"SELECT from_status, to_status, reason FROM project_status_events WHERE project_id = '"+r.p.ID.String()+"'",
		&from, &to, &reason); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if from != "RUNNING" || to != "PAUSED" || reason != "pause" {
		t.Fatalf("event = %s -> %s reason=%q, want RUNNING -> PAUSED reason=pause", from, to, reason)
	}
}
