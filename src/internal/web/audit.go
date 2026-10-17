package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/logging"
	"overseer/internal/web/middleware"
)

// AuditRecorder persists one audit event (implemented by
// service.AuditService).
type AuditRecorder interface {
	RecordAudit(ctx context.Context, e audit.Event) error
}

// ReasonHeader carries the human reason of an API mutation (WBS 17.100).
const ReasonHeader = "X-Overseer-Reason"

// auditStatus captures the response status.
type auditStatus struct {
	http.ResponseWriter
	status int
}

func (a *auditStatus) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditStatus) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (a *auditStatus) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// Flush supports streaming handlers.
func (a *auditStatus) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type auditBagKey struct{}

type auditBag struct {
	mu sync.Mutex
	m  map[string]any
}

// AddAuditDetail attaches structured detail to the audit row of the
// current request (e.g. the recovery action, the evidence hash viewed and
// the result, WBS 18.182). It is a no-op outside an audited request. The
// detail passes through the audit redactor.
func AddAuditDetail(ctx context.Context, key string, value any) {
	if b, ok := ctx.Value(auditBagKey{}).(*auditBag); ok && b != nil {
		b.mu.Lock()
		b.m[key] = value
		b.mu.Unlock()
	}
}

// AuditMiddleware returns the router audit wrapper (P17-40): every
// mutation route records SUCCEEDED / REJECTED / FAILED after the handler,
// with the principal, request id, path resource ids and the human reason
// (form field `reason`, JSON field "reason" or the X-Overseer-Reason
// header). An audit write failure is logged at ERROR; it never changes the
// response.
func AuditMiddleware(rec AuditRecorder, lg *logging.Logger) func(action access.Action, pattern string, h http.Handler) http.Handler {
	return func(action access.Action, pattern string, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reason := requestReason(r)
			sw := &auditStatus{ResponseWriter: w}
			bag := &auditBag{m: map[string]any{}}
			r = r.WithContext(context.WithValue(r.Context(), auditBagKey{}, bag))
			h.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			p := middleware.PrincipalFromContext(r.Context())
			res := resourceFor(pattern, r.PathValue)
			e := audit.Event{
				At: time.Now().UTC(), Actor: audit.Actor{ID: p.ID, Name: p.Name, Type: p.Type}, Action: action,
				ResourceKind: res.Kind, ResourceID: res.ID, Result: audit.ResultForStatus(sw.status), HTTPStatus: sw.status,
				Reason: reason, RequestID: RequestIDFromContext(r.Context()), Method: r.Method, Path: r.URL.Path,
				Metadata: map[string]any{"route": r.Method + " " + pattern},
			}
			bag.mu.Lock()
			for k, v := range bag.m {
				if k != "route" {
					e.Metadata[k] = v
				}
			}
			bag.mu.Unlock()
			if e.RequestID == "" {
				e.RequestID = w.Header().Get(requestIDHeader)
			}
			if e.RequestID == "" {
				e.RequestID = uuid.NewString()
			}
			e.ProjectID = pathUUID(r, "project", "id")
			if e.ProjectID == nil && sw.status == http.StatusCreated {
				// A created project is identified by its Location.
				e.ProjectID = createdProject(w.Header().Get("Location"))
			}
			e.ExecutionID = pathUUID(r, "execution")
			e.PhaseID = pathUUID(r, "phaseID", "phase")
			e.WorkUnitID = pathUUID(r, "unit")
			// Detached context: the audit row is written even when the
			// client disconnected.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if err := rec.RecordAudit(ctx, e); err != nil && lg != nil {
				lg.WithComponent("audit").Error("audit write failed", "request_id", e.RequestID, "action", string(action), "error", err.Error())
			}
		})
	}
}

// createdProject reads the project id of a "/api/v1/projects/{id}"
// Location header.
func createdProject(loc string) *uuid.UUID {
	rest, ok := strings.CutPrefix(loc, "/api/v1/projects/")
	if !ok {
		return nil
	}
	if id, err := uuid.Parse(rest); err == nil {
		return &id
	}
	return nil
}

func pathUUID(r *http.Request, names ...string) *uuid.UUID {
	for _, n := range names {
		if v := r.PathValue(n); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				return &id
			}
		}
	}
	return nil
}

// requestReason extracts the human reason without consuming the body.
func requestReason(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(ReasonHeader)); v != "" {
		return clip(v)
	}
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mt {
	case "application/json":
		b, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		if err != nil {
			return ""
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(b, &body)
		return clip(strings.TrimSpace(body.Reason))
	case "application/x-www-form-urlencoded", "multipart/form-data":
		// The CSRF middleware may already have parsed the form; ParseForm
		// is idempotent and leaves PostForm for the handler.
		_ = r.ParseMultipartForm(1 << 20)
		return clip(strings.TrimSpace(r.PostFormValue("reason")))
	}
	return ""
}

func clip(s string) string {
	if len(s) > audit.MaxReason {
		return s[:audit.MaxReason]
	}
	return s
}
