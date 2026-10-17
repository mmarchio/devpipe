package database

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// T18.130 / T18.134 — the application detects the database version and an
// old binary refuses a newer schema; an older schema with migrations
// disabled is refused too.
func TestT18134Compatibility(t *testing.T) {
	dsn := tempDB(t)
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	migs := []Migration{{Version: 1, Name: "a", SQL: "CREATE TABLE t1 (id INT)"}, {Version: 2, Name: "b", SQL: "CREATE TABLE t2 (id INT)"}}
	if v, err := CheckCompatibility(ctx, p.Raw(), migs, false); !errors.Is(err, ErrSchemaOutdated) || v != 0 {
		t.Fatalf("empty db with migrations disabled: %d %v", v, err)
	}
	if err := Migrate(ctx, p.Raw(), migs); err != nil {
		t.Fatal(err)
	}
	if v, err := CheckCompatibility(ctx, p.Raw(), migs, false); err != nil || v != 2 {
		t.Fatalf("current: %d %v", v, err)
	}
	// A newer binary migrated the database further.
	if err := Migrate(ctx, p.Raw(), append(migs, Migration{Version: 3, Name: "c", SQL: "CREATE TABLE t3 (id INT)"})); err != nil {
		t.Fatal(err)
	}
	if v, err := CheckCompatibility(ctx, p.Raw(), migs, true); !errors.Is(err, ErrSchemaNewer) || v != 3 {
		t.Fatalf("old binary on newer schema: %d %v", v, err)
	}
}

// Concurrent instances migrating the same database: the advisory lock
// serializes them; every migration applies exactly once.
func TestConcurrentMigrate(t *testing.T) {
	dsn := tempDB(t)
	ctx := context.Background()
	migs := []Migration{{Version: 1, Name: "a", SQL: "CREATE TABLE t1 (id INT); SELECT pg_sleep(0.2)"}, {Version: 2, Name: "b", SQL: "CREATE TABLE t2 (id INT)"}}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := newPoolForTest(t, dsn)
			errs[i] = Migrate(ctx, p.Raw(), migs)
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("concurrent migrate: %v", e)
		}
	}
	p := newPoolForTest(t, dsn)
	var n int
	_ = p.Raw().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n)
	if n != 2 {
		t.Fatalf("schema_migrations rows %d", n)
	}
}

// T18.132 — a failing transactional migration leaves no partial schema.
func TestT18132NoPartialSchema(t *testing.T) {
	dsn := tempDB(t)
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	bad := []Migration{{Version: 1, Name: "bad", SQL: "CREATE TABLE half (id INT); CREATE TABLE half_two (id INT); SELECT 1/0"}}
	if err := Migrate(ctx, p.Raw(), bad); err == nil {
		t.Fatal("failing migration succeeded")
	}
	var n int
	_ = p.Raw().QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename IN ('half', 'half_two')").Scan(&n)
	if n != 0 {
		t.Fatal("partial schema left behind")
	}
}

// T18.133 — a crash during a non-transactional migration is diagnosable
// and resumes from the last completed step.
func TestT18133ResumableMigration(t *testing.T) {
	dsn := tempDB(t)
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	sql := NonTransactionalDirective + "\nCREATE TABLE IF NOT EXISTS big (id INT, v TEXT);\n" + StepSeparator +
		"\nINSERT INTO big SELECT g, 'x' FROM generate_series(1, 10) g;\n" + StepSeparator + "\nCREATE INDEX CONCURRENTLY IF NOT EXISTS big_v ON big (v);"
	m := Migration{Version: 1, Name: "big", SQL: sql, NonTransactional: true}
	if len(m.Steps()) != 3 {
		t.Fatalf("steps %v", m.Steps())
	}
	crash := errors.New("simulated crash")
	SetStepHook(func(v, step int) error {
		if step == 2 {
			return crash
		}
		return nil
	})
	if err := Migrate(ctx, p.Raw(), []Migration{m}); !errors.Is(err, crash) {
		t.Fatalf("expected crash: %v", err)
	}
	SetStepHook(nil)
	pend, err := PendingProgress(ctx, p.Raw())
	if err != nil || len(pend) != 1 || pend[0].StepsDone != 2 || pend[0].StepsTotal != 3 || pend[0].Status != "IN_PROGRESS" {
		t.Fatalf("diagnosis: %+v %v", pend, err)
	}
	if err := Migrate(ctx, p.Raw(), []Migration{m}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var rows, idx, applied int
	_ = p.Raw().QueryRow(ctx, "SELECT count(*) FROM big").Scan(&rows)
	_ = p.Raw().QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = 'big_v'").Scan(&idx)
	_ = p.Raw().QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version = 1").Scan(&applied)
	if rows != 10 || idx != 1 || applied != 1 {
		t.Fatalf("resumed: rows %d (step 2 must not rerun) idx %d applied %d", rows, idx, applied)
	}
	if pend, _ := PendingProgress(ctx, p.Raw()); len(pend) != 0 {
		t.Fatal("progress row left after completion")
	}
}
