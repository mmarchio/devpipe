package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/persistence"
	"overseer/internal/phase"
)

// T2.8.1 — Get phase.
func TestT281GetPhase(t *testing.T) {
	r := newAppendRig(t)
	pid := r.project(t)
	p := r.append(t, pid, "Target")
	ctx := context.Background()
	got, err := r.repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != p.ID || got.Order != p.Order || got.Name != "Target" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, err := r.repo.Get(ctx, r.project(t)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing phase: got %v, want ErrNotFound", err)
	}
}

// T2.8.2 — List ordered phases (ascending).
func TestT282ListByProjectOrdered(t *testing.T) {
	r := newAppendRig(t)
	pid := r.project(t)
	r.append(t, pid, "One")
	r.append(t, pid, "Two")
	r.append(t, pid, "Three")
	other := r.project(t)
	r.append(t, other, "OtherProj")
	list, err := r.repo.ListByProject(context.Background(), pid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d phases for project, want 3", len(list))
	}
	for i, p := range list {
		if p.Order != i+1 {
			t.Fatalf("list order at %d = %d, want %d", i, p.Order, i+1)
		}
	}
}

// T2.8.3 — Next incomplete skips DONE phases (by order).
func TestT283NextIncompleteSkipsDone(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	p1 := r.append(t, pid, "A")
	p2 := r.append(t, pid, "B")
	p3 := r.append(t, pid, "C")
	p4 := r.append(t, pid, "D")
	for _, p := range []*phase.Phase{p1, p2} {
		if _, err := r.h.DBExec(ctx, "UPDATE phases SET status='DONE', completed_at=now() WHERE id='"+p.ID.String()+"'"); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	next, err := r.repo.GetNextIncomplete(ctx, pid)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if next == nil || next.Order != 3 || next.ID != p3.ID {
		t.Fatalf("next = %+v, want order 3", next)
	}
	_ = p4
}

// T2.8.4 — No incomplete phase returns the defined empty result (nil, nil).
func TestT284NoIncompleteEmpty(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	p1 := r.append(t, pid, "A")
	p2 := r.append(t, pid, "B")
	for _, p := range []*phase.Phase{p1, p2} {
		if _, err := r.h.DBExec(ctx, "UPDATE phases SET status='DONE', completed_at=now() WHERE id='"+p.ID.String()+"'"); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	next, err := r.repo.GetNextIncomplete(ctx, pid)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if next != nil {
		t.Fatalf("expected (nil, nil), got %+v", next)
	}
}

// T2.8.5 — Mark started stores timestamp.
func TestT285MarkStarted(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	p := r.append(t, pid, "Start")
	if err := r.repo.MarkStarted(ctx, p.ID); err != nil {
		t.Fatalf("mark started: %v", err)
	}
	got, _ := r.repo.Get(ctx, p.ID)
	if got.Status != phase.StatusEXECUTING {
		t.Fatalf("status %v, want EXECUTING", got.Status)
	}
	if got.StartedAt == nil {
		t.Fatal("StartedAt not stored")
	}
}

// T2.8.6 — Mark completed stores status and timestamp.
func TestT286MarkCompleted(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	p := r.append(t, pid, "Finish")
	if err := r.repo.MarkStarted(ctx, p.ID); err != nil {
		t.Fatalf("mark started: %v", err)
	}
	if err := r.repo.MarkCompleted(ctx, p.ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	got, _ := r.repo.Get(ctx, p.ID)
	if got.Status != phase.StatusDONE {
		t.Fatalf("status %v, want DONE", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatal("CompletedAt not stored")
	}
}
