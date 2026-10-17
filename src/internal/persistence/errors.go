package persistence

import "errors"

// Domain-level persistence errors. Infrastructure adapters translate
// driver-specific failures into these values so application code never
// inspects PostgreSQL error codes (WBS 2.24).
var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("persistence: not found")
	// ErrConflict means a uniqueness constraint was violated.
	ErrConflict = errors.New("persistence: conflict")
	// ErrInvalidReference means a foreign-key constraint was violated.
	ErrInvalidReference = errors.New("persistence: invalid reference")
)

// InfrastructureError wraps an unexpected persistence failure whose driver
// type must not escape the repository boundary.
type InfrastructureError struct {
	Op    string
	Cause error
}

func (e *InfrastructureError) Error() string {
	return "persistence: " + e.Op + ": " + e.Cause.Error()
}

func (e *InfrastructureError) Unwrap() error { return e.Cause }
