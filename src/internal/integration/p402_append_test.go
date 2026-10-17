package integration

import (
	"context"
	"sort"
	"sync"
	"testing"
)

// setProjectStatus drives the project to a lifecycle state by direct SQL —
// the only way to reach states the phase-creation tests don't produce
// (same pattern as p316/p325 integration tests).
func setProjectStatus(t *testing.T, h *Harness, projectID, status string) {
	t.Helper()
	if _, err := h.DBExec(context.Background(),
		"UPDATE projects SET status = '"+status+"' WHERE id = '"+projectID+"'"); err != nil {
		t.Fatal(err)
	}
}

// T4.3.1 — Phase appended during DRAFT: lands at MAX(order)+1.
func TestT431AppendDuringDraft(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h) // new projects are DRAFT
	svc := newPhaseService(t, h)

	_, err := svc.CreatePhase(ctx, projectID, "A", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreatePhase(ctx, projectID, "B", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Order != 2 {
		t.Fatalf("append during DRAFT order = %d, want 2 (MAX+1)", p.Order)
	}
}

// T4.3.2 — Phase appended during READY: lands at MAX(order)+1.
func TestT432AppendDuringReady(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	setProjectStatus(t, h, projectID, "READY")
	svc := newPhaseService(t, h)

	_, _ = svc.CreatePhase(ctx, projectID, "A", "")
	_, _ = svc.CreatePhase(ctx, projectID, "B", "")
	p, err := svc.CreatePhase(ctx, projectID, "C", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Order != 3 {
		t.Fatalf("append during READY order = %d, want 3 (MAX+1)", p.Order)
	}
}

// T4.3.3 — Phase appended during RUNNING: lands at MAX(order)+1, at the
// end of the sequence.
func TestT433AppendDuringRunning(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	setProjectStatus(t, h, projectID, "RUNNING")
	svc := newPhaseService(t, h)

	for _, name := range []string{"A", "B", "C"} {
		_, _ = svc.CreatePhase(ctx, projectID, name, "")
	}
	p, err := svc.CreatePhase(ctx, projectID, "D", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Order != 4 {
		t.Fatalf("append during RUNNING order = %d, want 4 (end of list)", p.Order)
	}
}

// T4.3.4 — Phase appended during PAUSED: lands at MAX(order)+1.
func TestT434AppendDuringPaused(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	setProjectStatus(t, h, projectID, "PAUSED")
	svc := newPhaseService(t, h)

	for _, name := range []string{"A", "B"} {
		_, _ = svc.CreatePhase(ctx, projectID, name, "")
	}
	p, err := svc.CreatePhase(ctx, projectID, "C", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Order != 3 {
		t.Fatalf("append during PAUSED order = %d, want 3 (MAX+1)", p.Order)
	}
}

// T4.3.5 — No existing phase is renumbered: the full order map is
// identical before and after an append.
func TestT435NoRenumbering(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	before := map[string]int{}
	for _, name := range []string{"A", "B", "C"} {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatal(err)
		}
		before[p.ID.String()] = p.Order
	}
	if _, err := svc.CreatePhase(ctx, projectID, "D", ""); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListPhases(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	after := map[string]int{}
	for _, p := range list {
		after[p.ID.String()] = p.Order
	}
	for id, order := range before {
		if got, ok := after[id]; !ok {
			t.Fatalf("existing phase %s missing after append", id)
		} else if got != order {
			t.Fatalf("phase %s renumbered %d -> %d", id, order, got)
		}
	}
}

// T4.3.6 — The current phase is unchanged by an append: its status and
// timestamps are identical before and after.
func TestT436CurrentPhaseUnchanged(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	first, err := svc.CreatePhase(ctx, projectID, "First", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+first.ID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	var beforeStatus, beforeStarted string
	if err := h.DBScan(ctx,
		"SELECT status, started_at::text FROM phases WHERE id = '"+first.ID.String()+"'", &beforeStatus, &beforeStarted); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreatePhase(ctx, projectID, "Second", ""); err != nil {
		t.Fatal(err)
	}
	var afterStatus, afterStarted string
	if err := h.DBScan(ctx,
		"SELECT status, started_at::text FROM phases WHERE id = '"+first.ID.String()+"'", &afterStatus, &afterStarted); err != nil {
		t.Fatal(err)
	}
	if afterStatus != beforeStatus || afterStarted != beforeStarted {
		t.Fatalf("current phase mutated by append: before=(%q %q) after=(%q %q)",
			beforeStatus, beforeStarted, afterStatus, afterStarted)
	}
}

// T4.3.7 — Concurrent additions receive unique sequential positions: N
// real goroutines appending against real PG must produce exactly the
// contiguous sequence 1..N (no gaps, no duplicates).
func TestT437ConcurrentAppendsSequential(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	const n = 12
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := svc.CreatePhase(ctx, projectID, "C"+itoa(i), "")
			if err != nil {
				results <- -1 // signals failure; the test fatals below
				return
			}
			results <- p.Order
		}(i)
	}
	wg.Wait()
	close(results)

	orders := []int{}
	for o := range results {
		if o < 0 {
			t.Fatal("concurrent CreatePhase failed")
		}
		orders = append(orders, o)
	}
	sort.Ints(orders)
	for i, o := range orders {
		if o != i+1 {
			t.Fatalf("concurrent append orders = %v, want exactly 1..%d (unique sequential)", orders, n)
		}
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE project_id = '"+projectID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("concurrent appends persisted %d rows, want %d", count, n)
	}
}
