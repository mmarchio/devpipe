package execution

import (
	"errors"
	"fmt"
)

// ExecutionStatus is the lifecycle state of one execution — a durable run of
// a project's configuration (WBS 15.2). Phase 15 owns INITIALIZING → READY →
// RUNNING; Phase 16 owns runtime progression (RUNNING ↔ WAITING, PAUSED,
// RECOVERY_REQUIRED, COMPLETED). The WBS 16.2 PENDING status corresponds to
// INITIALIZING / READY (an execution not yet driven).
type ExecutionStatus string

// Execution statuses (WBS 15.2).
const (
	ExecutionInitializing ExecutionStatus = "INITIALIZING"
	ExecutionReady        ExecutionStatus = "READY"
	ExecutionRunning      ExecutionStatus = "RUNNING"
	ExecutionPaused       ExecutionStatus = "PAUSED"
	ExecutionCompleted    ExecutionStatus = "COMPLETED"
	ExecutionFailed       ExecutionStatus = "FAILED"
	ExecutionCancelled    ExecutionStatus = "CANCELLED"
	ExecutionRecovery     ExecutionStatus = "RECOVERY_REQUIRED"
	// ExecutionWaiting: a worker dispatch or continuation is in flight
	// (WBS 16.29).
	ExecutionWaiting ExecutionStatus = "WAITING"
)

// ErrInvalidStatus reports an empty or unknown execution status.
var ErrInvalidStatus = errors.New("execution: invalid status")

var allStatuses = []ExecutionStatus{
	ExecutionInitializing, ExecutionReady, ExecutionRunning, ExecutionPaused,
	ExecutionCompleted, ExecutionFailed, ExecutionCancelled, ExecutionRecovery,
	ExecutionWaiting,
}

// AllStatuses returns every status in declaration order.
func AllStatuses() []ExecutionStatus { return append([]ExecutionStatus(nil), allStatuses...) }

// ParseExecutionStatus accepts exactly one canonical status token.
func ParseExecutionStatus(s string) (ExecutionStatus, error) {
	st := ExecutionStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidStatus, s)
	}
	return st, nil
}

// String returns the canonical token.
func (s ExecutionStatus) String() string { return string(s) }

// Valid reports whether s is a known status.
func (s ExecutionStatus) Valid() bool {
	for _, v := range allStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// IsActive reports a status that blocks another execution of the same
// project (WBS 15.4).
func (s ExecutionStatus) IsActive() bool {
	switch s {
	case ExecutionInitializing, ExecutionReady, ExecutionRunning, ExecutionPaused, ExecutionRecovery, ExecutionWaiting:
		return true
	}
	return false
}

// IsSchedulable reports a status the execution scheduler may drive
// (WBS 16.6): RUNNING, and WAITING (the scheduler checks whether the wait
// condition resolved). PAUSED, RECOVERY_REQUIRED and terminal statuses are
// never scheduled (WBS 16.76, 16.159).
func (s ExecutionStatus) IsSchedulable() bool {
	return s == ExecutionRunning || s == ExecutionWaiting
}

// IsTerminal reports a status with no outgoing transition.
func (s ExecutionStatus) IsTerminal() bool {
	switch s {
	case ExecutionCompleted, ExecutionFailed, ExecutionCancelled:
		return true
	}
	return false
}

// ActiveStatuses returns the active set in declaration order.
func ActiveStatuses() []ExecutionStatus {
	var out []ExecutionStatus
	for _, s := range allStatuses {
		if s.IsActive() {
			out = append(out, s)
		}
	}
	return out
}

// ErrIllegalTransition reports a disallowed status edge.
var ErrIllegalTransition = errors.New("execution: illegal status transition")

// edges is the execution status table (PLAN Decision 3).
var edges = map[ExecutionStatus][]ExecutionStatus{
	ExecutionInitializing: {ExecutionReady, ExecutionFailed, ExecutionCancelled},
	ExecutionReady:        {ExecutionRunning, ExecutionFailed, ExecutionCancelled},
	ExecutionRunning:      {ExecutionWaiting, ExecutionPaused, ExecutionRecovery, ExecutionCompleted, ExecutionFailed, ExecutionCancelled},
	ExecutionWaiting:      {ExecutionRunning, ExecutionPaused, ExecutionRecovery, ExecutionFailed, ExecutionCancelled},
	ExecutionPaused:       {ExecutionRunning, ExecutionFailed, ExecutionCancelled},
	ExecutionRecovery:     {ExecutionRunning, ExecutionFailed, ExecutionCancelled},
}

// CanTransition reports whether from → to is an allowed edge.
func CanTransition(from, to ExecutionStatus) bool {
	for _, t := range edges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrIllegalTransition for a disallowed edge.
func ValidateTransition(from, to ExecutionStatus) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	return nil
}
