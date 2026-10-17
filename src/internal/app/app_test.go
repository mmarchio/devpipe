package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"overseer/internal/config"
)

func validCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	dsn := os.Getenv("OVERSEER_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://pguser:***@172.17.0.1:5432/devpipe?sslmode=disable"
	}
	// Point the app at the test PG.
	cfg.Database.Host = "172.17.0.1"
	cfg.Database.Port = 5432
	cfg.Database.Name = "devpipe"
	cfg.Database.User = "pguser"
	cfg.Database.Password = "pguser"
	cfg.Database.SSLMode = "disable"
	if dsn != "" {
		_ = dsn
	}
	return cfg
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// T1.6.1 — Invalid DB config: Start fails, HTTP server never bound.
func TestStartupFailsOnInvalidDB(t *testing.T) {
	cfg := validCfg(t)
	cfg.Database.Host = "172.17.0.1"
	cfg.Database.Port = 1 // nothing listening
	port := freePort(t)
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	a := New(cfg)
	err := a.Start(context.Background())
	if err == nil {
		a.Stop(context.Background())
		t.Fatal("expected startup failure")
	}
	// HTTP never bound: the port is immediately re-bindable.
	ln, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatalf("port %d should be free after failed startup: %v", port, err)
	}
	ln.Close()
}

// T1.6.2 — Initialization failure stops startup; no ready state.
func TestStartupFailureStopsStartup(t *testing.T) {
	cfg := validCfg(t)
	cfg.Database.Port = 1
	port := freePort(t)
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	a := New(cfg)
	_ = a.Start(context.Background())
	if a.IsReady() {
		t.Fatal("app reports ready after failed startup")
	}
}

// T1.6.3 — DB initialized + migration ok + HTTP bind failure → DB resources closed.
func TestLaterFailureCleansUpEarlierResources(t *testing.T) {
	cfg := validCfg(t)
	// Occupy the target port so the HTTP bind fails after DB init.
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	port := occupant.Addr().(*net.TCPAddr).Port
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	a := New(cfg)
	err = a.Start(context.Background())
	if err == nil {
		a.Stop(context.Background())
		t.Fatal("expected bind failure")
	}
	// The DB pool must have been closed by cleanup.
	if a.DB() == nil {
		t.Fatal("expected DB to have been initialized before the bind failure")
	}
	ok, _ := a.DB().Ping(context.Background())
	if ok {
		t.Error("DB pool not closed after later startup failure")
	}
}

// T1.6.4 — Successful startup reaches ready state; all base endpoints respond.
func TestSuccessfulStartupReachesReady(t *testing.T) {
	cfg := validCfg(t)
	port := freePort(t)
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	a := New(cfg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop(context.Background())
	base := "http://127.0.0.1:" + itoa(port)
	if !a.IsReady() {
		t.Fatal("not ready after successful startup")
	}
	if st := getURL(t, base+"/health"); st != http.StatusOK {
		t.Errorf("/health = %d", st)
	}
	if st := getURL(t, base+"/ready"); st != http.StatusOK {
		t.Errorf("/ready = %d", st)
	}
	if st := getURL(t, base+"/"); st != http.StatusOK {
		t.Errorf("/ = %d", st)
	}
}

// T1.6.5 — Shutdown is idempotent.
func TestShutdownIdempotent(t *testing.T) {
	cfg := validCfg(t)
	port := freePort(t)
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	a := New(cfg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Stop panicked: %v", r)
			}
		}()
		_ = a.Stop(ctx)
	}()
	if a.IsReady() {
		t.Error("ready after stop")
	}
}

func getURL(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
