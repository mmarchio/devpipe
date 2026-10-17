package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/resilience"
)

func p18TxTable(t *testing.T, h *Harness) *pgxpool.Pool {
	t.Helper()
	if _, err := h.DBExec(context.Background(), "CREATE TABLE p18_tx (k TEXT PRIMARY KEY, n INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	return h.App.DB().Raw()
}

func p18TxN(t *testing.T, h *Harness, k string) int {
	t.Helper()
	n := -1
	_ = h.DBScan(context.Background(), "SELECT COALESCE((SELECT n FROM p18_tx WHERE k = '"+k+"'), 0)", &n)
	return n
}

func p18Serialization() error {
	return &pgconn.PgError{Code: "40001", Message: "injected serialization failure"}
}

// T18.35 — an injected serialization failure re-runs the whole
// transaction; the effect is applied exactly once.
func TestT1835SerializationRetried(t *testing.T) {
	h := NewHarness(t)
	pool := p18TxTable(t, h)
	reg := faults.NewRegistry().Arm(faults.Action{Point: faults.BeforeDBCommit, Nth: 1, Times: 1, Kind: faults.KindError, Err: p18Serialization()})
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	attempts := 0
	err := dbrepo.Run(ctx, pool, func(tx persistence.Transaction) error {
		attempts++
		_, err := tx.Exec(ctx, "INSERT INTO p18_tx (k, n) VALUES ('a', 1) ON CONFLICT (k) DO UPDATE SET n = p18_tx.n + 1")
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err %v attempts %d", err, attempts)
	}
	if n := p18TxN(t, h, "a"); n != 1 {
		t.Fatalf("effect applied %d times", n)
	}
}

// T18.35 — a real PostgreSQL deadlock (40P01) between two transactions
// is retried; both updates land exactly once.
func TestT1835DeadlockRetried(t *testing.T) {
	h := NewHarness(t)
	pool := p18TxTable(t, h)
	if _, err := h.DBExec(context.Background(), "INSERT INTO p18_tx VALUES ('a', 0), ('b', 0)"); err != nil {
		t.Fatal(err)
	}
	var barrier sync.WaitGroup
	barrier.Add(2)
	var mu sync.Mutex
	attempts := 0
	tx := func(first, second string) error {
		n := 0
		return dbrepo.Run(context.Background(), pool, func(tx persistence.Transaction) error {
			n++
			mu.Lock()
			attempts++
			mu.Unlock()
			if _, err := tx.Exec(context.Background(), "UPDATE p18_tx SET n = n + 1 WHERE k = $1", first); err != nil {
				return err
			}
			if n == 1 {
				barrier.Done()
				barrier.Wait() // both hold their first lock: a deadlock follows
			}
			_, err := tx.Exec(context.Background(), "UPDATE p18_tx SET n = n + 1 WHERE k = $1", second)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- tx("a", "b") }()
	go func() { errs <- tx("b", "a") }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("transaction failed: %v", err)
		}
	}
	if attempts < 3 {
		t.Fatalf("no deadlock retry happened (attempts %d)", attempts)
	}
	if a, b := p18TxN(t, h, "a"), p18TxN(t, h, "b"); a != 2 || b != 2 {
		t.Fatalf("a=%d b=%d, want 2/2", a, b)
	}
}

// T18.36 — a persistent transient failure is retried a bounded number of
// times, then control returns with the error and nothing committed.
func TestT1836PersistentFailureBounded(t *testing.T) {
	h := NewHarness(t)
	pool := p18TxTable(t, h)
	reg := faults.NewRegistry().Arm(faults.Action{Point: faults.BeforeDBCommit, Kind: faults.KindError, Err: p18Serialization()})
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	start := time.Now()
	err := dbrepo.Run(ctx, pool, func(tx persistence.Transaction) error {
		_, err := tx.Exec(ctx, "INSERT INTO p18_tx (k, n) VALUES ('a', 1)")
		return err
	})
	p, _ := resilience.MustDefault().Policy(resilience.PolicyDBTx)
	if err == nil || reg.Hits(faults.BeforeDBCommit) != p.MaxAttempts {
		t.Fatalf("err %v after %d attempts (policy %d)", err, reg.Hits(faults.BeforeDBCommit), p.MaxAttempts)
	}
	if d := time.Since(start); d > p.OverallBudget+time.Second {
		t.Fatalf("retries took %s", d)
	}
	if n := p18TxN(t, h, "a"); n != 0 {
		t.Fatal("a failed transaction committed")
	}
}

// T18.190 — a failure after COMMIT may already be applied: it is
// AMBIGUOUS and never replayed, even when it looks transient.
func TestT18190AmbiguousCommitNotReplayed(t *testing.T) {
	h := NewHarness(t)
	pool := p18TxTable(t, h)
	reg := faults.NewRegistry().Arm(faults.Action{Point: faults.AfterDBCommit, Times: 1, Kind: faults.KindError, Err: p18Serialization()})
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	err := dbrepo.Run(ctx, pool, func(tx persistence.Transaction) error {
		_, err := tx.Exec(ctx, "INSERT INTO p18_tx (k, n) VALUES ('a', 1) ON CONFLICT (k) DO UPDATE SET n = p18_tx.n + 1")
		return err
	})
	if !resilience.IsAmbiguous(err) || reg.Hits(faults.BeforeDBCommit) != 1 {
		t.Fatalf("err %v (class %s) attempts %d", err, resilience.ClassOf(err), reg.Hits(faults.BeforeDBCommit))
	}
	if n := p18TxN(t, h, "a"); n != 1 {
		t.Fatalf("effect applied %d times", n)
	}
}

// T18.35 (engine) — serialization failures injected into the engine's
// transactions are retried transparently: the execution completes with
// no duplicated step, event or unit completion.
func TestT1835EngineRetriesPreserveIdempotency(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	reg := faults.NewRegistry()
	for _, nth := range []int{1, 2, 4, 7, 11, 16, 22} {
		reg.Arm(faults.Action{Point: faults.BeforeDBCommit, Nth: nth, Times: 1, Kind: faults.KindError, Err: p18Serialization()})
	}
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	res, err := r.eng().Run(ctx, r.execID, 300)
	if err != nil || last(res) != execution.StepCompleted {
		t.Fatalf("run: %v %s", err, p16Trace(res))
	}
	if reg.Hits(faults.BeforeDBCommit) < 22 {
		t.Fatalf("only %d engine commits", reg.Hits(faults.BeforeDBCommit))
	}
	id := r.execID.String()
	if n := r.count(t, "SELECT count(*) FROM (SELECT idempotency_key FROM execution_steps WHERE execution_id = '"+id+"' GROUP BY 1 HAVING count(*) > 1) d"); n != 0 {
		t.Fatalf("%d duplicated steps", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_steps WHERE execution_id = '"+id+"' AND status = 'INTENT'"); n != 0 {
		t.Fatalf("%d steps left in INTENT", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE execution_id = '"+id+"' AND type = 'PROJECT_COMPLETED'"); n != 1 {
		t.Fatalf("completion recorded %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL AND project_id = '"+r.pid.String()+"'"); n != 2 {
		t.Fatalf("unit dispatches %d, want 2", n)
	}
	if s := r.unitStatuses(t, 1); s != "a:DONE,b:DONE" {
		t.Fatalf("units %s", s)
	}
}
