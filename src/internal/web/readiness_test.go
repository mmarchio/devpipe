package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// T1.8.1 — Health returns 200 after server startup, no checks required.
func TestHealth200OnStartup(t *testing.T) {
	r := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health = %d, want 200", rec.Code)
	}
}

// T1.8.2 — Health does not depend on dependencies: a failing check leaves
// /health at 200.
func TestHealthIndependentOfDependencies(t *testing.T) {
	r := NewRouter(RouterConfig{})
	r.SetReady(true)
	r.RegisterReadinessCheck("database", func(context.Context) (bool, error) {
		return false, context.DeadlineExceeded
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health = %d with failing check, want 200", rec.Code)
	}
}

// T1.8.3 — Ready succeeds when application initialized and dependencies available.
func TestReadyWhenDependenciesAvailable(t *testing.T) {
	r := NewRouter(RouterConfig{})
	r.SetReady(true)
	r.RegisterReadinessCheck("database", func(context.Context) (bool, error) { return true, nil })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ready = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// T1.8.4 — Ready fails (non-2xx) when the database is unavailable.
func TestReadyFailsWhenDatabaseUnavailable(t *testing.T) {
	r := NewRouter(RouterConfig{})
	r.SetReady(true)
	r.RegisterReadinessCheck("database", func(context.Context) (bool, error) {
		return false, context.DeadlineExceeded
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code == http.StatusOK || rec.Code < 200 || rec.Code > 299 {
		// expect non-2xx (503)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d, want 503", rec.Code)
	}
}

// T1.8.4b — Not initialized application is not ready either.
func TestReadyFailsWhenNotInitialized(t *testing.T) {
	r := NewRouter(RouterConfig{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d before SetReady, want 503", rec.Code)
	}
}

// T1.8.5 — Readiness is machine-readable and carries no sensitive detail.
func TestReadyMachineReadableNoSecrets(t *testing.T) {
	ready := NewRouter(RouterConfig{})
	ready.SetReady(true)
	ready.RegisterReadinessCheck("database", func(context.Context) (bool, error) { return true, nil })
	rec := httptest.NewRecorder()
	ready.ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ready body not JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("ready status = %v", body["status"])
	}

	notReady := NewRouter(RouterConfig{})
	notReady.SetReady(true)
	notReady.RegisterReadinessCheck("database", func(context.Context) (bool, error) {
		return false, context.DeadlineExceeded
	})
	rec2 := httptest.NewRecorder()
	notReady.ServeHTTP(rec2, httptest.NewRequest("GET", "/ready", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", rec2.Code)
	}
	var body2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("not-ready body not JSON: %v", err)
	}
	if body2["status"] != "not_ready" {
		t.Fatalf("not_ready status = %v", body2["status"])
	}
	checks, ok := body2["checks"].(map[string]any)
	if !ok || checks["database"] != "unavailable" {
		t.Fatalf("checks = %v, want database=unavailable", body2["checks"])
	}
	for _, secret := range []string{"password", "postgres://", "pq:"} {
		if strings.Contains(rec2.Body.String(), secret) {
			t.Errorf("not-ready body leaked %q: %s", secret, rec2.Body.String())
		}
	}
}
