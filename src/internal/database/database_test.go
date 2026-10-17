package database

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/logging"
)

func testLogger(t *testing.T) *logging.Logger {
	lg, err := logging.New(config.LoggingConfig{Level: "info", Format: "json"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return lg
}

const defaultTestDSN = "postgres://pguser:***@172.17.0.1:5432/devpipe?sslmode=disable"

func testDSN(t *testing.T) string {
	t.Helper()
	return envOr("OVERSEER_TEST_PG_DSN", defaultTestDSN)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// requirePG skips the test (not fails) when the test PG is unreachable.
func requirePG(t *testing.T) string {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rawPing(ctx, dsn); err != nil {
		t.Skipf("test PG unreachable (%v); set OVERSEER_TEST_PG_DSN to override", err)
	}
	return dsn
}

// T1.4.1 — Valid PostgreSQL connection succeeds.
func TestNewSucceeds(t *testing.T) {
	dsn := requirePG(t)
	p, err := New(context.Background(), dsn, testLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	ok, err := p.Ping(context.Background())
	if !ok || err != nil {
		t.Fatalf("Ping = (%v, %v), want (true, nil)", ok, err)
	}
}

// T1.4.2 — Invalid credentials fail with an actionable error.
func TestInvalidCredentials(t *testing.T) {
	requirePG(t)
	bad := "postgres://pguser:***@172.17.0.1:5432/no_such_db?sslmode=disable"
	_, err := New(context.Background(), bad, testLogger(t))
	if err == nil {
		t.Fatal("expected error for invalid credentials")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "database") && !strings.Contains(msg, "password") && !strings.Contains(msg, "role") {
		t.Errorf("error should be actionable (identify the failure): %v", err)
	}
}

// T1.4.3 — Unavailable database fails; no partially initialized pool returned.
func TestUnavailableDatabase(t *testing.T) {
	requirePG(t)
	bad := "postgres://pguser:***@172.17.0.1:1/devpipe?sslmode=disable"
	p, err := New(context.Background(), bad, testLogger(t))
	if err == nil {
		if p != nil {
			p.Close()
		}
		t.Fatal("expected error for unavailable database")
	}
	if p != nil {
		t.Error("New returned a pool together with an error")
	}
}

// T1.4.4 — Creating a pool without successful connectivity is not success.
func TestPingReflectsRealConnectivity(t *testing.T) {
	dsn := requirePG(t)
	p, err := New(context.Background(), dsn, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := p.Ping(context.Background()); !ok || err != nil {
		t.Fatalf("Ping = (%v, %v), want (true, nil)", ok, err)
	}
	p.Close()
	if ok, err := p.Ping(context.Background()); ok || err == nil {
		t.Fatalf("Ping after Close = (%v, %v), want (false, err)", ok, err)
	}
}

// T1.4.5 — Connection closes cleanly; Close is idempotent.
func TestCloseIsCleanAndIdempotent(t *testing.T) {
	dsn := requirePG(t)
	p, err := New(context.Background(), dsn, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	var close func()
	close = p.Close
	close()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Close panicked: %v", r)
			}
		}()
		close()
	}()
}

// T1.4.6 — The domain-facing contract exposes no PostgreSQL-specific types.
// Compile-time assertion: *Pool satisfies Connector (Ping/Close only).
var _ Connector = (*Pool)(nil)

// The Connector signature must use only core types (context.Context, bool,
// string, error) so domain packages never see pgx/database/sql types.
func TestConnectorSignatureHasNoDriverTypes(t *testing.T) {
	typ := reflect.TypeOf((*Connector)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		sig := m.Type
		for j := 0; j < sig.NumIn(); j++ {
			arg := sig.In(j)
			if j == 0 { // receiver
				continue
			}
			if !isCoreType(arg) {
				t.Errorf("%s parameter %d uses non-core type %s", m.Name, j, arg)
			}
		}
		for j := 0; j < sig.NumOut(); j++ {
			out := sig.Out(j)
			if !isCoreType(out) {
				t.Errorf("%s result %d uses non-core type %s", m.Name, j, out)
			}
		}
	}
}

func isCoreType(ty reflect.Type) bool {
	switch ty.Kind() {
	case reflect.Bool, reflect.String:
		return true
	case reflect.Interface:
		return ty == reflect.TypeOf((*error)(nil)).Elem() ||
			ty == reflect.TypeOf((*context.Context)(nil)).Elem()
	default:
		return false
	}
}
