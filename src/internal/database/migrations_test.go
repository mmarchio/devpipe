package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// tempDB creates a scratch database on the test PG, returns its DSN.
// The caller's cleanup drops it automatically.
func tempDB(t *testing.T) string {
	t.Helper()
	base := testDSN(t)
	if err := rawPing(context.Background(), base); err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	name := fmt.Sprintf("overseer_p107_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		conn.Close(ctx)
		t.Skipf("create temp db: %v", err)
	}
	conn.Close(ctx)
	dsn := strings.Replace(base, "/devpipe", "/"+name, 1)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if c, err := pgx.Connect(cctx, base); err == nil {
			c.Exec(cctx, "DROP DATABASE IF EXISTS "+name)
			c.Close(cctx)
		}
	})
	return dsn
}

func writeMigrations(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newPoolForTest(t *testing.T, dsn string) *Pool {
	t.Helper()
	p, err := New(context.Background(), dsn, testLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// T1.5.1 — Empty database migrates: all Phase 1 migrations apply.
func TestEmptyDatabaseMigrates(t *testing.T) {
	dsn := tempDB(t)
	migDir := t.TempDir()
	writeMigrations(t, migDir, map[string]string{
		"00001_bookkeeping.up.sql": "CREATE TABLE IF NOT EXISTS app_migration_marker (id INT PRIMARY KEY);",
	})
	migs, err := LoadMigrations(migDir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoolForTest(t, dsn)
	if err := Migrate(context.Background(), p.Raw(), migs); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var marker int
	if err := p.Raw().QueryRow(context.Background(), "SELECT count(*) FROM app_migration_marker").Scan(&marker); err != nil {
		t.Fatalf("marker table missing after migration: %v", err)
	}
}

// T1.5.2 — Migration is idempotent: re-run makes no destructive changes.
func TestMigrationIdempotent(t *testing.T) {
	dsn := tempDB(t)
	migDir := t.TempDir()
	writeMigrations(t, migDir, map[string]string{
		"00001_bookkeeping.up.sql": "CREATE TABLE IF NOT EXISTS app_migration_marker (id INT PRIMARY KEY); INSERT INTO app_migration_marker (id) VALUES (1);",
	})
	migs, err := LoadMigrations(migDir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	if err := Migrate(ctx, p.Raw(), migs); err != nil {
		t.Fatal(err)
	}
	v1, err := CurrentVersion(ctx, p.Raw())
	if err != nil || v1 != 1 {
		t.Fatalf("version after first run = %d, %v", v1, err)
	}
	if err := Migrate(ctx, p.Raw(), migs); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	v2, _ := CurrentVersion(ctx, p.Raw())
	if v2 != v1 {
		t.Fatalf("version changed on re-run: %d -> %d", v1, v2)
	}
	var n int
	if err := p.Raw().QueryRow(ctx, "SELECT count(*) FROM app_migration_marker").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-run changed data: rows = %d, want 1", n)
	}
}

// T1.5.3 — Migration version recorded and determinable.
func TestCurrentVersionRecorded(t *testing.T) {
	dsn := tempDB(t)
	migDir := t.TempDir()
	writeMigrations(t, migDir, map[string]string{
		"00001_a.up.sql": "CREATE TABLE IF NOT EXISTS a (id INT);",
		"00002_b.up.sql": "CREATE TABLE IF NOT EXISTS b (id INT PRIMARY KEY);",
	})
	migs, err := LoadMigrations(migDir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	if v, err := CurrentVersion(ctx, p.Raw()); err == nil && v != 0 {
		t.Fatalf("fresh db version = %d, want 0", v)
	}
	if err := Migrate(ctx, p.Raw(), migs); err != nil {
		t.Fatal(err)
	}
	v, err := CurrentVersion(ctx, p.Raw())
	if err != nil || v != 2 {
		t.Fatalf("CurrentVersion = %d, %v; want 2", v, err)
	}
}

// T1.5.4 — Failed migration prevents startup: error returned, version not
// recorded, database left retryable.
func TestFailedMigrationPreventsStartup(t *testing.T) {
	dsn := tempDB(t)
	migDir := t.TempDir()
	writeMigrations(t, migDir, map[string]string{
		"00001_a.up.sql":   "CREATE TABLE IF NOT EXISTS a (id INT);",
		"00002_bad.up.sql": "CREATE TABLE bad (id INT;",
	})
	migs, err := LoadMigrations(migDir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoolForTest(t, dsn)
	ctx := context.Background()
	if err := Migrate(ctx, p.Raw(), migs); err == nil {
		t.Fatal("expected migration failure")
	}
	v, err := CurrentVersion(ctx, p.Raw())
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("failed version recorded: CurrentVersion = %d, want 1", v)
	}
	// Retryable: fix the migration and re-run to completion.
	writeMigrations(t, migDir, map[string]string{
		"00001_a.up.sql":   "CREATE TABLE IF NOT EXISTS a (id INT);",
		"00002_bad.up.sql": "CREATE TABLE IF NOT EXISTS bad (id INT);",
	})
	migs, _ = LoadMigrations(migDir)
	if err := Migrate(ctx, p.Raw(), migs); err != nil {
		t.Fatalf("re-run after fix: %v", err)
	}
	v, _ = CurrentVersion(ctx, p.Raw())
	if v != 2 {
		t.Fatalf("after fix version = %d, want 2", v)
	}
}

// T1.5.5 — Migration ordering deterministic: N+1 depends on N's schema and
// cannot execute before N, even with reverse on-disk file order.
func TestMigrationOrderingDeterministic(t *testing.T) {
	dsn := tempDB(t)
	migDir := t.TempDir()
	// 00003 references the table created by 00002 — ordering must be 1,2,3.
	writeMigrations(t, migDir, map[string]string{
		"00001_a.up.sql": "CREATE TABLE IF NOT EXISTS a (id INT);",
		"00002_b.up.sql": "CREATE TABLE IF NOT EXISTS b (id INT PRIMARY KEY);",
		"00003_c.up.sql": "CREATE TABLE IF NOT EXISTS c (id INT, b_id INT REFERENCES b(id));",
	})
	migs, err := LoadMigrations(migDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 3 || migs[0].Version != 1 || migs[1].Version != 2 || migs[2].Version != 3 {
		t.Fatalf("LoadMigrations order = %v", versions(migs))
	}
	p := newPoolForTest(t, dsn)
	if err := Migrate(context.Background(), p.Raw(), migs); err != nil {
		t.Fatalf("ordered Migrate failed (ordering broken?): %v", err)
	}
	if v, _ := CurrentVersion(context.Background(), p.Raw()); v != 3 {
		t.Fatalf("version = %d, want 3", v)
	}
}

func versions(ms []Migration) []int {
	out := []int{}
	for _, m := range ms {
		out = append(out, m.Version)
	}
	return out
}
