package integration

import (
	"context"
	"testing"

	"overseer/internal/wbs"
)

// T2.17.1 — Unit with no dependencies eligible.
func TestT2171NoDependenciesEligible(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	solo := r.unit(t, planID, 1, "solo-pending")
	ready := r.unit(t, planID, 2, "solo-ready")
	if err := r.repo.MarkReady(ctx, ready.ID); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	busy := r.unit(t, planID, 3, "solo-executing")
	if err := r.repo.MarkExecuting(ctx, busy.ID); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	got, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("eligible = %d units, want 2: %v", len(got), ids(got))
	}
	for i, u := range got {
		if u.Sequence != i+1 {
			t.Fatalf("eligible order at %d = seq %d, want %d", i, u.Sequence, i+1)
		}
	}
	if got[0].ID != solo.ID || got[1].ID != ready.ID {
		t.Fatalf("eligible = %v, want [unit 1 PENDING, unit 2 READY]", ids(got))
	}
}

// T2.17.2 — Unit with completed dependencies eligible.
func TestT2172CompletedDependenciesEligible(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	b := r.unit(t, planID, 2, "b")
	c := r.unit(t, planID, 3, "c")
	if err := r.repo.MarkDone(ctx, a.ID); err != nil {
		t.Fatalf("mark a: %v", err)
	}
	if err := r.repo.MarkDone(ctx, b.ID); err != nil {
		t.Fatalf("mark b: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("add dep a: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, b.ID); err != nil {
		t.Fatalf("add dep b: %v", err)
	}
	got, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(got) != 1 || got[0].ID != c.ID {
		t.Fatalf("eligible = %v, want exactly c", ids(got))
	}
}

// T2.17.3 — Pending dependency prevents eligibility.
func TestT2173PendingDependencyPrevents(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	b := r.unit(t, planID, 2, "b")
	c := r.unit(t, planID, 3, "c")
	if err := r.repo.MarkDone(ctx, a.ID); err != nil {
		t.Fatalf("mark a: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("add dep a: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, b.ID); err != nil {
		t.Fatalf("add dep b: %v", err)
	}
	got, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("eligible = %v, want only b (c blocked by pending dep)", ids(got))
	}
}

// T2.17.4 — Failed dependency prevents eligibility.
func TestT2174FailedDependencyPrevents(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	c := r.unit(t, planID, 2, "c")
	if err := r.repo.MarkFailed(ctx, a.ID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("add dep: %v", err)
	}
	got, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("eligible = %v, want none", ids(got))
	}
}

// T2.17.5 — Blocked dependency prevents eligibility.
func TestT2175BlockedDependencyPrevents(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	c := r.unit(t, planID, 2, "c")
	got, err := r.repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Status = wbs.WorkUnitStatusBLOCKED
	if err := r.repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := r.repo.AddDependency(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("add dep: %v", err)
	}
	eligible, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("eligible = %v, want none", ids(eligible))
	}
}

// T2.17.6 — DONE unit isn't returned again.
func TestT2176DoneUnitNotReturned(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	b := r.unit(t, planID, 2, "b")
	c := r.unit(t, planID, 3, "c")
	if err := r.repo.AddDependency(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("add dep: %v", err)
	}
	if err := r.repo.MarkDone(ctx, a.ID); err != nil {
		t.Fatalf("mark a done: %v", err)
	}
	if err := r.repo.MarkDone(ctx, b.ID); err != nil {
		t.Fatalf("mark b done: %v", err)
	}
	got, err := r.repo.ListEligible(ctx, planID)
	if err != nil {
		t.Fatalf("list eligible: %v", err)
	}
	if len(got) != 1 || got[0].ID != c.ID {
		t.Fatalf("eligible = %v, want only c (a and b are DONE)", ids(got))
	}
}
