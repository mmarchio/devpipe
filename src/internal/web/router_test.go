package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper to create a real Server (with timeouts) that can be stopped.
func startTestServer(t *testing.T, handler http.Handler) (baseURL string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return "http://" + ln.Addr().String(), stop
}

// T1.7.1 — Root endpoint responds 200.
func TestRootEndpointResponds(t *testing.T) {
	h := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}

// T1.7.2 — Unknown route returns 404.
func TestUnknownRoute404(t *testing.T) {
	h := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope/never", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", rec.Code)
	}
}

// T1.7.3 — Unsupported method handled correctly (405).
func TestUnsupportedMethod405(t *testing.T) {
	h := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
}

// T1.7.4 — Server timeouts configured (non-zero).
func TestServerTimeoutsConfigured(t *testing.T) {
	cfg := DefaultServerConfig()
	if cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.ReadHeaderTimeout <= 0 {
		t.Fatalf("default timeouts not all finite: %+v", cfg)
	}
}

// T1.7.5 — API namespace available: /api/v1/ registration coexists with web routes.
func TestAPINamespaceAvailable(t *testing.T) {
	var apiHit bool
	router := NewRouter(RouterConfig{})
	if err := router.RegisterAPI("GET", "/api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
		_, _ = w.Write([]byte("pong"))
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/ping", nil))
	if !apiHit || rec.Code != http.StatusOK {
		t.Fatalf("api route not hit: hit=%v code=%d", apiHit, rec.Code)
	}
	// web route still works (no conflict)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, httptest.NewRequest("GET", "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("root broken after api registration: %d", rec2.Code)
	}
}

// T1.7.6 — Static namespace serves a file.
func TestStaticNamespaceServes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("static-body"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewRouter(RouterConfig{StaticDir: dir})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/a.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "static-body" {
		t.Fatalf("static = %d body=%q, want 200 static-body", rec.Code, rec.Body.String())
	}
}

// /health stays 200 (process liveness); /ready is 503 until initialized
// (P1-09: readiness is dependency-backed per WBS 1.8, placeholder retired).
func TestHealthReadyLifecycle(t *testing.T) {
	h := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200", rec.Code)
	}
	h.SetReady(true)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/ready", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /ready after SetReady = %d, want 200", rec2.Code)
	}
}
