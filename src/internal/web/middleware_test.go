package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/logging"
)

func testLog(t *testing.T) (*logging.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	lg, err := logging.New(config.LoggingConfig{Level: "debug", Format: "json"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return lg, &buf
}

func withMW(h http.Handler, lg *logging.Logger) http.Handler {
	h = RequestLog(lg)(h)
	h = Recovery(lg)(h)
	h = RequestID(h)
	return h
}

// T1.9.1 — Requests without an ID receive one.
func TestRequestIDGenerated(t *testing.T) {
	lg, _ := testLog(t)
	var seen string
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}), lg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if seen == "" {
		t.Fatal("no request ID in context")
	}
	if rec.Header().Get("X-Request-ID") != seen {
		t.Errorf("response header %q != context %q", rec.Header().Get("X-Request-ID"), seen)
	}
}

// T1.9.2 — Existing valid request ID propagated; invalid replaced.
func TestRequestIDPolicy(t *testing.T) {
	lg, _ := testLog(t)
	var seen string
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}), lg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "valid-id-123")
	h.ServeHTTP(rec, req)
	if seen != "valid-id-123" {
		t.Errorf("valid ID not propagated: %q", seen)
	}

	seen = ""
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "bad id with spaces!?!")
	h.ServeHTTP(rec, req)
	if seen == "bad id with spaces!?!" {
		t.Error("invalid ID was not replaced")
	}
	if seen == "" {
		t.Fatal("no replacement ID generated")
	}
}

// T1.9.3 — Request logging includes the request ID.
func TestRequestLogIncludesID(t *testing.T) {
	lg, buf := testLog(t)
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), lg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "log-check-id")
	h.ServeHTTP(rec, req)
	if !strings.Contains(buf.String(), "log-check-id") {
		t.Fatalf("request log missing request ID: %s", buf.String())
	}
}

// T1.9.4 — Panic recovered: HTTP 500, handler chain survives.
func TestPanicRecovered(t *testing.T) {
	lg, _ := testLog(t)
	var calls int
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			panic("boom")
		}
	}), lg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500", rec.Code)
	}
	// Server survives: the next request (handler no longer panics) is handled normally.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("post-panic request = %d, want 200", rec2.Code)
	}
}

// T1.9.5 — Panic logged with request context.
func TestPanicLoggedWithContext(t *testing.T) {
	lg, buf := testLog(t)
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}), lg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "panic-req-id")
	h.ServeHTTP(rec, req)
	out := buf.String()
	if !strings.Contains(out, "kaboom") {
		t.Errorf("panic value not logged: %s", out)
	}
	if !strings.Contains(out, "panic-req-id") {
		t.Errorf("panic log missing request ID: %s", out)
	}
}

// T1.9.6 — Request duration recorded.
func TestRequestDurationRecorded(t *testing.T) {
	lg, buf := testLog(t)
	h := withMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}), lg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec2 map[string]any
		if err := json.Unmarshal([]byte(line), &rec2); err != nil {
			continue
		}
		if d, ok := rec2["duration_ms"].(float64); ok && d >= 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("duration not recorded: %s", buf.String())
	}
}

var _ = io.Discard
var _ = context.Background
