package execution

import "strings"

// ---------------------------------------------------------------------------
// Backend-decided UI actions (WBS 17.164, 17.96, 17.98–17.100). The web
// console renders only what these functions return; it never derives
// legality itself.

// ControlAction is a lifecycle / engine control the console may offer.
type ControlAction string

const (
	ControlStart  ControlAction = "START"
	ControlPause  ControlAction = "PAUSE"
	ControlResume ControlAction = "RESUME"
	ControlCancel ControlAction = "CANCEL"
	// ControlRequeue restarts the current task from the beginning.
	ControlRequeue         ControlAction = "REQUEUE"
	ControlStep            ControlAction = "STEP"
	ControlResolveRecovery ControlAction = "RESOLVE_RECOVERY"
	// ControlPauseAfterTask pauses once the next task is loaded, before
	// work on it begins; ControlCancelPauseAfterTask withdraws the request.
	ControlPauseAfterTask       ControlAction = "PAUSE_AFTER_TASK"
	ControlCancelPauseAfterTask ControlAction = "CANCEL_PAUSE_AFTER_TASK"
)

// ControlFacts are the persisted facts AllowedActions decides from.
// ProjectStatus is the project lifecycle status string (Phase 3).
type ControlFacts struct {
	ProjectStatus   string
	HasExecution    bool
	ExecutionStatus ExecutionStatus
	ReadinessOK     bool
	// Requeueable: the execution runs the statefile workflow and has a
	// current task that is not DONE.
	Requeueable bool
	// Statefile: the execution runs the statefile workflow.
	Statefile bool
	// PauseAfterTask: a pause after the current task is requested.
	PauseAfterTask bool
}

// AllowedActions returns the ordered control actions legal for the facts
// (WBS 17.164). RECOVERY_REQUIRED suppresses Start / Resume / Step and
// offers recovery resolution instead (17.96 / 17.172); terminal projects
// and executions offer nothing.
func AllowedActions(f ControlFacts) []ControlAction {
	var out []ControlAction
	recovery := f.HasExecution && f.ExecutionStatus == ExecutionRecovery
	execActive := f.HasExecution && f.ExecutionStatus.IsActive()
	switch f.ProjectStatus {
	case "READY":
		if !execActive && f.ReadinessOK {
			out = append(out, ControlStart)
		}
		out = append(out, ControlCancel)
	case "DRAFT":
		out = append(out, ControlCancel)
	case "RUNNING":
		if !recovery {
			out = append(out, ControlPause)
		}
		if execActive && !recovery && f.Statefile {
			if f.PauseAfterTask {
				out = append(out, ControlCancelPauseAfterTask)
			} else {
				out = append(out, ControlPauseAfterTask)
			}
		}
		if execActive && !recovery && (f.ExecutionStatus == ExecutionRunning || f.ExecutionStatus == ExecutionWaiting) {
			out = append(out, ControlStep)
		}
		out = append(out, ControlCancel)
	case "PAUSED":
		if !recovery {
			out = append(out, ControlResume)
		}
		if execActive && f.Statefile && f.PauseAfterTask {
			out = append(out, ControlCancelPauseAfterTask)
		}
		out = append(out, ControlCancel)
	}
	live := false
	if f.HasExecution {
		switch f.ExecutionStatus {
		case ExecutionRunning, ExecutionWaiting, ExecutionPaused, ExecutionRecovery:
			live = true
		}
	}
	if f.Requeueable && live && (f.ProjectStatus == "RUNNING" || f.ProjectStatus == "PAUSED") {
		// Offered next to Cancel.
		for i, a := range out {
			if a == ControlCancel {
				out = append(out[:i+1], append([]ControlAction{ControlRequeue}, out[i+1:]...)...)
				break
			}
		}
	}
	if recovery && (f.ProjectStatus == "RUNNING" || f.ProjectStatus == "PAUSED") {
		out = append(out, ControlResolveRecovery)
	}
	return out
}

// RecoveryActionKind is a typed recovery operation (WBS 17.98, 17.102).
type RecoveryActionKind string

const (
	// RecoveryReconcileDispatch reconciles the worker session / dispatch
	// state through Phase 12 (never sends a request).
	RecoveryReconcileDispatch RecoveryActionKind = "RECONCILE_DISPATCH"
	// RecoveryRunValidation re-runs the interrupted validation step.
	RecoveryRunValidation RecoveryActionKind = "RUN_VALIDATION"
	// RecoveryRetryCheckpoint re-attempts the failed checkpoint step.
	RecoveryRetryCheckpoint RecoveryActionKind = "RETRY_CHECKPOINT"
	// RecoveryApproveCriteria records manual approval of acceptance
	// criteria awaiting review.
	RecoveryApproveCriteria RecoveryActionKind = "APPROVE_CRITERIA"
	// RecoveryManualResume returns the execution to RUNNING; the next step
	// re-evaluates persisted state (an unknown delivery is still never
	// resent).
	RecoveryManualResume RecoveryActionKind = "MANUAL_RESUME"
	// RecoveryValidateRepositoryState runs the unit's deterministic
	// validation against the current repository as evidence only
	// (WBS 18.23); it never changes the dispatch's delivery status.
	RecoveryValidateRepositoryState RecoveryActionKind = "VALIDATE_REPOSITORY_STATE"
	// RecoveryResolveDelivery records the operator's reviewed resolution
	// of an unknown delivery (RESEND_AFTER_REVIEW / ACCEPT_REPOSITORY_STATE,
	// PLAN P18 Decision 8). Never automatic.
	RecoveryResolveDelivery RecoveryActionKind = "RESOLVE_DELIVERY"
	// RecoveryAcceptRepositoryHead accepts an externally changed
	// repository HEAD as the new expected state (WBS 18.50).
	RecoveryAcceptRepositoryHead RecoveryActionKind = "ACCEPT_REPOSITORY_HEAD"
)

// Delivery resolutions (RESOLVE_DELIVERY).
const (
	ResolutionResendAfterReview     = "RESEND_AFTER_REVIEW"
	ResolutionAcceptRepositoryState = "ACCEPT_REPOSITORY_STATE"
)

// RecoveryAction is one offered operation.
type RecoveryAction struct {
	Kind RecoveryActionKind `json:"kind"`
	// HighImpact actions require explicit confirmation and a reason
	// (WBS 17.100).
	HighImpact bool   `json:"high_impact"`
	Label      string `json:"label"`
}

// RecoveryFacts are the persisted recovery facts: the recorded reason and
// evidence of the RECOVERY_REQUIRED execution.
type RecoveryFacts struct {
	Reason string
	// StepAction is the engine action that stopped (evidence "action").
	StepAction StepAction
	// Token is the dispatch outcome token (evidence "token").
	Token ResultToken
}

// RecoveryFactsFrom reads the facts from a recovery reason + evidence map.
func RecoveryFactsFrom(reason string, evidence map[string]any) RecoveryFacts {
	f := RecoveryFacts{Reason: reason}
	if s, ok := evidence["action"].(string); ok {
		f.StepAction = StepAction(s)
	}
	if s, ok := evidence["token"].(string); ok {
		f.Token = ResultToken(s)
	}
	return f
}

var recoveryLabels = map[RecoveryActionKind]string{
	RecoveryReconcileDispatch:       "Reconcile Dispatch",
	RecoveryRunValidation:           "Run Validation",
	RecoveryRetryCheckpoint:         "Retry Checkpoint",
	RecoveryApproveCriteria:         "Approve Criteria (Manual Review)",
	RecoveryManualResume:            "Resume After Manual Review",
	RecoveryValidateRepositoryState: "Validate Repository State (evidence only)",
	RecoveryResolveDelivery:         "Resolve Unknown Delivery",
	RecoveryAcceptRepositoryHead:    "Accept Repository HEAD",
}

// RecoveryActionOf returns the presentation of one action kind.
func RecoveryActionOf(k RecoveryActionKind) RecoveryAction { return recoveryAction(k) }

// KnownRecoveryAction reports whether k is an operator action kind.
func KnownRecoveryAction(k RecoveryActionKind) bool { _, ok := recoveryLabels[k]; return ok }

func recoveryAction(k RecoveryActionKind) RecoveryAction {
	high := k == RecoveryApproveCriteria || k == RecoveryManualResume || k == RecoveryResolveDelivery || k == RecoveryAcceptRepositoryHead
	return RecoveryAction{Kind: k, HighImpact: high, Label: recoveryLabels[k]}
}

// RecoveryActions returns the backend-approved recovery operations for the
// facts (WBS 17.98 / 18.98). An unknown delivery offers reconciliation,
// evidence-only repository validation and the explicit, confirmed
// operator resolution (Decision 8) — never a blind retry (17.99), and not
// MANUAL_RESUME (it would only re-evaluate the same ambiguous dispatch).
func RecoveryActions(f RecoveryFacts) []RecoveryAction {
	reason := strings.ToLower(f.Reason)
	var kinds []RecoveryActionKind
	switch {
	case f.Token == TokenDeliveryUnknown || strings.Contains(reason, "delivery unknown"):
		out := []RecoveryAction{}
		for _, k := range []RecoveryActionKind{RecoveryReconcileDispatch, RecoveryValidateRepositoryState, RecoveryResolveDelivery} {
			out = append(out, recoveryAction(k))
		}
		return out
	case strings.Contains(reason, "repository head") || strings.Contains(reason, "repository branch"):
		kinds = []RecoveryActionKind{RecoveryAcceptRepositoryHead}
	case strings.Contains(reason, "manual review"):
		kinds = []RecoveryActionKind{RecoveryApproveCriteria}
	case strings.Contains(reason, "checkpoint"):
		kinds = []RecoveryActionKind{RecoveryRetryCheckpoint}
	case f.StepAction == ActionRunUnitValidation || f.StepAction == ActionPhaseValidation || f.StepAction == ActionFinalValidation ||
		strings.Contains(reason, "validation could not run"):
		kinds = []RecoveryActionKind{RecoveryRunValidation}
	}
	kinds = append(kinds, RecoveryManualResume)
	out := make([]RecoveryAction, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, recoveryAction(k))
	}
	return out
}

// EventCategory groups execution event types for timeline filtering
// (WBS 17.81).
type EventCategory string

const (
	EventCatPlanning   EventCategory = "planning"
	EventCatWorker     EventCategory = "worker"
	EventCatValidation EventCategory = "validation"
	EventCatCheckpoint EventCategory = "checkpoint"
	EventCatPhase      EventCategory = "phase"
	EventCatRecovery   EventCategory = "recovery"
	EventCatSystem     EventCategory = "system"
)

// EventCategories lists the categories in display order.
func EventCategories() []EventCategory {
	return []EventCategory{EventCatPlanning, EventCatWorker, EventCatValidation, EventCatCheckpoint, EventCatPhase, EventCatRecovery, EventCatSystem}
}

// ParseEventCategory accepts one category.
func ParseEventCategory(s string) (EventCategory, bool) {
	for _, c := range EventCategories() {
		if string(c) == s {
			return c, true
		}
	}
	return "", false
}

// CategoryOf returns the category of an event type; every type maps to
// exactly one category (unknown types are system).
func CategoryOf(t EventType) EventCategory {
	switch t {
	case EventPlanActivated, EventPlanRejected, EventPlanAmended:
		return EventCatPlanning
	case EventUnitClaimed, EventDispatchIntent, EventExecutionWaiting, EventUnitDone, EventUnitRequeueRequested, EventUnitRequeued,
		EventPauseAfterTaskRequested, EventPauseAfterTaskCancelled, EventPauseAfterTaskApplied:
		return EventCatWorker
	case EventValidationStarted, EventValidationPassed, EventValidationFailed, EventValidationInfraError, EventFixAttempt,
		EventFinalValidationPassed, EventFinalValidationFailed:
		return EventCatValidation
	case EventCheckpointCreated, EventCheckpointFailed, EventCheckpointSkipped:
		return EventCatCheckpoint
	case EventPhaseValidationFailed, EventPhaseDone, EventProjectCompleted,
		EventHardeningStarted, EventHardeningAnalyzed, EventHardeningOperation, EventHardeningGroupDone,
		EventHardeningGroupSkipped, EventHardeningCompleted:
		return EventCatPhase
	case EventRecoveryRequired, EventRecoveryResolved, EventBootstrapFailed,
		EventUnitAnalysisStarted, EventUnitAnalyzed, EventUnitFixStarted, EventUnitFixed, EventUnitRepeatDetected:
		return EventCatRecovery
	}
	return EventCatSystem
}

// TypesInCategories returns every event type belonging to one of cats.
func TypesInCategories(cats []EventCategory) []EventType {
	want := map[EventCategory]bool{}
	for _, c := range cats {
		want[c] = true
	}
	var out []EventType
	for _, t := range allEvents {
		if want[CategoryOf(t)] {
			out = append(out, t)
		}
	}
	return out
}
