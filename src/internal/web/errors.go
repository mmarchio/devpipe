package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"overseer/internal/web/middleware"

	"overseer/internal/logging"
)

// Sentinel errors for deterministic classification (errors.Is/As support).
var (
	ErrValidation     = errors.New("validation error")
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrInfrastructure = errors.New("infrastructure error")
	ErrInternal       = errors.New("internal error")
	// ErrBadGateway: an upstream worker failed or is unreachable (502).
	ErrBadGateway = errors.New("bad gateway")
	// Phase 17 boundary errors (WBS 17.118–17.131).
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrTooLarge        = errors.New("request too large")
	ErrRateLimited     = errors.New("rate limited")
	// ErrUnavailable: the instance cannot serve the request now (503).
	ErrUnavailable = errors.New("service unavailable")
)

// NewBadGatewayCode is a 502 with a resource-specific code.
func NewBadGatewayCode(code, msg string) error {
	return &appError{sentinel: ErrBadGateway, code: code, msg: msg}
}

// Stable machine-readable error codes (WBS 3.23). Resource-specific codes
// (e.g. "invalid_project_settings") are defined at the resource boundary.
const (
	CodeInvalidInput     = "invalid_input"
	CodeMalformedJSON    = "malformed_json"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeInternal         = "internal_error"
	CodeMethodNotAllowed = "method_not_allowed"
	// Phase 17 boundary codes (WBS 17.118–17.131).
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeCSRFInvalid     = "CSRF_INVALID"
	CodeTooLarge        = "REQUEST_TOO_LARGE"
	CodeRateLimited     = "RATE_LIMITED"
)

// NewUnauthenticatedError is a 401: the request carries no acceptable
// credentials (WBS 17.126).
func NewUnauthenticatedError() error {
	return &appError{sentinel: ErrUnauthenticated, code: CodeUnauthenticated, msg: "authentication required"}
}

// NewForbiddenError is a 403: the principal may not perform the action
// (WBS 17.129).
func NewForbiddenError() error {
	return &appError{sentinel: ErrForbidden, code: CodeForbidden, msg: "not permitted"}
}

// NewCSRFError is a 403 for a missing / invalid CSRF token (WBS 17.118).
func NewCSRFError() error {
	return &appError{sentinel: ErrForbidden, code: CodeCSRFInvalid, msg: "missing or invalid CSRF token"}
}

// NewTooLargeError is a 413 (WBS 17.120).
func NewTooLargeError() error {
	return &appError{sentinel: ErrTooLarge, code: CodeTooLarge, msg: "request too large"}
}

// NewRateLimitedError is a 429 (WBS 17.154).
func NewRateLimitedError() error {
	return &appError{sentinel: ErrRateLimited, code: CodeRateLimited, msg: "too many requests"}
}

type appError struct {
	sentinel error
	code     string
	msg      string
	fields   map[string]string
	cause    error
}

func (e *appError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.msg, e.sentinel, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.msg, e.sentinel)
}

func (e *appError) Unwrap() error { return e.cause }

// Is reports equality with the sentinel.
func (e *appError) Is(target error) bool { return target == e.sentinel }

func NewValidationError(msg string) error {
	return &appError{sentinel: ErrValidation, code: CodeInvalidInput, msg: msg}
}
func NewValidationCode(code, msg string) error {
	return &appError{sentinel: ErrValidation, code: code, msg: msg}
}
func NewValidationFields(code, msg string, fields map[string]string) error {
	return &appError{sentinel: ErrValidation, code: code, msg: msg, fields: fields}
}
func NewNotFoundError(what string) error {
	return &appError{sentinel: ErrNotFound, code: CodeNotFound, msg: what + " not found"}
}
func NewConflictError(msg string) error {
	return &appError{sentinel: ErrConflict, code: CodeConflict, msg: msg}
}

// NewConflictCode is a 409 with a resource-specific code.
func NewConflictCode(code, msg string) error {
	return &appError{sentinel: ErrConflict, code: code, msg: msg}
}
func NewInternalError(msg string) error {
	return &appError{sentinel: ErrInternal, code: CodeInternal, msg: msg}
}
func NewInfrastructureError(cause error) error {
	return &appError{sentinel: ErrInfrastructure, code: CodeInternal, msg: "internal error", cause: cause}
}

type infrastructureError = appError

// SafeMessage returns the client-facing message. For infrastructure and
// internal errors the cause is stripped so raw driver/SQL/DSN detail never
// reaches the browser/API.
func SafeMessage(err error) string {
	var ae *appError
	if !errors.As(err, &ae) {
		return "internal error"
	}
	return ae.msg
}

// statusFor maps an error to its HTTP status.
var ErrMethodNotAllowed = errors.New("method not allowed")

func statusFor(err error) int {
	if rej, ok := middleware.AsRejection(err); ok {
		return rej.Status
	}
	switch {
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrBadGateway):
		return http.StatusBadGateway
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// StatusFor maps an error to its HTTP status (exported for page handlers).
func StatusFor(err error) int { return statusFor(err) }

// ErrorParts returns the client-safe code, message and field errors of err.
func ErrorParts(err error) (code, message string, fields map[string]string) {
	if rej, ok := middleware.AsRejection(err); ok {
		return rej.Code, rej.Message, nil
	}
	var ae *appError
	if errors.As(err, &ae) {
		return ae.code, ae.msg, ae.fields
	}
	return CodeInternal, "internal error", nil
}

// ErrorDetail is one entry of the error `details` list (WBS 17.131).
type ErrorDetail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// detailsOf renders field errors as a deterministic (field-sorted) list.
func detailsOf(fields map[string]string) []ErrorDetail {
	out := make([]ErrorDetail, 0, len(fields))
	for f, m := range fields {
		out = append(out, ErrorDetail{Field: f, Message: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// WriteError writes the centralized structured error response (WBS 3.23,
// 17.131):
//
//	{"error": {"code": ..., "message": ..., "details": [...][, "fields": {...}]}, "request_id": ...}
//
// `details` is always present (empty when there are no field errors);
// `fields` is kept for compatibility with the Phase 3 contract.
//
// Infrastructure detail stays server-side; the body carries only a stable
// code, a safe message, optional field-level validation messages, and the
// request ID for log correlation.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code, message, fields := ErrorParts(err)
	if htmlErrors != nil && wantsHTML(r) {
		htmlErrors(w, r, StatusFor(err), code, message)
		return
	}
	if StatusFor(err) == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	errObj := map[string]any{"code": code, "message": message, "details": detailsOf(fields)}
	if len(fields) > 0 {
		errObj["fields"] = fields
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusFor(err))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      errObj,
		"request_id": RequestIDFromContext(r.Context()),
	})
}

// htmlErrors renders page errors (installed by the page handlers).
var htmlErrors func(w http.ResponseWriter, r *http.Request, status int, code, message string)

// wantsHTML reports a browser page request: outside /api/ and /static/ and
// accepting text/html.
func wantsHTML(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/static/") || p == "/health" || p == "/ready" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// requestIDKey is the context key for request IDs.
type contextKey struct{}

var requestIDKey = contextKey{}

// WithRequestID stores a request ID in the context; it is also the
// operation's correlation id (WBS 18.112) for every log, step, dispatch,
// validation run and audit row the request causes.
func WithRequestID(ctx context.Context, id string) context.Context {
	return logging.WithCorrelationID(context.WithValue(ctx, requestIDKey, id), id)
}

// RequestIDFromContext returns the request ID, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
