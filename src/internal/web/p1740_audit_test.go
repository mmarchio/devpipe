package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"overseer/internal/access"
	"overseer/internal/audit"
)

type fakeAudit struct{ events []audit.Event }

func (f *fakeAudit) RecordAudit(_ context.Context, e audit.Event) error {
	f.events = append(f.events, e)
	return e.Validate()
}

// P17-40 (WBS 17.106 / 17.100) — mutations are audited with actor, request
// id, resource and reason; reads are not; the handler still sees the body.
func TestP1740AuditMiddleware(t *testing.T) {
	rec := &fakeAudit{}
	r := NewRouter(RouterConfig{})
	r.SetAudit(AuditMiddleware(rec, nil))
	var body string
	_ = r.RegisterAPI(http.MethodPost, "/api/v1/projects/{id}/pause", func(w http.ResponseWriter, req *http.Request) {
		b := make([]byte, 100)
		n, _ := req.Body.Read(b)
		body = string(b[:n])
		w.WriteHeader(http.StatusOK)
	})
	_ = r.RegisterAPI(http.MethodPost, "/api/v1/projects/{id}/resume", func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, NewConflictError("invalid lifecycle transition"))
	})
	_ = r.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}", func(w http.ResponseWriter, req *http.Request) {})
	pid := "6f1c1d1e-1111-4222-8333-444455556666"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+pid+"/pause", strings.NewReader(`{"reason":"maintenance window"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithRequestID(req.Context(), "req-1"))
	r.ServeHTTP(httptest.NewRecorder(), req)
	if body != `{"reason":"maintenance window"}` {
		t.Fatalf("handler body %q", body)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+pid+"/resume", nil)
	req.Header.Set(ReasonHeader, "retry")
	r.ServeHTTP(httptest.NewRecorder(), req)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+pid, nil))
	if len(rec.events) != 2 {
		t.Fatalf("events %d", len(rec.events))
	}
	e := rec.events[0]
	if e.Action != access.ActionLifecyclePause || e.Result != audit.ResultSucceeded || e.Reason != "maintenance window" ||
		e.RequestID != "req-1" || e.ProjectID == nil || e.ProjectID.String() != pid || e.Actor.Type != access.PrincipalLocalAdmin ||
		e.ResourceKind != "pause" || e.Validate() != nil {
		t.Fatalf("event: %+v", e)
	}
	e = rec.events[1]
	if e.Result != audit.ResultRejected || e.HTTPStatus != 409 || e.Reason != "retry" || e.RequestID == "" {
		t.Fatalf("rejected event: %+v", e)
	}
}
