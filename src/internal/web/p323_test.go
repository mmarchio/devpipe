package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// T3.23.4 — Lifecycle conflict returns 409 with the consistent structured
// shape. A conflict sentinel driven through the central writer.
func TestT3234LifecycleConflict409Structured(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewConflictError("invalid lifecycle transition"))
	})
	rec := doRequest(t, h, "POST", "/api/v1/projects/x/start", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, rec.Body.String())
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no structured error object: %s", rec.Body.String())
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatalf("error object missing string code: %v", errObj)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error object missing string message: %v", errObj)
	}
	if _, ok := out["request_id"].(string); !ok {
		t.Fatalf("missing request_id: %v", out["request_id"])
	}
}

// T3.23.5 — Internal error returns 500 without implementation details: the
// central writer carries the structured shape and never a raw driver/SQL/DSN
// fragment.
func TestT3235Internal500Redacted(t *testing.T) {
	raw := errors.New(`pq: password authentication failed for user "overseer" (conn: postgres://overseer:***@dbhost:5432/overseer)`)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NewInfrastructureError(raw))
	})
	rec := doRequest(t, h, "GET", "/api/v1/things", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	out := rec.Body.String()
	for _, secret := range []string{"pq:", "password", "dbhost", "postgres://", "SQLSTATE"} {
		if strings.Contains(out, secret) {
			t.Fatalf("response leaked implementation detail %q: %s", secret, out)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, out)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no structured error object: %s", out)
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatalf("error object missing string code: %v", errObj)
	}
	if _, ok := body["request_id"].(string); !ok {
		t.Fatalf("missing request_id: %v", body["request_id"])
	}
}

var _ io.Reader = (*strings.Reader)(nil)
