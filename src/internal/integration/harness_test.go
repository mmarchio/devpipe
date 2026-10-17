package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func skipIfNoPG(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	c.Close(ctx)
}

// T1.13.1 — A complete test instance initializes against the test database.
func TestHarnessStarts(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	if !h.App.IsReady() {
		t.Fatal("harness app not ready")
	}
}

// T1.13.2 — The test database migrates.
func TestHarnessMigrates(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if version < 1 {
		t.Fatalf("migration version = %d, want >= 1", version)
	}
}

// T1.13.3 — HTTP endpoints reachable on the harness.
func TestHarnessHTTPReachable(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/health", "/ready"} {
		resp, err := client.Get(h.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
	}
}

// T1.13.4 — Test state isolated: data written by one instance is invisible
// to another.
func TestHarnessIsolation(t *testing.T) {
	skipIfNoPG(t)
	h1 := NewHarness(t)
	h2 := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Seed a marker row in h1's database (schema_migrations is created by the
	// real migration and is a per-DB table).
	if _, err := h1.DBExec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (9999, 'iso_marker')"); err != nil {
		t.Fatal(err)
	}
	var n1, n2 int
	if err := h1.DBScan(ctx, "SELECT count(*) FROM schema_migrations WHERE version=9999", &n1); err != nil {
		t.Fatal(err)
	}
	if err := h2.DBScan(ctx, "SELECT count(*) FROM schema_migrations WHERE version=9999", &n2); err != nil {
		t.Fatal(err)
	}
	if n1 != 1 {
		t.Fatalf("h1 row count = %d, want 1", n1)
	}
	if n2 != 0 {
		t.Fatalf("h2 sees h1 data: %d rows", n2)
	}
}

// T1.13.5 — Cleanup: no running server, no leaked pool, temp database dropped.
func TestHarnessCleanup(t *testing.T) {
	skipIfNoPG(t)
	h := NewHarness(t)
	dbName := h.DBName()
	port := h.Port()
	db := h.App.DB() // capture before Close nils the App
	h.Close()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d still bound after cleanup: %v", port, err)
	}
	ln.Close()

	if ok, _ := db.Ping(context.Background()); ok {
		t.Error("database pool not closed")
	}

	base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := pgx.Connect(base, baseDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(base)
	var n int
	if err := c.QueryRow(base, "SELECT count(*) FROM pg_database WHERE datname=$1", dbName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("temp database %s not dropped", dbName)
	}
}
