package service

import (
	"errors"

	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/resilience"
)

// Service-level errors. Callers match these with errors.Is; no
// persistence driver type ever crosses the service boundary (WBS 3.1,
// following the Phase 2 error-translation rule of WBS 2.24).
var (
	// ErrInvalidInput means the operation input failed domain validation.
	ErrInvalidInput = errors.New("service: invalid input")
	// ErrNotFound means the requested project does not exist.
	ErrNotFound = errors.New("service: project not found")
	// ErrConflict means a uniqueness constraint was violated.
	ErrConflict = errors.New("service: conflict")
	// ErrInvalidReference means a foreign-key constraint was violated.
	ErrInvalidReference = errors.New("service: invalid reference")
	// ErrInfrastructure means an unexpected persistence failure.
	ErrInfrastructure = errors.New("service: infrastructure failure")
	// ErrShuttingDown: the process is shutting down; the operation was not
	// started, or its outcome is left for restart reconciliation.
	ErrShuttingDown = errors.New("service: shutting down")
	// ErrMissingStateFile means a required state file is absent at its
	// resolved scope (WBS 7.19); the error message identifies the missing
	// type's key (T7.19.2).
	ErrMissingStateFile = errors.New("service: required state file missing")
)

// translate maps the repository domain error set onto the service-level
// set (T3.1.3).
func translate(err error) error {
	switch {
	case errors.Is(err, faults.ErrCrash):
		// A simulated crash stays one (fault-injection tests).
		return err
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrConflict),
		errors.Is(err, ErrInvalidReference),
		errors.Is(err, ErrInvalidInput),
		errors.Is(err, ErrMissingStateFile):
		// already a service-level error (e.g. from an inner service call)
		return err
	case errors.Is(err, persistence.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, persistence.ErrConflict):
		return ErrConflict
	case errors.Is(err, persistence.ErrInvalidReference):
		return ErrInvalidReference
	default:
		// Keep only the production classification (WBS 18.188) so a
		// transaction helper can still retry a serialization failure or
		// refuse to replay an ambiguous commit; the driver error itself
		// never crosses the service boundary.
		if c := resilience.ClassOf(err); c == resilience.Transient || c == resilience.Ambiguous {
			return resilience.Mark(ErrInfrastructure, c)
		}
		return ErrInfrastructure
	}
}
