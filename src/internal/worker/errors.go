package worker

import (
	"errors"
	"strings"
	"time"
)

// DeliveryState records whether a request reached the worker (WBS 12.34).
type DeliveryState string

// Delivery states.
const (
	DeliveryNotSent  DeliveryState = "DEFINITELY_NOT_SENT"
	DeliveryReceived DeliveryState = "SENT_RESPONSE_RECEIVED"
	DeliveryUnknown  DeliveryState = "DELIVERY_UNKNOWN"
)

// RetryClass is the retry guidance exposed to later orchestration (WBS
// 12.35). Phase 12 itself never retries.
type RetryClass string

// Retry classes.
const (
	RetrySafe     RetryClass = "SAFE_TO_RETRY"
	RetryUnsafe   RetryClass = "UNSAFE_TO_RETRY"
	RetryRecovery RetryClass = "REQUIRES_RECOVERY"
)

// WorkerErrorCode is a normalized provider error (WBS 12.38).
type WorkerErrorCode string

// Normalized error codes.
const (
	CodeUnavailable     WorkerErrorCode = "WORKER_UNAVAILABLE"
	CodeAuthFailed      WorkerErrorCode = "AUTHENTICATION_FAILED"
	CodeSessionLost     WorkerErrorCode = "SESSION_LOST"
	CodeTimeout         WorkerErrorCode = "REQUEST_TIMEOUT"
	CodeRateLimited     WorkerErrorCode = "RATE_LIMITED"
	CodeInvalidRequest  WorkerErrorCode = "INVALID_REQUEST"
	CodeContextLimit    WorkerErrorCode = "CONTEXT_LIMIT"
	CodeProtocol        WorkerErrorCode = "PROTOCOL_ERROR"
	CodeDeliveryUnknown WorkerErrorCode = "DELIVERY_UNKNOWN"
	CodeInternal        WorkerErrorCode = "INTERNAL_PROVIDER_ERROR"
)

// Additional adapter-side codes.
const (
	// CodeCancelled is a caller cancellation (WBS 12.37).
	CodeCancelled WorkerErrorCode = "CANCELLED"
	// CodeBusy is a second concurrent send on a single-request session
	// (WBS 12.56).
	CodeBusy WorkerErrorCode = "SESSION_BUSY"
	// CodeClosed is a request after Close began (WBS 12.66).
	CodeClosed WorkerErrorCode = "WORKER_CLOSED"
	// CodeNoSession is a send/cd with no current session.
	CodeNoSession WorkerErrorCode = "NO_SESSION"
)

// ClassifyRetry maps delivery + code to retry guidance (WBS 12.35): a
// request that never left is safe; an ambiguous delivery requires
// recovery; anything the worker received is unsafe to duplicate blindly.
func ClassifyRetry(d DeliveryState, code WorkerErrorCode) RetryClass {
	switch d {
	case DeliveryNotSent:
		return RetrySafe
	case DeliveryUnknown:
		return RetryRecovery
	}
	switch code {
	case CodeSessionLost:
		return RetryRecovery
	case CodeRateLimited, CodeUnavailable, CodeBusy:
		// An explicit rejection: the worker answered that it did not
		// accept the request, so nothing executed.
		return RetrySafe
	}
	return RetryUnsafe
}

// WorkerError is the generic worker error (WBS 12.39/12.59).
type WorkerError struct {
	Code WorkerErrorCode

	Message string

	Retryable bool

	Delivery DeliveryState

	ProviderCode *string

	// Diagnostic is the sanitized raw provider detail (WBS 12.38).
	Diagnostic string

	RetryAfter *time.Duration

	Cause error
}

// NewError builds a WorkerError; Retryable follows ClassifyRetry.
func NewError(code WorkerErrorCode, d DeliveryState, msg string, cause error) *WorkerError {
	return &WorkerError{Code: code, Message: msg, Delivery: d, Retryable: ClassifyRetry(d, code) == RetrySafe, Cause: cause}
}

func (e *WorkerError) Error() string {
	s := "worker: " + string(e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *WorkerError) Unwrap() error { return e.Cause }

// Is matches another *WorkerError by code.
func (e *WorkerError) Is(target error) bool {
	var t *WorkerError
	if errors.As(target, &t) {
		return t.Code == e.Code && t.Message == ""
	}
	return false
}

// RetryClass returns the retry guidance.
func (e *WorkerError) RetryClass() RetryClass { return ClassifyRetry(e.Delivery, e.Code) }

// Sanitize returns a copy with every secret value removed from message,
// diagnostic and provider code; the cause is dropped when it contains a
// secret (WBS 12.39.3).
func (e *WorkerError) Sanitize(secrets ...string) *WorkerError {
	c := *e
	c.Message, c.Diagnostic = Redact(c.Message, secrets...), Redact(c.Diagnostic, secrets...)
	if c.ProviderCode != nil {
		pc := Redact(*c.ProviderCode, secrets...)
		c.ProviderCode = &pc
	}
	if c.Cause != nil && Redact(c.Cause.Error(), secrets...) != c.Cause.Error() {
		c.Cause = errors.New(Redact(c.Cause.Error(), secrets...))
	}
	return &c
}

// Redact replaces each non-empty secret in s with "[REDACTED]".
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[REDACTED]")
		}
	}
	return s
}

// WorkerErrorSummary is the secret-free error read model (WBS 12.49).
type WorkerErrorSummary struct {
	Code         WorkerErrorCode
	Message      string
	Delivery     DeliveryState
	ProviderCode string
	At           time.Time
}

// Summary returns the read-model summary (no cause, no diagnostic).
func (e *WorkerError) Summary(at time.Time) WorkerErrorSummary {
	s := WorkerErrorSummary{Code: e.Code, Message: e.Message, Delivery: e.Delivery, At: at}
	if e.ProviderCode != nil {
		s.ProviderCode = *e.ProviderCode
	}
	return s
}

// AsWorkerError extracts a *WorkerError, wrapping any other error as
// INTERNAL_PROVIDER_ERROR with unknown delivery (never assumed unsent).
func AsWorkerError(err error) *WorkerError {
	if err == nil {
		return nil
	}
	var we *WorkerError
	if errors.As(err, &we) {
		return we
	}
	return NewError(CodeInternal, DeliveryUnknown, "unclassified worker error", err)
}

// Truncate cuts s to at most n bytes (diagnostic helper).
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
