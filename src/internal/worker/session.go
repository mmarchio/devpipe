package worker

import (
	"errors"
	"fmt"
)

// ErrSessionIDRequired is returned when a session-capable provider returns
// an empty session id (WBS 12.16).
var ErrSessionIDRequired = errors.New("worker: session id required")

// Validate checks provider-reported session info. The id is never
// trimmed or rewritten.
func (s SessionInfo) Validate(requiresSessions bool) error {
	if requiresSessions && s.ID == "" {
		return ErrSessionIDRequired
	}
	if s.WorkingDirectory != nil && *s.WorkingDirectory == "" {
		return fmt.Errorf("worker: working directory, when reported, must not be empty")
	}
	return nil
}

// SessionStatus is the overseer's knowledge of an external session
// (WBS 12.17).
type SessionStatus string

// Session statuses.
const (
	SessionActive SessionStatus = "ACTIVE"
	SessionReset  SessionStatus = "RESET"
	SessionLost   SessionStatus = "LOST"
	SessionClosed SessionStatus = "CLOSED"
	SessionFailed SessionStatus = "FAILED"
)

// Valid reports whether s is a known status.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionActive, SessionReset, SessionLost, SessionClosed, SessionFailed:
		return true
	}
	return false
}

// Terminal reports whether s is final: only ACTIVE may change.
func (s SessionStatus) Terminal() bool { return s.Valid() && s != SessionActive }

// ErrIllegalSessionTransition is returned for a forbidden status change.
var ErrIllegalSessionTransition = errors.New("worker: illegal session status transition")

// CheckSessionTransition allows ACTIVE → RESET/LOST/CLOSED/FAILED only.
func CheckSessionTransition(from, to SessionStatus) error {
	if from == SessionActive && to.Terminal() {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrIllegalSessionTransition, from, to)
}

// SessionPolicy controls how many sessions a project may hold (WBS 12.22).
type SessionPolicy string

// Session policies.
const (
	PolicySingleActive   SessionPolicy = "SINGLE_ACTIVE_PER_PROJECT"
	PolicyMultipleActive SessionPolicy = "MULTIPLE_ACTIVE"
)

// DefaultSessionPolicy is the initial Hermes workflow policy.
const DefaultSessionPolicy = PolicySingleActive

// Valid reports whether p is a known policy.
func (p SessionPolicy) Valid() bool { return p == PolicySingleActive || p == PolicyMultipleActive }

// Reconciliation is the outcome of checking a persisted session against
// the provider (WBS 12.42).
type Reconciliation string

// Reconciliation outcomes.
const (
	ReconcileConfirmed Reconciliation = "CONFIRMED"
	ReconcileLost      Reconciliation = "LOST"
	ReconcileUnknown   Reconciliation = "UNKNOWN"
)

// SessionReconciliation carries the outcome plus what the provider reported.
type SessionReconciliation struct {
	Result  Reconciliation
	Session SessionInfo
	Err     error
}

// SessionReadiness is the dispatch readiness of a project session
// (WBS 12.27). It never triggers session creation or reset.
type SessionReadiness struct {
	Connected             bool
	SessionActive         bool
	WorkingDirectoryReady bool
	// CWDInitialized is false while the session's directory is UNKNOWN
	// (WBS 12.26.3).
	CWDInitialized bool
}

// Ready reports whether a dispatch may proceed.
func (r SessionReadiness) Ready() bool {
	return r.Connected && r.SessionActive && r.WorkingDirectoryReady
}

// EvaluateReadiness compares the confirmed session directory with the
// project's desired one (WBS 12.24: stored separately, never assumed equal).
func EvaluateReadiness(connected bool, status *SessionStatus, confirmedCWD *string, desiredCWD string) SessionReadiness {
	r := SessionReadiness{Connected: connected}
	r.SessionActive = status != nil && *status == SessionActive
	r.CWDInitialized = confirmedCWD != nil
	r.WorkingDirectoryReady = r.SessionActive && confirmedCWD != nil && desiredCWD != "" && *confirmedCWD == desiredCWD
	return r
}
