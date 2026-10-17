package continuation

import "overseer/internal/worker"

// FailureKind classifies a Phase 12 dispatch failure for continuation.
type FailureKind string

// Failure kinds.
const (
	// FailureFresh: fresh-session recovery is safe (nothing ambiguous).
	FailureFresh FailureKind = "FRESH_SESSION"
	// FailureRecovery: operator recovery; never resend.
	FailureRecovery FailureKind = "RECOVERY_REQUIRED"
	// FailureRetry: definitely not sent and transient; the same step may
	// be admitted again.
	FailureRetry FailureKind = "RETRY"
	// FailureTerminal: the continuation fails.
	FailureTerminal FailureKind = "FAILED"
)

// FailureClass is the classification with its reason.
type FailureClass struct {
	Kind     FailureKind
	Reason   DecisionReason
	Recovery RecoveryReason
}

// ClassifyDispatchFailure applies WBS 14.44-14.47: DELIVERY_UNKNOWN is
// never resent; CONTEXT_LIMIT selects fresh-session recovery; a session
// lost before execution (not sent, or rejected by the gateway because the
// session does not exist) selects fresh-session recovery.
func ClassifyDispatchFailure(code worker.WorkerErrorCode, delivery worker.DeliveryState) FailureClass {
	if delivery == worker.DeliveryUnknown || code == worker.CodeDeliveryUnknown {
		return FailureClass{Kind: FailureRecovery, Recovery: RecoveryDeliveryUnknown}
	}
	switch code {
	case worker.CodeContextLimit:
		return FailureClass{Kind: FailureFresh, Reason: ReasonContextLimit}
	case worker.CodeSessionLost, worker.CodeNoSession:
		// Not sent, or the gateway answered that the session does not
		// exist (nothing executed): unambiguous, fresh session allowed.
		return FailureClass{Kind: FailureFresh, Reason: ReasonSessionLost}
	}
	if delivery == worker.DeliveryNotSent {
		switch code {
		case worker.CodeUnavailable, worker.CodeRateLimited, worker.CodeBusy, worker.CodeClosed, worker.CodeCancelled, worker.CodeTimeout:
			return FailureClass{Kind: FailureRetry}
		}
	}
	return FailureClass{Kind: FailureTerminal}
}
