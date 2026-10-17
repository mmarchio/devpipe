package continuation

import (
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
)

// Action is the next restart-safe step for a persisted continuation.
type Action string

// Reconciliation actions.
const (
	ActionDecide         Action = "DECIDE"
	ActionValidateSame   Action = "VALIDATE_SESSION"
	ActionStartReset     Action = "START_RESET"
	ActionCreateSession  Action = "CREATE_SESSION"
	ActionAdoptSession   Action = "ADOPT_SESSION"
	ActionRestoreContext Action = "RESTORE_CONTEXT"
	ActionCheckCWD       Action = "CHECK_CWD"
	ActionPrepare        Action = "PREPARE_AND_DISPATCH"
	ActionAdmitDispatch  Action = "ADMIT_DISPATCH"
	ActionWait           Action = "WAIT"
	ActionParse          Action = "PARSE"
	ActionConsume        Action = "CONSUME"
	ActionFresh          Action = "FRESH_SESSION"
	ActionRetryDispatch  Action = "RETRY_DISPATCH"
	ActionRecovery       Action = "RECOVERY_REQUIRED"
	ActionFail           Action = "FAIL"
	ActionNone           Action = "NONE"
)

// SessionFact is a Phase 12 session as persisted.
type SessionFact struct {
	ID               uuid.UUID
	Status           worker.SessionStatus
	CreatedAt        time.Time
	WorkingDirectory *string
}

// DispatchFact is the continuation's Phase 12 dispatch as persisted.
type DispatchFact struct {
	ID        uuid.UUID
	Status    worker.DispatchStatus
	Delivery  *worker.DeliveryState
	ErrorCode string
	// Parsed: Phase 13 accepted a result for the dispatch.
	Parsed bool
	// Rejected: Phase 13 recorded only rejected parse attempts.
	Rejected bool
	// Consumed: the accepted result was already consumed.
	Consumed bool
}

// Facts are the persisted continuation position plus observed Phase 12
// state.
type Facts struct {
	Status Status
	// IntentAt is when the current status was entered.
	IntentAt time.Time

	OldSessionID  *uuid.UUID
	NewSessionID  *uuid.UUID
	ActiveSession *SessionFact
	DesiredCWD    string

	Dispatch *DispatchFact
}

// Plan is the planner's verdict.
type Plan struct {
	Action    Action
	SessionID *uuid.UUID
	Reason    DecisionReason
	Recovery  RecoveryReason
}

// PlanRecovery is the pure restart planner (WBS 14.94-14.99): it never
// assumes an external operation failed or did not happen, never creates a
// second session once one was created after the persisted intent, and
// never admits a dispatch while one exists for the continuation.
func PlanRecovery(f Facts) Plan {
	switch f.Status {
	case StatusPending, StatusDeciding:
		return Plan{Action: ActionDecide}
	case StatusSameSession:
		return Plan{Action: ActionValidateSame}
	case StatusResetRequired:
		return Plan{Action: ActionStartReset}
	case StatusCreatingSession:
		if s := f.ActiveSession; s != nil && s.Status == worker.SessionActive && !s.CreatedAt.Before(f.IntentAt) &&
			(f.OldSessionID == nil || s.ID != *f.OldSessionID) {
			id := s.ID
			return Plan{Action: ActionAdoptSession, SessionID: &id}
		}
		return Plan{Action: ActionCreateSession}
	case StatusRestoringContext:
		return Plan{Action: ActionRestoreContext}
	case StatusRestoringCWD:
		return Plan{Action: ActionCheckCWD}
	case StatusReady:
		return Plan{Action: ActionCheckCWD}
	case StatusDispatching:
		if f.Dispatch == nil {
			return Plan{Action: ActionAdmitDispatch}
		}
		return followDispatch(*f.Dispatch)
	case StatusWaiting:
		if f.Dispatch == nil {
			return Plan{Action: ActionRecovery, Recovery: RecoveryDispatchMissing}
		}
		return followDispatch(*f.Dispatch)
	}
	return Plan{Action: ActionNone}
}

func followDispatch(d DispatchFact) Plan {
	switch d.Status {
	case worker.DispatchCompleted:
		switch {
		case d.Parsed:
			return Plan{Action: ActionConsume}
		case d.Rejected:
			return Plan{Action: ActionRecovery, Recovery: RecoveryResultRejected}
		}
		return Plan{Action: ActionParse}
	case worker.DispatchPending, worker.DispatchSending, worker.DispatchWaiting:
		return Plan{Action: ActionWait}
	case worker.DispatchUnknown:
		return Plan{Action: ActionRecovery, Recovery: RecoveryDeliveryUnknown}
	case worker.DispatchFailed, worker.DispatchCancelled:
		del := worker.DeliveryUnknown
		if d.Delivery != nil {
			del = *d.Delivery
		}
		fc := ClassifyDispatchFailure(worker.WorkerErrorCode(d.ErrorCode), del)
		switch fc.Kind {
		case FailureFresh:
			return Plan{Action: ActionFresh, Reason: fc.Reason}
		case FailureRecovery:
			return Plan{Action: ActionRecovery, Recovery: fc.Recovery}
		case FailureRetry:
			return Plan{Action: ActionRetryDispatch}
		}
		return Plan{Action: ActionFail}
	}
	return Plan{Action: ActionRecovery, Recovery: RecoveryDeliveryUnknown}
}
