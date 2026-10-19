package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/app"
	"overseer/internal/config"
)

// Harness is a reusable integration-test instance: a temporary PostgreSQL
// database, the full application started against it (migrations + HTTP), and
// accessors for both. It is the shared artifact for T1.15.x and later phases.
// It skips the calling test when the base PostgreSQL is unreachable.
type Harness struct {
	App     *app.App
	BaseURL string
	t       *testing.T

	dbName  string
	port    int
	baseDSN string
	keepDB  bool // when true, Close() skips dropping the database
}

// NewHarness creates a temporary database, starts the application against it,
// and registers cleanup (app stop + temp DB drop). It skips the test when the
// base PostgreSQL is unreachable.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	return NewHarnessWith(t, nil)
}

// NewHarnessWith is NewHarness with a configuration hook applied before the
// application starts (Phase 9: compression providers pointed at a fake).
func NewHarnessWith(t *testing.T, mutate func(*config.Config)) *Harness {
	t.Helper()
	base := baseDSN()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v); set OVERSEER_TEST_PG_DSN", err)
	}
	name := fmt.Sprintf("overseer_it_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		conn.Close(ctx)
		t.Skipf("create temp db: %v", err)
	}
	conn.Close(ctx)

	// Point the app at the temporary database, reusing base credentials.
	host, port, user, pass, ssl := parseDSNFields(t, base)
	cfg := config.Default()
	cfg.Database.Host = host
	cfg.Database.Port = port
	cfg.Database.Name = name
	cfg.Database.User = user
	cfg.Database.Password = pass
	cfg.Database.SSLMode = ssl
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = freePort(t)
	// Phase 16: the engine scheduler only runs in tests that opt in.
	cfg.Engine.SchedulerDisabled = true
	if mutate != nil {
		mutate(&cfg)
	}

	a := newHarnessApp(cfg)
	if err := a.Start(ctx); err != nil {
		dropDB(t, base, name)
		t.Fatalf("harness start: %v", err)
	}

	h := &Harness{
		App:     a,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		t:       t,
		dbName:  name,
		port:    cfg.Server.Port,
		baseDSN: base,
	}
	t.Cleanup(h.Close)
	return h
}

// NewHarnessOn starts a harness against an existing database (created by the
// caller, who owns its lifecycle). Used for restart and graceful-restart
// acceptance tests.
func NewHarnessOn(t *testing.T, base, dbName string) *Harness {
	t.Helper()
	return NewHarnessOnWith(t, base, dbName, nil)
}

// NewHarnessOnWith is NewHarnessOn with a configuration hook.
func NewHarnessOnWith(t *testing.T, base, dbName string, mutate func(*config.Config)) *Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	conn.Close(ctx)

	host, port, user, pass, ssl := parseDSNFields(t, base)
	cfg := config.Default()
	cfg.Database.Host = host
	cfg.Database.Port = port
	cfg.Database.Name = dbName
	cfg.Database.User = user
	cfg.Database.Password = pass
	cfg.Database.SSLMode = ssl
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = freePort(t)
	// Phase 16: the engine scheduler only runs in tests that opt in.
	cfg.Engine.SchedulerDisabled = true
	if mutate != nil {
		mutate(&cfg)
	}

	a := newHarnessApp(cfg)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("harness start on %s: %v", dbName, err)
	}
	return &Harness{
		App:     a,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		t:       t,
		dbName:  dbName,
		baseDSN: base,
		keepDB:  true,
	}
}

// NewAppWithDBName builds a stopped App pointed at the named database (which
// need not exist). Used for fail-at-startup tests; the caller starts it.
func NewAppWithDBName(t *testing.T, dbName string) *app.App {
	t.Helper()
	base := baseDSN()
	host, port, user, pass, ssl := parseDSNFields(t, base)
	cfg := config.Default()
	cfg.Database.Host = host
	cfg.Database.Port = port
	cfg.Database.Name = dbName
	cfg.Database.User = user
	cfg.Database.Password = pass
	cfg.Database.SSLMode = ssl
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = freePort(t)
	return newHarnessApp(cfg)
}

// Close stops the application and drops the temporary database. Idempotent.
func (h *Harness) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if h.App != nil {
		_ = h.App.Shutdown(ctx)
		h.App = nil
	}
	if !h.keepDB {
		dropDB(h.t, h.baseDSN, h.dbName)
	}
}

// DBName returns the temporary database name.
func (h *Harness) DBName() string { return h.dbName }

// Port returns the HTTP listen port.
func (h *Harness) Port() int { return h.port }

// DBScan runs a query against the temporary database and scans the first row
// into dest. The connection is fully managed by the harness.
func (h *Harness) DBScan(ctx context.Context, query string, dest ...any) error {
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	return conn.QueryRow(ctx, query).Scan(dest...)
}

// DBExec runs a statement against the temporary database.
func (h *Harness) DBExec(ctx context.Context, query string) (pgconn.CommandTag, error) {
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	return conn.Exec(ctx, query)
}

func (h *Harness) connect(ctx context.Context) *pgx.Conn {
	conn, err := pgx.Connect(ctx, replaceDBName(h.baseDSN, h.dbName))
	if err != nil {
		h.t.Fatalf("harness connect: %v", err)
	}
	return conn
}

// --- helpers -------------------------------------------------------------

func baseDSN() string {
	if v := os.Getenv("OVERSEER_TEST_PG_DSN"); v != "" {
		return v
	}
	return "postgres://pguser:***@172.17.0.1:5432/devpipe?sslmode=disable"
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func dropDB(t *testing.T, base, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	// Test cleanups run LIFO, so this may fire before the harness app is
	// stopped — terminate the app's pooled backends first or the DROP
	// fails with a still-active connection and leaks the database.
	_, _ = c.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
	_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
}

// replaceDBName swaps the database path segment in a postgres DSN.
func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// parseDSNFields extracts host, port, user, password, and sslmode from a
// postgres URL-style DSN.
func parseDSNFields(t *testing.T, dsn string) (string, int, string, string, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	ssl := u.Query().Get("sslmode")
	if ssl == "" {
		ssl = "disable"
	}
	return u.Hostname(), port, user, pass, ssl
}

// harnessLogWriter, when set by a test, receives every harness app's
// structured log (secret-scanning tests, WBS 18.75). Tests run
// sequentially; set it with captureLogs.
var harnessLogWriter io.Writer

// harnessConfigPath, when set by a test, is the configuration file the
// harness app's System / Models categories edit. Unset, edits fail: a
// test never touches the developer's OVERSEER_CONFIG_PATH.
var harnessConfigPath string

func newHarnessApp(cfg config.Config) *app.App {
	opts := app.DefaultOptions()
	opts.LogWriter = harnessLogWriter
	opts.ConfigPath = harnessConfigPath
	return app.NewWith(cfg, opts)
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs captures harness app logs for the rest of the test.
func captureLogs(t *testing.T) *lockedBuffer {
	buf := &lockedBuffer{}
	harnessLogWriter = buf
	t.Cleanup(func() { harnessLogWriter = nil })
	return buf
}
