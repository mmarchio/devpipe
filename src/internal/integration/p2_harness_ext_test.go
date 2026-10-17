package integration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
)

// WBS 2.25 — Database integration test infrastructure. The harness must give
// persistence tests (projects, phases, WBS, plans, work units, dependencies,
// transactions, concurrency) a clean, migrated, isolated PostgreSQL database,
// and PostgreSQL-semantics tests must run against real PostgreSQL. Each test
// below verifies one infrastructure property; a RED here means the harness
// (harness.go) must be extended.

const finalMigration = 9 // 00001..00009 (latest: updated_at)

func migrationMaxVersion(t *testing.T, h *Harness) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	return version
}

func assertCoreTablesEmpty(t *testing.T, h *Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, table := range []string{"projects", "phases", "work_units", "work_unit_dependencies"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM "+table, &n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows, want 0 (clean schema)", table, n)
		}
	}
}

// T2.25.1 — Clean schema available per test suite: every harness starts with
// empty core tables.
func TestT2251CleanSchemaPerSuite(t *testing.T) {
	skipIfNoPG(t)
	for i := 0; i < 2; i++ {
		h := NewHarness(t)
		assertCoreTablesEmpty(t, h)
	}
}

// T2.25.2 — Migrations execute on the harness database.
func TestT2252MigrationsExecute(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	if v := migrationMaxVersion(t, h); v < finalMigration {
		t.Fatalf("max migration version %d, want >= %d", v, finalMigration)
	}
}

// T2.25.3 — Tests don't leak state across harness instances: data written
// through one harness is invisible to another.
func TestT2253NoStateLeak(t *testing.T) {
	skipIfNoPG(t)
	h1 := NewHarness(t)
	h2 := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, name := range []string{"leak_probe_a", "leak_probe_b"} {
		sql := "INSERT INTO projects (id, name) VALUES ('" + uuid.New().String() + "', '" + name + "')"
		if _, err := h1.DBExec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var n1, n2 int
	if err := h1.DBScan(ctx, "SELECT count(*) FROM projects", &n1); err != nil {
		t.Fatal(err)
	}
	if err := h2.DBScan(ctx, "SELECT count(*) FROM projects", &n2); err != nil {
		t.Fatal(err)
	}
	if n1 != 2 {
		t.Fatalf("h1 projects = %d, want 2", n1)
	}
	if n2 != 0 {
		t.Fatalf("h2 sees %d rows written through h1 (leak)", n2)
	}
}

// T2.25.4 — Constraint tests use real PostgreSQL: a live FK violation is
// detected by the server, not the driver.
func TestT2254ConstraintsOnRealPG(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		uuid.New().String() + "', '" + uuid.New().String() + "', 1, 'x', 'PENDING')"
	if _, err := h.DBExec(ctx, sql); err == nil {
		t.Fatal("insert with nonexistent project_id succeeded, want FK violation from real PostgreSQL")
	}
}

// T2.25.5 — Concurrent phase-order test uses real PostgreSQL: 12 concurrent
// appends through the repository produce exactly orders 1..12.
func TestT2255ConcurrentPhaseOrderOnRealPG(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	repo := dbrepo.NewPhaseRepository(h.App.DB().Raw())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
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
			p, err := repo.Append(ctx, pid, fmt.Sprintf("P%d", i), "")
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
			t.Fatalf("orders %v, want exactly 1..%d on real PostgreSQL", orders, n)
		}
	}
}
