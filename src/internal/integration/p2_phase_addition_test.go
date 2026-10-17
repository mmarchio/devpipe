package integration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/phase"
)

// devScenario builds the WBS 2.27 state: Phase 1 DONE, Phase 2 EXECUTING,
// Phases 3-4 PENDING.
func (r *appendRig) devScenario(t *testing.T) (uuid.UUID, *phase.Phase, *phase.Phase, *phase.Phase, *phase.Phase) {
	t.Helper()
	ctx := context.Background()
	pid := r.project(t)
	p1 := r.append(t, pid, "Phase 1")
	p2 := r.append(t, pid, "Phase 2")
	p3 := r.append(t, pid, "Phase 3")
	p4 := r.append(t, pid, "Phase 4")
	if err := r.repo.MarkCompleted(ctx, p1.ID); err != nil {
		t.Fatalf("mark phase 1 done: %v", err)
	}
	if err := r.repo.MarkStarted(ctx, p2.ID); err != nil {
		t.Fatalf("start phase 2: %v", err)
	}
	return pid, p1, p2, p3, p4
}

// T2.27.1 — A phase added while the project is running (phases already
// DONE/EXECUTING) appends to the end.
func TestT2271AppendDuringRunning(t *testing.T) {
	r := newAppendRig(t)
	pid, _, _, _, _ := r.devScenario(t)
	if p := r.append(t, pid, "Security Hardening"); p.Order != 5 {
		t.Fatalf("appended order %d, want 5", p.Order)
	}
}

// T2.27.2 — Adding a phase does not renumber the existing phases.
func TestT2272ExistingPhasesNotRenumbered(t *testing.T) {
	r := newAppendRig(t)
	pid, p1, p2, p3, p4 := r.devScenario(t)
	r.append(t, pid, "Security Hardening")
	got, err := r.repo.ListByProject(context.Background(), pid)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("phase count %d, want 5", len(got))
	}
	for _, ph := range got {
		want := 0
		switch ph.ID {
		case p1.ID:
			want = 1
		case p2.ID:
			want = 2
		case p3.ID:
			want = 3
		case p4.ID:
			want = 4
		}
		if want != 0 && ph.Order != want {
			t.Fatalf("phase %q order = %d, want %d (existing order must remain untouched)", ph.Name, ph.Order, want)
		}
	}
}

// T2.27.3 — The current (next incomplete) phase is unchanged by an append.
func TestT2273CurrentPhaseUnchanged(t *testing.T) {
	r := newAppendRig(t)
	pid, _, p2, _, _ := r.devScenario(t)
	ctx := context.Background()
	before, err := r.repo.GetNextIncomplete(ctx, pid)
	if err != nil {
		t.Fatalf("next incomplete: %v", err)
	}
	r.append(t, pid, "Security Hardening")
	after, err := r.repo.GetNextIncomplete(ctx, pid)
	if err != nil {
		t.Fatalf("next incomplete: %v", err)
	}
	if before == nil || after == nil || before.ID != p2.ID || after.ID != p2.ID {
		t.Fatalf("current phase changed: before=%v after=%v, want phase 2 both times", before, after)
	}
}

// T2.27.4 — A phase appended during development later becomes the current
// phase once the earlier phases complete.
func TestT2274AppendedPhaseBecomesCurrent(t *testing.T) {
	r := newAppendRig(t)
	pid, _, p2, p3, p4 := r.devScenario(t)
	p5 := r.append(t, pid, "Security Hardening")
	ctx := context.Background()
	for _, ph := range []*phase.Phase{p2, p3, p4} {
		if err := r.repo.MarkCompleted(ctx, ph.ID); err != nil {
			t.Fatalf("mark %s done: %v", ph.Name, err)
		}
	}
	next, err := r.repo.GetNextIncomplete(ctx, pid)
	if err != nil {
		t.Fatalf("next incomplete: %v", err)
	}
	if next == nil || next.ID != p5.ID || next.Order != 5 {
		t.Fatalf("next incomplete = %+v, want appended phase 5", next)
	}
}

// T2.27.5 — Concurrent additions while phases exist receive deterministic,
// unique, contiguous positions after the existing maximum (5..4+n).
func TestT2275ConcurrentAdditionsDeterministic(t *testing.T) {
	r := newAppendRig(t)
	pid, _, _, _, _ := r.devScenario(t)
	const n = 8
	orders := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := r.repo.Append(context.Background(), pid, fmt.Sprintf("Added %d", i), "")
			if err != nil {
				errs[i] = err
				return
			}
			orders[i] = p.Order
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d: %v", i, e)
		}
	}
	sort.Ints(orders)
	for i, o := range orders {
		if o != 5+i {
			t.Fatalf("orders %v, want exactly 5..%d (contiguous after existing max)", orders, 4+n)
		}
	}
}
