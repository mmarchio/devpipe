package continuation

import "overseer/internal/worker"

// Continuation audit events (WBS 14.87) — appended to the worker event
// log only for committed transitions.
const (
	EventCreated               worker.EventType = "CONTINUATION_CREATED"
	EventDecided               worker.EventType = "CONTINUATION_DECIDED"
	EventSessionResetStarted   worker.EventType = "CONTINUATION_SESSION_RESET_STARTED"
	EventSessionResetCompleted worker.EventType = "CONTINUATION_SESSION_RESET_COMPLETED"
	EventContextRestored       worker.EventType = "CONTINUATION_CONTEXT_RESTORED"
	EventCWDRestored           worker.EventType = "CONTINUATION_CWD_RESTORED"
	EventDispatchStarted       worker.EventType = "CONTINUATION_DISPATCH_STARTED"
	EventDispatchCompleted     worker.EventType = "CONTINUATION_DISPATCH_COMPLETED"
	EventBlocked               worker.EventType = "CONTINUATION_BLOCKED"
	EventNoProgress            worker.EventType = "CONTINUATION_NO_PROGRESS"
	EventRecoveryRequired      worker.EventType = "CONTINUATION_RECOVERY_REQUIRED"
	EventFailed                worker.EventType = "CONTINUATION_FAILED"
	EventCancelled             worker.EventType = "CONTINUATION_CANCELLED"
)

// EventFor returns the audit event of a committed transition (false when
// the edge has no dedicated event).
func EventFor(from, to Status) (worker.EventType, bool) {
	switch to {
	case StatusSameSession, StatusResetRequired:
		if from == StatusDeciding {
			return EventDecided, true
		}
	case StatusCreatingSession:
		return EventSessionResetStarted, true
	case StatusRestoringContext:
		return EventSessionResetCompleted, true
	case StatusRestoringCWD:
		if from == StatusRestoringContext {
			return EventContextRestored, true
		}
	case StatusReady:
		if from == StatusRestoringCWD {
			return EventCWDRestored, true
		}
	case StatusDispatching:
		return EventDispatchStarted, true
	case StatusWaiting:
		return EventDispatchCompleted, true
	case StatusBlocked:
		return EventBlocked, true
	case StatusRecoveryRequired:
		return EventRecoveryRequired, true
	case StatusFailed:
		return EventFailed, true
	case StatusCancelled:
		return EventCancelled, true
	}
	return "", false
}
