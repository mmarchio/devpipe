package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"overseer/internal/logging"
)

// requestIDHeader is the header carrying the request ID.
const requestIDHeader = "X-Request-ID"

// validRequestID is the accepted charset for a propagated request ID:
// alphanumerics, dash, underscore, dot. Anything else is replaced.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// RequestID middleware assigns a request ID to every request: it propagates
// a valid incoming X-Request-ID, or generates one when absent/invalid
// (deterministic policy, T1.9.1/T1.9.2). The ID is stored in the request
// context and echoed in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = generateRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func generateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return time.Now().UTC().Format("20060102150405") + hex.EncodeToString(b[:2])
	}
	return hex.EncodeToString(b)
}

// Recovery middleware recovers from handler panics, returning a 500 JSON
// error (T1.9.4) and logging the panic with request context (T1.9.5). The
// server (handler chain) survives to serve subsequent requests.
func Recovery(lg *logging.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if lg != nil {
						// The stack goes to the log only — never to the client
						// (WBS 18.196).
						lg.WithComponent("http").With("request_id", RequestIDFromContext(r.Context())).
							Error("recovered from panic", "panic", rec, "stack", string(debug.Stack()))
					}
					// The response may be partially written; best-effort 500.
					WriteError(w, r, NewInternalError("internal error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer to http.ResponseController
// (flush / per-request deadlines for SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RequestLog middleware emits a structured log record per request carrying
// method, path, status, duration, and the request ID (T1.9.3/T1.9.6).
func RequestLog(lg *logging.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if lg != nil {
				lg.WithComponent("http").With("request_id", RequestIDFromContext(r.Context())).Info("http request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", rec.status,
					"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
				)
			}
		})
	}
}

// ApplyMiddleware composes the standard middleware stack around h:
// RequestLog(Recovery(RequestID(h))).
func ApplyMiddleware(h http.Handler, lg *logging.Logger) http.Handler {
	h = RequestLog(lg)(h)
	h = Recovery(lg)(h)
	h = RequestID(h)
	return h
}
