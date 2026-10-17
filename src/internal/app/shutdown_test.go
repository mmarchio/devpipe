package app

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/web"
)

func shutdownCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := validCfg(t)
	port := freePort(t)
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = port
	return cfg
}

// T1.12.1 — SIGTERM initiates graceful shutdown.
func TestShutdownSignal(t *testing.T) {
	cfg := shutdownCfg(t)
	cfg.Shutdown.Timeout = 5 * time.Second
	a := New(cfg)
	ctx, stopSignals := signalCtx(t)
	defer stopSignals()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Start(ctx); err != nil {
			t.Logf("Start: %v", err)
			return
		}
		<-ctx.Done()
		_ = a.Shutdown(context.Background())
	}()
	waitReady(t, a, 5*time.Second)
	// Send SIGTERM to this process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("app did not shut down on SIGTERM")
	}
	if a.IsReady() {
		t.Error("ready after shutdown")
	}
}

// T1.12.2 — Readiness removed during shutdown.
func TestReadinessRemovedDuringShutdown(t *testing.T) {
	cfg := shutdownCfg(t)
	a := New(cfg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.IsReady() {
		t.Fatal("not ready before shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if a.IsReady() {
		t.Error("ready state not removed during shutdown")
	}
}

// T1.12.5 — Database closes after shutdown.
func TestDatabaseClosesAfterShutdown(t *testing.T) {
	cfg := shutdownCfg(t)
	a := New(cfg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.DB() == nil {
		t.Fatal("DB nil before shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if a.DB() == nil {
		t.Fatal("DB reference lost (should be a closed pool)")
	}
	if ok, _ := a.DB().Ping(context.Background()); ok {
		t.Error("database still open after shutdown")
	}
}

// T1.12.3 — In-flight HTTP request allowed to finish within timeout.
func TestInFlightRequestFinishes(t *testing.T) {
	srv, base := slowServer(t, 200*time.Millisecond)
	defer stopSrv(t, srv)
	req, _ := http.NewRequest("GET", base+"/", nil)
	client := &http.Client{Timeout: 3 * time.Second}
	inflight := make(chan int, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			inflight <- 0
			return
		}
		resp.Body.Close()
		inflight <- 1
	}()
	time.Sleep(50 * time.Millisecond) // let the request start in-flight
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case code := <-inflight:
		if code != 1 {
			t.Fatal("in-flight request did not complete")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("in-flight request did not finish in time")
	}
}

// T1.12.4 — New work rejected during shutdown.
func TestNewWorkRejectedDuringShutdown(t *testing.T) {
	srv, base := slowServer(t, 800*time.Millisecond)
	defer stopSrv(t, srv)
	req, _ := http.NewRequest("GET", base+"/", nil)
	client := &http.Client{Timeout: 3 * time.Second}
	go client.Do(req)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = srv.Stop(ctx) }()
	time.Sleep(50 * time.Millisecond) // shutdown in progress, draining
	req2, _ := http.NewRequest("GET", base+"/", nil)
	resp, err := client.Do(req2)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("new request succeeded during shutdown")
		}
	}
}

// T1.12.6 — Shutdown timeout enforced: a stuck component cannot block exit.
func TestShutdownTimeoutEnforced(t *testing.T) {
	srv, base := slowServer(t, 5*time.Second) // stuck in-flight request
	defer stopSrv(t, srv)
	req, _ := http.NewRequest("GET", base+"/", nil)
	client := &http.Client{Timeout: 8 * time.Second}
	go client.Do(req)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = srv.Stop(ctx)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop did not respect timeout: %v", elapsed)
	}
}

// slowServer returns a running web.Server (on an ephemeral port) with a
// handler that sleeps dur, plus its bound address.
func slowServer(t *testing.T, dur time.Duration) (*web.Server, string) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(dur)
		w.WriteHeader(http.StatusOK)
	})
	srv := web.NewServer(web.DefaultServerConfig(), h)
	go func() { _ = srv.Start(context.Background(), "127.0.0.1:0") }()
	waitBind(t, srv)
	return srv, "http://" + srv.BoundAddr()
}

func stopSrv(t *testing.T, srv *web.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
}

func waitReady(t *testing.T, a *App, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if a.IsReady() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("app never became ready")
}

func waitBind(t *testing.T, srv *web.Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.BoundAddr() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never bound")
}

func signalCtx(t *testing.T) (context.Context, func()) {
	t.Helper()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	return ctx, stop
}
