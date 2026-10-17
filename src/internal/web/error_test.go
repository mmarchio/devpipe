package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doRequest(t *testing.T, h http.Handler, method, path string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, rec.Body.String())
	}
	return body
}

// T1.11.1 — Validation error mapped correctly.
func TestValidationErrorMapped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewValidationError("port out of range"))
	})
	rec := doRequest(t, h, "POST", "/api/v1/things", context.Background())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := decodeBody(t, rec)
	if !strings.Contains(fmt.Sprint(body["error"]), "port out of range") {
		t.Errorf("error field = %v", body["error"])
	}
}

// T1.11.2 — Not-found mapped to HTTP 404.
func TestNotFoundMapped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewNotFoundError("project"))
	})
	rec := doRequest(t, h, "GET", "/api/v1/projects/missing", context.Background())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// T1.11.3 — Conflict mapped to HTTP 409.
func TestConflictMapped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewConflictError("name taken"))
	})
	rec := doRequest(t, h, "POST", "/api/v1/things", context.Background())
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

// T1.11.4 — Internal error mapped to HTTP 500.
func TestInternalMapped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewInternalError("unexpected state"))
	})
	rec := doRequest(t, h, "GET", "/api/v1/things", context.Background())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// T1.11.5 — Infrastructure details hidden: a raw SQL/pgx-style error is
// logged internally (carried on the error) but not returned to clients.
func TestInfrastructureDetailsHidden(t *testing.T) {
	raw := errors.New(`pq: password authentication failed for user "overseer" (conn: postgres://overseer:***@dbhost:5432/overseer)`)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewInfrastructureError(raw))
	})
	rec := doRequest(t, h, "GET", "/api/v1/things", context.Background())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	out := rec.Body.String()
	for _, secret := range []string{"pq:", "password", "dbhost", "postgres://"} {
		if strings.Contains(out, secret) {
			t.Errorf("response leaked infrastructure detail %q: %s", secret, out)
		}
	}
	// The internal error must still carry the raw detail for logging.
	var ie *infrastructureError
	if !errors.As(NewInfrastructureError(raw), &ie) || !strings.Contains(ie.Error(), "pq:") {
		t.Error("internal error lost the raw detail")
	}
}

// T1.11.6 — Error response carries request ID.
func TestErrorResponseCarriesRequestID(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewNotFoundError("thing"))
	})
	rec := doRequest(t, h, "GET", "/api/v1/thingz", WithRequestID(context.Background(), "req-abc-123"))
	body := decodeBody(t, rec)
	if body["request_id"] != "req-abc-123" {
		t.Fatalf("request_id = %v", body["request_id"])
	}
}

// errors.Is compatibility: typed errors are detectable through wrapping.
func TestErrorsIsCompatibility(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", NewConflictError("x"))
	if !errors.Is(wrapped, ErrConflict) {
		t.Error("errors.Is should detect ErrConflict through wrapping")
	}
	if !errors.Is(NewNotFoundError("x"), ErrNotFound) {
		t.Error("ErrNotFound sentinel not detectable")
	}
	if !errors.Is(NewValidationError("x"), ErrValidation) {
		t.Error("ErrValidation sentinel not detectable")
	}
}

var _ io.Reader = (*strings.Reader)(nil)
