package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/app"
	"overseer/internal/backup"
	"overseer/internal/database"
	"overseer/internal/execution"
)

// p18CleanDB creates an empty database and returns a pool on it.
func p18CleanDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	base := baseDSN()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("pg: %v", err)
	}
	name := fmt.Sprintf("overseer_restore_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	pool, err := pgxpool.New(ctx, strings.Replace(base, "/devpipe", "/"+name, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); dropDB(t, base, name) })
	return pool, name
}

func p18Migrations(t *testing.T) []database.Migration {
	t.Helper()
	migs, err := database.LoadMigrations(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return migs
}

// tableDigests returns an md5 of every table's sorted rows.
func tableDigests(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, skip ...string) map[string]string {
	t.Helper()
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, n := range names {
		if inList(skip, n) {
			continue
		}
		var d *string
		if err := q.QueryRow(ctx, `SELECT md5(string_agg(x::text, E'\n' ORDER BY x::text)) FROM public."`+n+`" x`).Scan(&d); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = ""
		if d != nil {
			out[n] = *d
		}
	}
	return out
}

func inList(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// fkViolations counts rows violating any foreign key (restores load with
// constraint triggers disabled, so this proves internal consistency).
func fkViolations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT k.conname, c.relname, r.relname,
		array(SELECT attname FROM unnest(k.conkey) WITH ORDINALITY u(n, i) JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = u.n ORDER BY i),
		array(SELECT attname FROM unnest(k.confkey) WITH ORDINALITY u(n, i) JOIN pg_attribute a ON a.attrelid = k.confrelid AND a.attnum = u.n ORDER BY i)
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_class r ON r.oid = k.confrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE k.contype = 'f' AND n.nspname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	type fk struct {
		name, child, parent string
		cc, pc              []string
	}
	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.name, &f.child, &f.parent, &f.cc, &f.pc); err != nil {
			t.Fatal(err)
		}
		fks = append(fks, f)
	}
	rows.Close()
	out := map[string]int{}
	for _, f := range fks {
		var nn, join []string
		for i := range f.cc {
			nn = append(nn, `c."`+f.cc[i]+`" IS NOT NULL`)
			join = append(join, `p."`+f.pc[i]+`" = c."`+f.cc[i]+`"`)
		}
		q := `SELECT count(*) FROM public."` + f.child + `" c WHERE ` + strings.Join(nn, " AND ") +
			` AND NOT EXISTS (SELECT 1 FROM public."` + f.parent + `" p WHERE ` + strings.Join(join, " AND ") + `)`
		var n int
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if n > 0 {
			out[f.name] = n
		}
	}
	return out
}

// P18-23 (WBS 18.41–18.43) — a backup taken during active execution
// contains every table, carries its metadata, and restores into a clean
// database without impossible partial state; a completed project's backup
// restores row-for-row.
func TestP1823Backup(t *testing.T) {
	r := p17NewRigWith(t, p16Options{phases: []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}})
	ctx := context.Background()
	pool := r.h.App.DB().Raw()
	for _, k := range []string{"a", "b"} {
		r.queuePlan(r.planJSON(p16Unit{key: k}))
		r.queue("complete")
	}
	// Backup while the engine is driving the execution.
	dirActive := filepath.Join(t.TempDir(), "active")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = r.eng().Run(ctx, r.execID, 300) }()
	time.Sleep(50 * time.Millisecond)
	m, err := backup.Backup(ctx, pool, dirActive, backup.Options{AppVersion: app.Version, Profile: "test"})
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("execution %s", r.execStatus(t))
	}
	// 18.41 — required tables present; 18.42 — metadata.
	have := map[string]bool{}
	for _, tb := range m.Tables {
		have[tb.Name] = true
	}
	for _, want := range []string{"projects", "phases", "wbs_versions", "execution_plans", "work_units", "runtime_state", "worker_prompts",
		"worker_results", "validation_runs", "validation_command_results", "repository_checkpoints", "audit_events", "executions",
		"execution_events", "schema_migrations"} {
		if !have[want] {
			t.Fatalf("backup lacks %s", want)
		}
	}
	if have["restore_state"] {
		t.Fatal("per-database restore state copied")
	}
	rm, err := backup.ReadManifest(dirActive)
	if err != nil || rm.SchemaVersion != r.h.App.SchemaVersion() || rm.AppVersion != app.Version || rm.BackupID != m.BackupID || rm.CreatedAt.IsZero() {
		t.Fatalf("manifest: %+v %v", rm, err)
	}
	var recs int
	_ = r.h.DBScan(ctx, "SELECT count(*) FROM backup_records WHERE id = '"+m.BackupID.String()+"' AND schema_version = "+fmt.Sprint(rm.SchemaVersion), &recs)
	if recs != 1 {
		t.Fatal("backup not recorded")
	}
	// 18.43 — the active-time backup restores consistently.
	target, _ := p18CleanDB(t)
	res, err := backup.Restore(ctx, target, dirActive, p18Migrations(t))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if v := fkViolations(t, ctx, target); len(v) != 0 {
		t.Fatalf("restored active-time backup has FK violations: %v", v)
	}
	var st string
	_ = target.QueryRow(ctx, "SELECT status FROM restore_state").Scan(&st)
	if st != "VALIDATION_PENDING" || res.TotalRows == 0 {
		t.Fatalf("restore state %q rows %d", st, res.TotalRows)
	}
	// Completed project: backup after completion restores exactly.
	dirDone := filepath.Join(t.TempDir(), "done")
	if _, err := backup.Backup(ctx, pool, dirDone, backup.Options{AppVersion: app.Version, Profile: "test"}); err != nil {
		t.Fatal(err)
	}
	src := tableDigests(t, ctx, pool, "backup_records")
	target2, _ := p18CleanDB(t)
	if _, err := backup.Restore(ctx, target2, dirDone, p18Migrations(t)); err != nil {
		t.Fatal(err)
	}
	dst := tableDigests(t, ctx, target2, "backup_records", "restore_state")
	for n, d := range src {
		if dst[n] != d {
			t.Fatalf("table %s differs after restore", n)
		}
	}
	// Restore refuses a non-empty target and a tampered backup.
	if _, err := backup.Restore(ctx, target2, dirDone, p18Migrations(t)); err == nil {
		t.Fatal("restored into a non-empty database")
	}
	tampered := filepath.Join(dirDone, rm.Tables[0].File)
	_ = os.WriteFile(tampered, append(mustRead(t, tampered), []byte("x\n")...), 0o640)
	target3, _ := p18CleanDB(t)
	if _, err := backup.Restore(ctx, target3, dirDone, p18Migrations(t)); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("tampered backup: %v", err)
	}
	_ = execution.StepCompleted
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
