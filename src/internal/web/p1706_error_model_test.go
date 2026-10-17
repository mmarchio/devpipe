package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"overseer/internal/service"
)

// errorMappers lists every resource-level domain-error mapper of the API
// (WBS 17.131: all handlers map domain errors consistently).
var errorMappers = map[string]func(error) error{
	"assignment": assignmentServiceError, "history": historyServiceError, "compression": compressionServiceError,
	"continuation": continuationError, "phase": phaseServiceError, "readiness": readinessServiceError,
	"lifecycle": lifecycleServiceError, "execution": executionError, "contextSnapshot": contextSnapshotServiceError,
	"transitionPreview": transitionPreviewServiceError, "transitionConfig": transitionConfigServiceError,
	"wbs": wbsServiceError, "currentWBS": currentWBSServiceError, "machine": machineServiceError,
	"promptPreview": promptPreviewServiceError, "rolePrompt": rolePromptServiceError, "role": roleServiceError,
	"promptCache": promptCacheError, "instruction": instructionServiceError, "provenance": provenanceServiceError,
	"summary": summaryServiceError, "runtimeState": runtimeStateServiceError, "project": serviceError,
	"settings": settingsServiceError, "stateFile": stateFileServiceError, "materialize": matServiceError,
	"precompute": precomputeError, "engine": engineError, "stateConfig": stateConfigServiceError,
	"result": resultError, "worker": workerError,
}

// P17-06 (T17.131) — the service error set maps to the same status classes
// for every resource; unknown / infrastructure errors never leak detail.
func TestP1706ConsistentDomainErrorMapping(t *testing.T) {
	secret := errors.New("pq: password=hunter2 dial tcp 10.0.0.5")
	cases := []struct {
		in   error
		want int
	}{
		{fmt.Errorf("%w: bad", service.ErrInvalidInput), http.StatusBadRequest},
		{fmt.Errorf("%w: gone", service.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("%w: stale", service.ErrConflict), http.StatusConflict},
		{fmt.Errorf("%w: %w", service.ErrInfrastructure, secret), http.StatusInternalServerError},
		{secret, http.StatusInternalServerError},
	}
	for name, m := range errorMappers {
		for _, c := range cases {
			got := m(c.in)
			want := c.want
			// An invalid lifecycle transition is a lifecycle conflict
			// (WBS 3.24 / 17.133): 409 by design.
			if name == "lifecycle" && errors.Is(c.in, service.ErrInvalidInput) {
				want = http.StatusConflict
			}
			if s := statusFor(got); s != want {
				t.Errorf("%s(%v) = %d, want %d", name, c.in, s, want)
			}
			rec := httptest.NewRecorder()
			WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), got)
			if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Errorf("%s leaked infrastructure detail: %s", name, rec.Body.String())
			}
		}
	}
}

// P17-06 (T17.131 / T17.134) — every error body carries code, message and a
// deterministic details list mirroring the field errors.
func TestP1706ErrorEnvelopeDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodPost, "/", nil),
		NewValidationFields("invalid_settings", "Settings are invalid.", map[string]string{"worker_model": "required", "context_window": "must be positive"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details []ErrorDetail     `json:"details"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "invalid_settings" || len(body.Error.Details) != 2 || len(body.Error.Fields) != 2 {
		t.Fatalf("body %s", rec.Body.String())
	}
	if body.Error.Details[0].Field != "context_window" || body.Error.Details[1].Field != "worker_model" {
		t.Fatalf("details not sorted: %+v", body.Error.Details)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), NewNotFoundError("project"))
	if !strings.Contains(rec.Body.String(), `"details":[]`) {
		t.Fatalf("details must always be present: %s", rec.Body.String())
	}
}

// P17-06 — Phase 17 boundary errors map to their statuses and codes.
func TestP1706BoundaryErrors(t *testing.T) {
	cases := []struct {
		err  error
		code string
		want int
	}{
		{NewUnauthenticatedError(), CodeUnauthenticated, 401},
		{NewForbiddenError(), CodeForbidden, 403},
		{NewCSRFError(), CodeCSRFInvalid, 403},
		{NewTooLargeError(), CodeTooLarge, 413},
		{NewRateLimitedError(), CodeRateLimited, 429},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), c.err)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), `"code":"`+c.code+`"`) {
			t.Fatalf("%v: %d %s", c.err, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), NewRateLimitedError())
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}
