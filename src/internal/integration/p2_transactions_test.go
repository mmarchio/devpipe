package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// txRig bundles a harness with the repositories exercised by the
// transaction-boundary tests (WBS 2.21, T2.21.1-T2.21.4).
type txRig struct {
	h    *Harness
	pool *pgxpool.Pool
	proj *dbrepo.ProjectRepository
	ph   *dbrepo.PhaseRepository
	plan *dbrepo.PlanRepository
	wu   *dbrepo.WorkUnitRepository
}

func newTxRig(t *testing.T) *txRig {
	t.Helper()
	h := NewHarness(t)
	pool := h.App.DB().Raw()
	return &txRig{
		h:    h,
		pool: pool,
		proj: dbrepo.NewProjectRepository(pool),
		ph:   dbrepo.NewPhaseRepository(pool),
		plan: dbrepo.NewPlanRepository(pool),
		wu:   dbrepo.NewWorkUnitRepository(pool),
	}
}

func (r *txRig) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM "+table, &n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedPhase creates a committed project + phase and returns the phase ID.
func (r *txRig) seedPhase(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pid := uuid.New()
	if _, err := r.h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	p, err := r.ph.Append(ctx, pid, "Phase 1", "")
	if err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	return p.ID
}

// T2.21.1 — Successful transaction commits: work performed through the
// domain-level transaction surface is durable and visible after Commit.
func TestT2211SuccessfulTransactionCommits(t *testing.T) {
	r := newTxRig(t)
	ctx := context.Background()
	var projID uuid.UUID
	err := dbrepo.Run(ctx, r.pool, func(tx persistence.Transaction) error {
		proj, ph := r.proj.WithTx(tx), r.ph.WithTx(tx)
		p, err := project.New("Committed project", "")
		if err != nil {
			return err
		}
		projID = p.ID
		if err := proj.Create(ctx, p); err != nil {
			return err
		}
		if _, err := ph.Append(ctx, projID, "Inside tx", ""); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := r.proj.Get(ctx, projID.String())
	if err != nil {
		t.Fatalf("committed project not visible after commit: %v", err)
	}
	if got.Name != "Committed project" {
		t.Fatalf("project name %q, want %q", got.Name, "Committed project")
	}
	if _, err := r.h.DBExec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("harness unusable: %v", err)
	}
	phases, err := r.ph.ListByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	if len(phases) != 1 {
		t.Fatalf("phases in committed transaction %d, want 1", len(phases))
	}
}

// T2.21.2 — Failure rolls back: create plan, create units, dependency
// creation fails -> NO partial plan, NO orphan units, NO partial
// dependency graph.
func TestT2212FailureRollsBack(t *testing.T) {
	r := newTxRig(t)
	ctx := context.Background()
	// A committed foreign plan whose unit makes the cross-plan edge invalid.
	foreignPhase := r.seedPhase(t)
	foreignPlan, err := wbs.NewExecutionPlan(foreignPhase, 1)
	if err != nil {
		t.Fatalf("foreign plan: %v", err)
	}
	if err := r.plan.Create(ctx, foreignPlan); err != nil {
		t.Fatalf("create foreign plan: %v", err)
	}
	foreignUnit, err := wbs.NewWorkUnit(foreignPlan.ID, 1, "F", wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("foreign unit: %v", err)
	}
	if err := r.wu.Create(ctx, foreignUnit); err != nil {
		t.Fatalf("create foreign unit: %v", err)
	}
	if r.count(t, "work_unit_dependencies") != 0 {
		t.Fatal("precondition: dependencies table should be empty")
	}

	err = dbrepo.Run(ctx, r.pool, func(tx persistence.Transaction) error {
		planRepo, wu := r.plan.WithTx(tx), r.wu.WithTx(tx)
		plan, err := wbs.NewExecutionPlan(r.seedPhase(t), 1)
		if err != nil {
			return err
		}
		if err := planRepo.Create(ctx, plan); err != nil {
			return err
		}
		var unit1 *wbs.WorkUnit
		for i := 1; i <= 2; i++ {
			u, err := wbs.NewWorkUnit(plan.ID, i, "U"+itoa(i), wbs.WorkUnitKindATOMIC)
			if err != nil {
				return err
			}
			if err := wu.Create(ctx, u); err != nil {
				return err
			}
			if i == 1 {
				unit1 = u
			}
		}
		// dependency creation fails: cross-plan reference.
		return wu.AddDependency(ctx, unit1.ID, foreignUnit.ID)
	})
	if err == nil {
		t.Fatal("expected the cross-plan dependency to fail")
	}
	if !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("want persistence.ErrInvalidReference, got %v", err)
	}
	if n := r.count(t, "execution_plans"); n != 1 {
		t.Fatalf("execution_plans %d, want 1 (only the committed foreign plan; no partial plan)", n)
	}
	if n := r.count(t, "work_units"); n != 1 {
		t.Fatalf("work_units %d, want 1 (no orphan units)", n)
	}
	if n := r.count(t, "work_unit_dependencies"); n != 0 {
		t.Fatalf("work_unit_dependencies %d, want 0 (no partial dependency graph)", n)
	}
}

// T2.21.3 — A panic inside a transaction rolls back, does not leave the
// transaction open, and leaves the pool usable.
func TestT2213PanicDoesNotLeaveTransactionOpen(t *testing.T) {
	r := newTxRig(t)
	ctx := context.Background()
	basePhase := r.seedPhase(t)
	if r.count(t, "execution_plans") != 0 {
		t.Fatal("precondition: no plans before the panicked transaction")
	}
	panicked := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate out of Run")
			}
			panicked = true
		}()
		_ = dbrepo.Run(ctx, r.pool, func(tx persistence.Transaction) error {
			planRepo := r.plan.WithTx(tx)
			plan, err := wbs.NewExecutionPlan(basePhase, 1)
			if err != nil {
				return err
			}
			if err := planRepo.Create(ctx, plan); err != nil {
				return err
			}
			panic("boom in transaction body")
		})
	}()
	if !panicked {
		t.Fatal("panic did not propagate")
	}
	if n := r.count(t, "execution_plans"); n != 0 {
		t.Fatalf("execution_plans %d, want 0 (panic body must roll back)", n)
	}
	// Pool remains usable: a committed write is visible afterwards.
	p, err := project.New("After panic", "")
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	if err := r.proj.Create(ctx, p); err != nil {
		t.Fatalf("pool unusable after panicked transaction: %v", err)
	}
	if _, err := r.proj.Get(ctx, p.ID.String()); err != nil {
		t.Fatalf("read after panicked transaction: %v", err)
	}
}

// T2.21.4 — Concurrent phase append remains consistent through the
// transaction surface: all appends succeed and orders are exactly 1..N.
func TestT2214ConcurrentPhaseAppendConsistent(t *testing.T) {
	r := newTxRig(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := r.h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	const n = 12
	var wg sync.WaitGroup
	orders := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := r.ph.Append(ctx, pid, fmt.Sprintf("P%d", i), "")
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
			t.Fatalf("orders %v, want exactly 1..%d (unique, contiguous)", orders, n)
		}
	}
}
