package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// T1.15.1 — Fresh installation: fresh database, application initializes.
func TestE2EFreshInstallation(t *testing.T) {
	h := NewHarness(t)
	if !h.App.IsReady() {
		t.Fatal("fresh install: app not ready")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, p := range []string{"/health", "/ready", "/"} {
		resp, err := client.Get(h.BaseURL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, resp.StatusCode)
		}
	}
}

// T1.15.2 — Existing installation: restart against a migrated database
// initializes without destructive changes.
func TestE2ERestartAgainstMigratedDB(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName) // first boot: migrates
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}
	// Seed a marker table to prove restart is non-destructive (a fake
	// schema_migrations version would trip the newer-schema guard, 18.134).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := h1.DBExec(ctx2, "CREATE TABLE restart_marker (id int); INSERT INTO restart_marker VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var markerCount int
	if err := h1.DBScan(ctx2, "SELECT count(*) FROM restart_marker", &markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("marker count = %d", markerCount)
	}
	h1.Close()

	// Second boot against the same (already migrated) database.
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	var after int
	if err := h2.DBScan(ctx2, "SELECT count(*) FROM restart_marker", &after); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("restart was destructive: marker rows = %d, want 1", after)
	}
}

// T1.15.3 — Database unavailable at startup: clean failure, no readiness.
func TestE2EDBUnavailableAtStartup(t *testing.T) {
	h := NewHarness(t)
	_ = h // keep harness alive for its DB; the failing app uses a bogus DB.
	a := NewAppWithDBName(t, "definitely_not_a_database_"+fmt.Sprint(time.Now().UnixNano()))
	if err := a.Start(context.Background()); err == nil {
		a.Shutdown(context.Background())
		t.Fatal("Start should fail when DB is unavailable")
	}
	if a.IsReady() {
		t.Fatal("app claims readiness without a database")
	}
}

// T1.15.4 — Database becomes unavailable after startup: health alive,
// readiness unavailable.
func TestE2EDBUnavailableAfterStartup(t *testing.T) {
	h := NewHarness(t)
	// Terminate all backend connections to the temporary database.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()", h.DBName()); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)

	client := &http.Client{Timeout: 5 * time.Second}
	time.Sleep(500 * time.Millisecond) // let the pool notice
	health, _ := client.Get(h.BaseURL + "/health")
	if health == nil || health.StatusCode != http.StatusOK {
		t.Fatalf("health = %v, want 200", healthStatusCode(health))
	}
	health.Body.Close()
	ready, err := client.Get(h.BaseURL + "/ready")
	if err != nil {
		t.Fatalf("GET /ready: %v", err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready = %d, want 503", ready.StatusCode)
	}
}

// T1.15.5 — Graceful restart: shutdown then restart restores operation.
func TestE2EGracefulRestart(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}
	h1.Close() // graceful shutdown: stops server + pool, drops nothing

	// A fresh instance boots against the same database and is ready.
	h2 := NewHarnessOn(t, base, dbName)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(h2.BaseURL + "/ready")
	if err != nil {
		t.Fatalf("after restart GET /ready: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after restart ready = %d, want 200", resp.StatusCode)
	}
}

func healthStatusCode(r *http.Response) int {
	if r == nil {
		return -1
	}
	return r.StatusCode
}
