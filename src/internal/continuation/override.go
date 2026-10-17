package continuation

import (
	"errors"

	"overseer/internal/worker"
)

// Override / retry errors.
var (
	ErrOverrideNotAllowed = errors.New("continuation: override not allowed in this status")
	ErrRetryNotAllowed    = errors.New("continuation: retry not allowed in this status")
	ErrReconciliationAck  = errors.New("continuation: delivery-unknown continuation requires a reconciliation acknowledgement")
)

// overridable are the pre-dispatch statuses in which an operator may
// change the strategy (WBS 14.85).
func overridable(s Status) bool {
	switch s {
	case StatusPending, StatusDeciding, StatusSameSession, StatusResetRequired, StatusRestoringCWD, StatusReady:
		return true
	}
	return false
}

// ValidateOverride checks an operator strategy override (WBS 14.85,
// 14.86): a fresh session may be requested before dispatch; same-session
// is rejected when the session is not ACTIVE or the context limit or
// threshold is known to be reached.
func ValidateOverride(requested Strategy, c *Continuation, sessionStatus worker.SessionStatus) error {
	if !requested.Valid() {
		return ErrInvalidStrategy
	}
	if !overridable(c.Status) {
		return ErrOverrideNotAllowed
	}
	if requested == ContinueFreshSession {
		return nil
	}
	if sessionStatus != worker.SessionActive || c.Trigger == TriggerContextLimit || c.Trigger == TriggerSessionLost {
		return ErrUnsafeOverride
	}
	if d := c.Decision; d != nil {
		switch d.Reason {
		case ReasonContextLimit, ReasonSessionLost, ReasonAtThreshold, ReasonAboveThreshold:
			return ErrUnsafeOverride
		}
	}
	return nil
}

// CanRetry checks an administrative retry (WBS 14.84, 14.48): only a
// RECOVERY_REQUIRED or FAILED continuation; delivery-unknown needs an
// explicit acknowledgement that the repository was reconciled.
func CanRetry(c *Continuation, reconciled bool) error {
	if c.Status != StatusRecoveryRequired && c.Status != StatusFailed {
		return ErrRetryNotAllowed
	}
	if c.RecoveryReason == RecoveryDeliveryUnknown || c.RecoveryReason == RecoverySessionLostAfterSend {
		if !reconciled {
			return ErrReconciliationAck
		}
	}
	return nil
}
