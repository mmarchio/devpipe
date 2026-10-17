package continuation

import "errors"

// Strategy is how an unfinished work unit continues (WBS 14.3).
type Strategy string

// Strategies.
const (
	ContinueSameSession  Strategy = "same_session"
	ContinueFreshSession Strategy = "fresh_session"
)

// ErrInvalidStrategy reports an unknown strategy.
var ErrInvalidStrategy = errors.New("continuation: invalid strategy")

// ParseStrategy accepts same_session or fresh_session.
func ParseStrategy(s string) (Strategy, error) {
	switch st := Strategy(s); st {
	case ContinueSameSession, ContinueFreshSession:
		return st, nil
	}
	return "", ErrInvalidStrategy
}

// Valid reports a known strategy.
func (s Strategy) Valid() bool { _, err := ParseStrategy(string(s)); return err == nil }

// Label is the display label ("Same Session" / "Fresh Session").
func (s Strategy) Label() string {
	switch s {
	case ContinueSameSession:
		return "Same Session"
	case ContinueFreshSession:
		return "Fresh Session"
	}
	return "Undecided"
}

// DecisionReason records why a strategy was selected (WBS 14.14).
type DecisionReason string

// Decision reasons.
const (
	ReasonBelowThreshold   DecisionReason = "below_threshold"
	ReasonAtThreshold      DecisionReason = "at_threshold"
	ReasonAboveThreshold   DecisionReason = "above_threshold"
	ReasonContextUnknown   DecisionReason = "context_unknown"
	ReasonContextLimit     DecisionReason = "context_limit"
	ReasonSessionLost      DecisionReason = "session_lost"
	ReasonOperatorOverride DecisionReason = "operator_override"
)

// ErrInvalidReason reports an unknown decision reason.
var ErrInvalidReason = errors.New("continuation: invalid decision reason")

// ParseDecisionReason accepts one of the decision reasons.
func ParseDecisionReason(s string) (DecisionReason, error) {
	switch r := DecisionReason(s); r {
	case ReasonBelowThreshold, ReasonAtThreshold, ReasonAboveThreshold, ReasonContextUnknown,
		ReasonContextLimit, ReasonSessionLost, ReasonOperatorOverride:
		return r, nil
	}
	return "", ErrInvalidReason
}

// Trigger is what created a continuation.
type Trigger string

// Triggers.
const (
	TriggerResultIncomplete Trigger = "RESULT_INCOMPLETE"
	TriggerContextLimit     Trigger = "CONTEXT_LIMIT"
	TriggerSessionLost      Trigger = "SESSION_LOST"
	TriggerManualRetry      Trigger = "MANUAL_RETRY"
)

// ErrInvalidTrigger reports an unknown trigger.
var ErrInvalidTrigger = errors.New("continuation: invalid trigger")

// ParseTrigger accepts one of the triggers.
func ParseTrigger(s string) (Trigger, error) {
	switch t := Trigger(s); t {
	case TriggerResultIncomplete, TriggerContextLimit, TriggerSessionLost, TriggerManualRetry:
		return t, nil
	}
	return "", ErrInvalidTrigger
}

// RequiresResult reports whether the trigger needs a source WorkerResult
// (context-limit / lost-session recovery binds only the source dispatch,
// WBS 14.44).
func (t Trigger) RequiresResult() bool { return t == TriggerResultIncomplete }

// RecoveryReason records why a continuation needs operator recovery.
type RecoveryReason string

// Recovery reasons.
const (
	RecoveryDeliveryUnknown      RecoveryReason = "DELIVERY_UNKNOWN"
	RecoveryNoProgress           RecoveryReason = "NO_PROGRESS"
	RecoveryContinuationLimit    RecoveryReason = "CONTINUATION_LIMIT"
	RecoveryResetLimit           RecoveryReason = "RESET_LIMIT"
	RecoverySessionCreateFailed  RecoveryReason = "SESSION_CREATE_FAILED"
	RecoveryCWDRestoreFailed     RecoveryReason = "CWD_RESTORE_FAILED"
	RecoveryBindingChanged       RecoveryReason = "BINDING_CHANGED"
	RecoveryDispatchRejected     RecoveryReason = "DISPATCH_REJECTED"
	RecoverySessionLostAfterSend RecoveryReason = "SESSION_LOST_AFTER_SEND"
	RecoveryResultRejected       RecoveryReason = "RESULT_REJECTED"
	RecoveryDispatchMissing      RecoveryReason = "DISPATCH_MISSING"
)

// ErrInvalidRecoveryReason reports an unknown recovery reason.
var ErrInvalidRecoveryReason = errors.New("continuation: invalid recovery reason")

// ParseRecoveryReason accepts one of the recovery reasons.
func ParseRecoveryReason(s string) (RecoveryReason, error) {
	switch r := RecoveryReason(s); r {
	case RecoveryDeliveryUnknown, RecoveryNoProgress, RecoveryContinuationLimit, RecoveryResetLimit,
		RecoverySessionCreateFailed, RecoveryCWDRestoreFailed, RecoveryBindingChanged, RecoveryDispatchRejected,
		RecoverySessionLostAfterSend, RecoveryResultRejected, RecoveryDispatchMissing:
		return r, nil
	}
	return "", ErrInvalidRecoveryReason
}
