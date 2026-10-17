package compression

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Compressor is the provider-independent compression contract (WBS 9.1).
// Provider-specific adapters (OpenAI-compatible HTTP, local servers, ...)
// live outside the domain and satisfy this interface; domain and
// application code depend only on it. Implementations must honour ctx
// cancellation/deadline.
type Compressor interface {
	Compress(ctx context.Context, req CompressionRequest) (CompressionResponse, error)
}

// CompressorFunc adapts a function to the Compressor interface.
type CompressorFunc func(ctx context.Context, req CompressionRequest) (CompressionResponse, error)

// Compress calls f.
func (f CompressorFunc) Compress(ctx context.Context, req CompressionRequest) (CompressionResponse, error) {
	return f(ctx, req)
}

// CompressionResponse is the provider-independent compression result (WBS
// 9.12). Token counts, finish reason and request id are optional provider
// metadata (zero / empty when the provider does not report them, T9.12.5).
type CompressionResponse struct {
	Content string

	Provider string
	Model    string

	InputTokens  int
	OutputTokens int

	FinishReason string

	ProviderRequestID string

	Duration time.Duration
}

// ErrorKind is the generic classification of a compression failure (WBS
// 9.37) — no provider type crosses the interface.
type ErrorKind string

const (
	ErrorNetwork         ErrorKind = "NETWORK"
	ErrorUnavailable     ErrorKind = "UNAVAILABLE"
	ErrorRateLimit       ErrorKind = "RATE_LIMIT"
	ErrorAuth            ErrorKind = "AUTH"
	ErrorMalformed       ErrorKind = "MALFORMED"
	ErrorEmpty           ErrorKind = "EMPTY"
	ErrorContextOverflow ErrorKind = "CONTEXT_OVERFLOW"
	ErrorTimeout         ErrorKind = "TIMEOUT"
	ErrorCancelled       ErrorKind = "CANCELLED"
	ErrorConfiguration   ErrorKind = "CONFIGURATION"
	ErrorUnknown         ErrorKind = "UNKNOWN"
	// ErrorCircuitOpen: the provider's circuit opened during the run
	// (WBS 18.27); no request was sent and the run stops retrying.
	ErrorCircuitOpen ErrorKind = "CIRCUIT_OPEN"
)

// Retryable reports whether a failure of this kind is transient (WBS
// 9.37/9.38): network failures, provider unavailability, rate limits,
// timeouts, and malformed/empty responses may succeed on retry; invalid
// credentials, configuration errors, context overflow (the same input will
// overflow again) and caller cancellation are permanent.
func (k ErrorKind) Retryable() bool {
	switch k {
	case ErrorNetwork, ErrorUnavailable, ErrorRateLimit, ErrorTimeout, ErrorMalformed, ErrorEmpty:
		return true
	default:
		return false
	}
}

// CompressionError is the generic compression failure (T9.1.4). Message must
// never contain credentials (adapters redact before constructing it).
type CompressionError struct {
	Kind       ErrorKind
	Message    string
	StatusCode int
}

func (e *CompressionError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("compression: %s (status %d): %s", e.Kind, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("compression: %s: %s", e.Kind, e.Message)
}

// NewError builds a CompressionError.
func NewError(kind ErrorKind, msg string) *CompressionError {
	return &CompressionError{Kind: kind, Message: msg}
}

// KindOf classifies any error returned by a Compressor: a CompressionError
// keeps its kind; context deadline → TIMEOUT; context cancellation →
// CANCELLED; anything else → UNKNOWN (treated as permanent).
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var ce *CompressionError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrorCancelled
	}
	return ErrorUnknown
}

// IsRetryable reports whether err is a transient compression failure.
func IsRetryable(err error) bool { return KindOf(err).Retryable() }
