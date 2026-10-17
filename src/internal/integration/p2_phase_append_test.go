package integration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
)

// appendRig bundles a harness (for project rows) with the phase repository.
type appendRig struct {
	h    *Harness
	repo *dbrepo.PhaseRepository
}

func newAppendRig(t *testing.T) *appendRig {
	t.Helper()
	h := NewHarness(t)
	return &appendRig{h: h, repo: dbrepo.NewPhaseRepository(h.App.DB().Raw())}
}

func (r *appendRig) project(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pid := uuid.New()
	if _, err := r.h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	return pid
}

func (r *appendRig) append(t *testing.T, pid uuid.UUID, name string) *phase.Phase {
	t.Helper()
	p, err := r.repo.Append(context.Background(), pid, name, "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return p
}

// T2.7.1 — First phase gets order 1.
func TestT271FirstPhaseOrderOne(t *testing.T) {
	r := newAppendRig(t)
	pid := r.project(t)
	if p := r.append(t, pid, "First"); p.Order != 1 {
		t.Fatalf("first phase order %d, want 1", p.Order)
	}
}

// T2.7.2 — Second phase gets order 2.
func TestT272SecondPhaseOrderTwo(t *testing.T) {
	r := newAppendRig(t)
	pid := r.project(t)
	r.append(t, pid, "First")
	if p := r.append(t, pid, "Second"); p.Order != 2 {
		t.Fatalf("second phase order %d, want 2", p.Order)
	}
}

// T2.7.3 — A phase added while earlier phases are DONE/EXECUTING appends to
// the end; it is not inserted after the executing phase.
func TestT273AppendDuringExecution(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	p1 := r.append(t, pid, "P1")
	p2 := r.append(t, pid, "P2")
	p3 := r.append(t, pid, "P3")
	p4 := r.append(t, pid, "P4")
	// 1 DONE, 2 EXECUTING, 3/4 PENDING — the WBS 2.7 scenario.
	if _, err := r.h.DBExec(ctx, "UPDATE phases SET status='DONE', completed_at=now() WHERE id='"+p1.ID.String()+"'"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if _, err := r.h.DBExec(ctx, "UPDATE phases SET status='EXECUTING', started_at=now() WHERE id='"+p2.ID.String()+"'"); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	p5 := r.append(t, pid, "Security Hardening")
	if p5.Order != 5 {
		t.Fatalf("appended order %d, want 5 (must not land after the executing phase)", p5.Order)
	}
	_ = p3
	_ = p4
}

// T2.7.4 — New phase after completed phases appends.
func TestT274AppendAfterCompleted(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	a := r.append(t, pid, "A")
	b := r.append(t, pid, "B")
	c := r.append(t, pid, "C")
	for _, p := range []*phase.Phase{a, b, c} {
		if _, err := r.h.DBExec(ctx, "UPDATE phases SET status='DONE', completed_at=now() WHERE id='"+p.ID.String()+"'"); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	if p := r.append(t, pid, "D"); p.Order != 4 {
		t.Fatalf("order %d, want 4", p.Order)
	}
}

// T2.7.5 — Deleted phase does not cause order reuse (MAX-based, not gap-fill).
func TestT275NoOrderReuse(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	// Orders 1, 2, 4 — order 3 was deleted.
	for _, o := range []int{1, 2, 4} {
		sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
			uuid.New().String() + "', '" + pid.String() + "', " + fmt.Sprint(o) + ", 'P" + fmt.Sprint(o) + "', 'PENDING')"
		if _, err := r.h.DBExec(ctx, sql); err != nil {
			t.Fatalf("phase %d: %v", o, err)
		}
	}
	// P4-31: the service-created rows that would exist here advanced the
	// project's high-water mark; the direct-SQL seed does not, so backfill
	// it to the seeded maximum (accepted contract change, not a weakened
	// test).
	if _, err := r.h.DBExec(ctx, "UPDATE projects SET max_phase_order = 4 WHERE id = '"+pid.String()+"'"); err != nil {
		t.Fatalf("backfill mark: %v", err)
	}
	if p := r.append(t, pid, "New"); p.Order != 5 {
		t.Fatalf("new order %d, want 5 (no reuse of 3)", p.Order)
	}
}

// T2.7.6 — Concurrent phase creation produces unique order values (real PG).
func TestT276ConcurrentAppendUniqueOrders(t *testing.T) {
	r := newAppendRig(t)
	ctx := context.Background()
	pid := r.project(t)
	const n = 10
	var wg sync.WaitGroup
	orders := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := r.repo.Append(ctx, pid, fmt.Sprintf("P%d", i), "")
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
		if o != i+1 {
			t.Fatalf("orders %v, want exactly 1..%d (unique)", orders, n)
		}
	}
}
