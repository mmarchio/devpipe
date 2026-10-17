package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// P3-23 — centralized API request validation + structured errors (WBS 3.23).

// getErrorBody fetches an error response and returns its decoded body, the
// "error" object, and the "request_id" value.
func getErrorBody(t *testing.T, method, url, body string) (int, map[string]any, map[string]any, any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("non-JSON body %q", raw)
		}
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no structured error object: %s", raw)
	}
	return resp.StatusCode, out, errObj, out["request_id"]
}

// T3.23.1 — Malformed JSON returns 400 with the consistent structured shape
// across resources.
func TestT3231MalformedJSON400Structured(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p323-malformed", "")
	status, _, errObj, reqID := getErrorBody(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/settings", `{not json`)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status = %d, want 400", status)
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatalf("error object missing string code: %v", errObj)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error object missing string message: %v", errObj)
	}
	if _, ok := reqID.(string); !ok {
		t.Fatalf("missing request_id: %v", reqID)
	}
}

// T3.23.2 — Unknown/unsupported fields are rejected per the strict policy
// (400), consistent across resources.
func TestT3232UnknownFieldsRejected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p323-unknown", "")
	status, _, errObj, _ := getErrorBody(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		settingsBody(map[string]any{"bogus": "1"}))
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", status)
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatalf("error object missing string code: %v", errObj)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error object missing string message: %v", errObj)
	}
}

// T3.23.3 — Missing required field returns a validation response: 400 with a
// fields map naming the offending field.
func TestT3233MissingFieldFieldsMap(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p323-missing", "")
	status, _, errObj, _ := getErrorBody(t, http.MethodPut,
		h.BaseURL+"/api/v1/projects/"+created.ID+"/settings",
		`{"working_directory":"/x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("missing field: status = %d, want 400", status)
	}
	fields, ok := errObj["fields"].(map[string]any)
	if !ok {
		t.Fatalf("error object has no fields map: %v", errObj)
	}
	if len(fields) == 0 {
		t.Fatalf("fields map is empty: %v", fields)
	}
	for k, v := range fields {
		if _, ok := v.(string); !ok {
			t.Fatalf("field %q has no message: %v", k, v)
		}
	}
}
