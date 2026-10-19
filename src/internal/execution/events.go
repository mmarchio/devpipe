package execution

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EventType is a bootstrap audit event type (WBS 15.104).
type EventType string

// Event types.
const (
	EventBootstrapRequested       EventType = "EXECUTION_BOOTSTRAP_REQUESTED"
	EventReadinessValidated       EventType = "EXECUTION_READINESS_VALIDATED"
	EventConfigurationSnapshotted EventType = "EXECUTION_CONFIGURATION_SNAPSHOTTED"
	EventStateFilesInitialized    EventType = "EXECUTION_STATE_FILES_INITIALIZED"
	EventRuntimeInitialized       EventType = "EXECUTION_RUNTIME_INITIALIZED"
	EventContextPrepared          EventType = "EXECUTION_CONTEXT_PREPARED"
	EventPromptPrepared           EventType = "EXECUTION_PROMPT_PREPARED"
	EventBootstrapCompleted       EventType = "EXECUTION_BOOTSTRAP_COMPLETED"
	EventBootstrapFailed          EventType = "EXECUTION_BOOTSTRAP_FAILED"
	EventStarted                  EventType = "EXECUTION_STARTED"
)

// Engine event types (WBS 16.81, P16-14).
const (
	EventStepIntent           EventType = "EXECUTION_STEP_INTENT"
	EventStepApplied          EventType = "EXECUTION_STEP_APPLIED"
	EventPlanActivated        EventType = "PLAN_ACTIVATED"
	EventPlanRejected         EventType = "PLAN_REJECTED"
	EventPlanAmended          EventType = "PLAN_AMENDED"
	EventUnitClaimed          EventType = "WORK_UNIT_CLAIMED"
	EventDispatchIntent       EventType = "WORKER_DISPATCH_INTENT"
	EventValidationStarted    EventType = "VALIDATION_STARTED"
	EventValidationPassed     EventType = "VALIDATION_PASSED"
	EventValidationFailed     EventType = "VALIDATION_FAILED"
	EventValidationInfraError EventType = "VALIDATION_INFRA_ERROR"
	EventFixAttempt           EventType = "FIX_ATTEMPT"
	EventCheckpointCreated    EventType = "CHECKPOINT_CREATED"
	EventCheckpointFailed     EventType = "CHECKPOINT_FAILED"
	EventCheckpointSkipped    EventType = "CHECKPOINT_SKIPPED"
	EventUnitDone             EventType = "WORK_UNIT_DONE"
	// EventUnitReplanned: the architect replanned a task the overseer's
	// validation failed the same way repeatedly.
	EventUnitReplanned         EventType = "WORK_UNIT_REPLANNED"
	EventPhaseValidationFailed EventType = "PHASE_VALIDATION_FAILED"
	EventPhaseDone             EventType = "PHASE_DONE"
	EventExecutionWaiting      EventType = "EXECUTION_WAITING"
	EventExecutionPaused       EventType = "EXECUTION_PAUSED"
	EventExecutionResumed      EventType = "EXECUTION_RESUMED"
	EventExecutionCancelled    EventType = "EXECUTION_CANCELLED"
	EventRecoveryRequired      EventType = "EXECUTION_RECOVERY_REQUIRED"
	EventRecoveryResolved      EventType = "EXECUTION_RECOVERY_RESOLVED"
	EventHardeningStarted      EventType = "HARDENING_STARTED"
	EventHardeningAnalyzed     EventType = "HARDENING_ANALYZED"
	EventHardeningOperation    EventType = "HARDENING_OPERATION"
	EventHardeningGroupDone    EventType = "HARDENING_GROUP_CONSOLIDATED"
	EventHardeningGroupSkipped EventType = "HARDENING_GROUP_SKIPPED"
	EventHardeningCompleted    EventType = "HARDENING_COMPLETED"
	EventFinalValidationPassed EventType = "FINAL_VALIDATION_PASSED"
	EventFinalValidationFailed EventType = "FINAL_VALIDATION_FAILED"
	EventProjectCompleted      EventType = "PROJECT_COMPLETED"
	// EventRepositoryHeadAccepted: an operator accepted an externally
	// changed repository HEAD / branch (WBS 18.50 / 18.51).
	EventRepositoryHeadAccepted EventType = "REPOSITORY_HEAD_ACCEPTED"
	// EventWorkerRunEvidence: repository state before / after a worker
	// run (WBS 18.62) — evidence, never a causal claim.
	EventWorkerRunEvidence EventType = "WORKER_RUN_REPOSITORY_EVIDENCE"
)

var allEvents = []EventType{
	EventBootstrapRequested, EventReadinessValidated, EventConfigurationSnapshotted,
	EventStateFilesInitialized, EventRuntimeInitialized, EventContextPrepared,
	EventPromptPrepared, EventBootstrapCompleted, EventBootstrapFailed, EventStarted,
	EventStepIntent, EventStepApplied, EventPlanActivated, EventPlanRejected,
	EventPlanAmended, EventUnitClaimed, EventDispatchIntent, EventValidationStarted,
	EventValidationPassed, EventValidationFailed, EventValidationInfraError,
	EventFixAttempt, EventCheckpointCreated, EventCheckpointFailed,
	EventCheckpointSkipped, EventUnitDone, EventPhaseValidationFailed, EventPhaseDone,
	EventExecutionWaiting, EventExecutionPaused, EventExecutionResumed,
	EventExecutionCancelled, EventRecoveryRequired, EventRecoveryResolved,
	EventHardeningStarted, EventHardeningAnalyzed, EventHardeningOperation,
	EventHardeningGroupDone, EventHardeningGroupSkipped, EventHardeningCompleted,
	EventFinalValidationPassed, EventFinalValidationFailed, EventProjectCompleted,
	EventRepositoryHeadAccepted, EventWorkerRunEvidence, EventUnitReplanned,
}

// AllEventTypes returns every type in declaration order.
func AllEventTypes() []EventType { return append([]EventType(nil), allEvents...) }

// ParseEventType accepts exactly one canonical type.
func ParseEventType(s string) (EventType, error) {
	for _, e := range allEvents {
		if string(e) == s {
			return e, nil
		}
	}
	return "", fmt.Errorf("execution: invalid event type %q", s)
}

// Event is one persisted audit event.
type Event struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	ExecutionID *uuid.UUID
	Type        EventType
	Payload     map[string]any
	// ExecSeq is the per-execution sequence (WBS 16.82); 0 for events
	// without an execution.
	ExecSeq int64
	// ProjectSeq is the per-project, commit-ordered sequence (WBS 17.76).
	ProjectSeq int64
	CreatedAt  time.Time
}

// Metrics are derived from committed events (WBS 15.106).
type Metrics struct {
	Attempts          int            `json:"bootstrap_attempts"`
	Successes         int            `json:"bootstrap_successes"`
	Failures          int            `json:"bootstrap_failures"`
	ReadinessFailures int            `json:"readiness_failures"`
	Retries           int            `json:"bootstrap_retries"`
	FailuresByCode    map[string]int `json:"failures_by_code"`
	DurationTotalMS   int64          `json:"bootstrap_duration_total_ms"`
	DurationMaxMS     int64          `json:"bootstrap_duration_max_ms"`
}

// AggregateMetrics counts outcomes; payload keys used: "retry" (bool) on
// REQUESTED, "code" + "stage" on FAILED. Duration is the first REQUESTED →
// COMPLETED per execution. Events must be chronological.
func AggregateMetrics(events []Event) Metrics {
	m := Metrics{FailuresByCode: map[string]int{}}
	firstReq := map[uuid.UUID]time.Time{}
	for _, e := range events {
		switch e.Type {
		case EventBootstrapRequested:
			m.Attempts++
			if b, _ := e.Payload["retry"].(bool); b {
				m.Retries++
			}
		case EventReadinessValidated:
			if e.ExecutionID != nil {
				if _, ok := firstReq[*e.ExecutionID]; !ok {
					firstReq[*e.ExecutionID] = e.CreatedAt
				}
			}
		case EventBootstrapCompleted:
			m.Successes++
			if e.ExecutionID != nil {
				if t, ok := firstReq[*e.ExecutionID]; ok {
					d := e.CreatedAt.Sub(t).Milliseconds()
					m.DurationTotalMS += d
					if d > m.DurationMaxMS {
						m.DurationMaxMS = d
					}
				}
			}
		case EventBootstrapFailed:
			m.Failures++
			code, _ := e.Payload["code"].(string)
			m.FailuresByCode[code]++
			if st, _ := e.Payload["stage"].(string); st == string(StageValidating) {
				m.ReadinessFailures++
			}
		}
	}
	return m
}
