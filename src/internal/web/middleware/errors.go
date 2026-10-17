package middleware

import (
	"errors"
	"fmt"
	"net/http"
)

// Rejection is a request refused by the web boundary before any handler
// runs. The web error writer renders it through the standard error model
// (WBS 17.131).
type Rejection struct {
	Status  int
	Code    string
	Message string
}

func (r *Rejection) Error() string { return fmt.Sprintf("%d %s: %s", r.Status, r.Code, r.Message) }

// Rejection codes (mirror internal/web Code* constants).
const (
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeCSRFInvalid     = "CSRF_INVALID"
	// CodeSessionExpired: a mutation presented an expired / unknown session
	// (WBS 18.82).
	CodeSessionExpired = "SESSION_EXPIRED"
	CodeTooLarge       = "REQUEST_TOO_LARGE"
	CodeRateLimited    = "RATE_LIMITED"
	CodeInvalidInput   = "invalid_input"
)

var (
	errUnauthenticated = &Rejection{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Message: "authentication required"}
	errForbidden       = &Rejection{Status: http.StatusForbidden, Code: CodeForbidden, Message: "not permitted"}
	errSessionExpired  = &Rejection{Status: http.StatusForbidden, Code: CodeSessionExpired, Message: "session expired; reload the page to start a new session"}
	errCSRF            = &Rejection{Status: http.StatusForbidden, Code: CodeCSRFInvalid, Message: "missing or invalid CSRF token"}
	errTooLarge        = &Rejection{Status: http.StatusRequestEntityTooLarge, Code: CodeTooLarge, Message: "request too large"}
	errRateLimited     = &Rejection{Status: http.StatusTooManyRequests, Code: CodeRateLimited, Message: "too many requests"}
)

// AsRejection extracts a Rejection from err.
func AsRejection(err error) (*Rejection, bool) {
	var r *Rejection
	ok := errors.As(err, &r)
	return r, ok
}

// ErrorWriter renders an error response. internal/web passes WriteError.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// plainErrorWriter is the fallback when no writer is configured.
func plainErrorWriter(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusInternalServerError
	if rej, ok := AsRejection(err); ok {
		status = rej.Status
	}
	http.Error(w, http.StatusText(status), status)
}

func writerOr(ew ErrorWriter) ErrorWriter {
	if ew == nil {
		return plainErrorWriter
	}
	return ew
}
