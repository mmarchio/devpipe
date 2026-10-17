package continuation

import "errors"

// Status is the continuation operation's lifecycle state (WBS 14.2). It
// describes the operation, never the WBS work-unit state.
type Status string

// Lifecycle states.
const (
	StatusPending          Status = "PENDING"
	StatusDeciding         Status = "DECIDING"
	StatusSameSession      Status = "SAME_SESSION"
	StatusResetRequired    Status = "RESET_REQUIRED"
	StatusCreatingSession  Status = "CREATING_SESSION"
	StatusRestoringContext Status = "RESTORING_CONTEXT"
	StatusRestoringCWD     Status = "RESTORING_CWD"
	StatusReady            Status = "READY"
	StatusDispatching      Status = "DISPATCHING"
	StatusWaiting          Status = "WAITING"
	StatusCompleted        Status = "COMPLETED"
	StatusBlocked          Status = "BLOCKED"
	StatusFailed           Status = "FAILED"
	StatusCancelled        Status = "CANCELLED"
	StatusRecoveryRequired Status = "RECOVERY_REQUIRED"
)

// ErrInvalidStatus reports an empty or unknown lifecycle state.
var ErrInvalidStatus = errors.New("continuation: invalid status")

var allStatuses = []Status{
	StatusPending, StatusDeciding, StatusSameSession, StatusResetRequired, StatusCreatingSession,
	StatusRestoringContext, StatusRestoringCWD, StatusReady, StatusDispatching, StatusWaiting,
	StatusCompleted, StatusBlocked, StatusFailed, StatusCancelled, StatusRecoveryRequired,
}

// AllStatuses returns every lifecycle state in declaration order.
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// ParseStatus accepts exactly one of the lifecycle state names.
func ParseStatus(s string) (Status, error) {
	st := Status(s)
	if !st.Valid() {
		return "", ErrInvalidStatus
	}
	return st, nil
}

// Valid reports whether s is a lifecycle state.
func (s Status) Valid() bool {
	for _, v := range allStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// IsTerminal reports a state with no outgoing transition.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusBlocked, StatusFailed, StatusCancelled, StatusRecoveryRequired:
		return true
	}
	return false
}

// TerminalStatuses returns the terminal states.
func TerminalStatuses() []Status {
	return []Status{StatusCompleted, StatusBlocked, StatusFailed, StatusCancelled, StatusRecoveryRequired}
}
