package execution

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/state"
	"overseer/internal/wbs"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// StepAction is the one deterministic orchestration action a step performs
// at the runtime's current state (WBS 16.1).
type StepAction string

// Step actions.
const (
	// Statefile workflow: a command state sends its command; the check
	// states read the statefiles the team wrote.
	ActionCommand           StepAction = "COMMAND"
	ActionImplementCheck    StepAction = "IMPLEMENT_CHECK"
	ActionTestCheck         StepAction = "TEST_CHECK"
	ActionAcceptanceCheck   StepAction = "ACCEPTANCE_CHECK"
	ActionPlanDispatch      StepAction = "PLAN_DISPATCH"
	ActionPlanValidate      StepAction = "PLAN_VALIDATE"
	ActionSelectUnit        StepAction = "SELECT_UNIT"
	ActionDispatch          StepAction = "DISPATCH"
	ActionEvaluateResult    StepAction = "EVALUATE_RESULT"
	ActionContinuation      StepAction = "CONTINUATION"
	ActionRunUnitValidation StepAction = "RUN_UNIT_VALIDATION"
	ActionUnitGates         StepAction = "UNIT_GATES"
	ActionMoreWork          StepAction = "MORE_WORK"
	ActionPhaseValidation   StepAction = "PHASE_VALIDATION"
	ActionMorePhases        StepAction = "MORE_PHASES"
	ActionFinalHardening    StepAction = "FINAL_HARDENING"
	ActionHardeningFix      StepAction = "HARDENING_FIX"
	ActionFinalValidation   StepAction = "FINAL_VALIDATION"
	ActionFinalCheckpoint   StepAction = "FINAL_CHECKPOINT"
	ActionControl           StepAction = "CONTROL"
	ActionComplete          StepAction = "COMPLETE"
)

var allActions = []StepAction{
	ActionCommand, ActionImplementCheck, ActionTestCheck, ActionAcceptanceCheck,
	ActionPlanDispatch, ActionPlanValidate, ActionSelectUnit, ActionDispatch,
	ActionEvaluateResult, ActionContinuation, ActionRunUnitValidation,
	ActionUnitGates, ActionMoreWork, ActionPhaseValidation, ActionMorePhases,
	ActionFinalHardening, ActionHardeningFix, ActionFinalValidation,
	ActionFinalCheckpoint, ActionControl, ActionComplete,
}

// AllStepActions returns every action in declaration order.
func AllStepActions() []StepAction { return append([]StepAction(nil), allActions...) }

// ParseStepAction accepts exactly one canonical action.
func ParseStepAction(s string) (StepAction, error) {
	for _, a := range allActions {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("execution: invalid step action %q", s)
}

// StepStatus is the durable state of one step record (WBS 16.8).
type StepStatus string

// Step statuses: INTENT is committed before any external action; APPLIED /
// FAILED record the result; ABANDONED marks an intent superseded during
// recovery.
const (
	StepIntent    StepStatus = "INTENT"
	StepApplied   StepStatus = "APPLIED"
	StepFailed    StepStatus = "FAILED"
	StepAbandoned StepStatus = "ABANDONED"
)

// StepOutcome is what one Step call achieved (WBS 16.1).
type StepOutcome string

// Step outcomes.
const (
	StepProgressed       StepOutcome = "PROGRESSED"
	StepWaiting          StepOutcome = "WAITING"
	StepPaused           StepOutcome = "PAUSED"
	StepRecoveryRequired StepOutcome = "RECOVERY_REQUIRED"
	StepExecutionFailed  StepOutcome = "FAILED"
	StepCancelled        StepOutcome = "CANCELLED"
	StepCompleted        StepOutcome = "COMPLETED"
	StepNoop             StepOutcome = "NOOP"
)

// StopsRun reports an outcome after which Run stops (WBS 16.1: waits,
// pauses, requires recovery, fails, is cancelled, completes). NOOP stops
// too: nothing changed, so repeating would spin.
func (o StepOutcome) StopsRun() bool { return o != StepProgressed }

// StepResult is returned by Engine.Step.
type StepResult struct {
	Outcome StepOutcome `json:"outcome"`
	Action  StepAction  `json:"action,omitempty"`
	State   string      `json:"state,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	StepSeq int64       `json:"step_seq,omitempty"`
}

// IdempotencyKey identifies one logical step: the same runtime position
// (history sequence), action and attempt always yield the same key, so a
// repeated step is detected instead of duplicated (WBS 16.83).
func IdempotencyKey(executionID uuid.UUID, historySeq int64, action StepAction, attempt int) string {
	return fmt.Sprintf("%s:%d:%s:%d", executionID, historySeq, action, attempt)
}

// actionBindings binds the default-workflow state keys (PLAN Decision 4).
var actionBindings = map[string]StepAction{
	"PLAN_CHECK":        ActionPlanValidate,
	"IMPLEMENT_CHECK":   ActionImplementCheck,
	"TEST_CHECK":        ActionTestCheck,
	"ACCEPTANCE_CHECK":  ActionAcceptanceCheck,
	"PHASE_PLAN":        ActionPlanDispatch,
	"PLAN_VALIDATE":     ActionPlanValidate,
	"WORK_UNIT_PREPARE": ActionSelectUnit,
	"IMPLEMENT":         ActionDispatch,
	"FIX":               ActionDispatch,
	"CONTINUE":          ActionDispatch,
	"RESPONSE_EVALUATE": ActionEvaluateResult,
	"CONTINUE_DECISION": ActionContinuation,
	"SAME_SESSION":      ActionContinuation,
	"RESET_SESSION":     ActionContinuation,
	"VALIDATE":          ActionRunUnitValidation,
	"UNIT_DONE":         ActionUnitGates,
	"MORE_WORK":         ActionMoreWork,
	"PHASE_VALIDATE":    ActionPhaseValidation,
	"MORE_PHASES":       ActionMorePhases,
	"FINAL_HARDENING":   ActionFinalHardening,
	"HARDENING_FIX":     ActionHardeningFix,
	"FINAL_VALIDATE":    ActionFinalValidation,
	"FINAL_CHECKPOINT":  ActionFinalCheckpoint,
	"PROJECT_DONE":      ActionComplete,
}

// CommandTemplatePrefix marks a command template: its state sends the
// template text (with {{command.*}} values) to the worker as is, without
// prompt composition.
const CommandTemplatePrefix = "CMD_"

// IsCommandTemplate reports a command template key.
func IsCommandTemplate(key string) bool { return strings.HasPrefix(key, CommandTemplatePrefix) }

// ResolveAction maps the runtime's current state onto the engine action
// (WBS 16.1): a built-in binding for default-workflow keys, else by kind
// (ACTION → DISPATCH, VALIDATION → RUN_UNIT_VALIDATION, TERMINAL →
// COMPLETE, everything else → CONTROL). It reads only the persisted
// machine snapshot, never browser or provider state.
func ResolveAction(s state.StateSnapshot) StepAction {
	if s.Terminal || s.Kind == string(state.KindTerminal) {
		return ActionComplete
	}
	if a, ok := actionBindings[s.Key]; ok {
		return a
	}
	switch s.Kind {
	case string(state.KindAction):
		return ActionDispatch
	case string(state.KindValidation):
		return ActionRunUnitValidation
	}
	return ActionControl
}

// ResultToken is the engine's classification of a dispatch outcome
// (WBS 16.31–16.42, 16.75).
type ResultToken string

// Result tokens.
const (
	TokenComplete        ResultToken = "COMPLETE"
	TokenIncomplete      ResultToken = "INCOMPLETE"
	TokenBlocked         ResultToken = "BLOCKED"
	TokenFailed          ResultToken = "FAILED"
	TokenProtocolError   ResultToken = "PROTOCOL_ERROR"
	TokenContextLimit    ResultToken = "CONTEXT_LIMIT"
	TokenDeliveryUnknown ResultToken = "DELIVERY_UNKNOWN"
	TokenTransportFailed ResultToken = "TRANSPORT_FAILED"
)

// DispatchOutcome is what the engine knows about a finished dispatch.
type DispatchOutcome struct {
	// Error is the normalized worker error of a dispatch that did not
	// complete (nil = a response was received).
	Error *worker.WorkerError
	// ProtocolFailure: the response could not be parsed into a
	// WorkerResult (Phase 13).
	ProtocolFailure bool
	// Status is the parsed result status (valid when Error == nil and
	// !ProtocolFailure).
	Status workerresult.WorkerResultStatus
}

// MapDispatchOutcome classifies a dispatch outcome. The engine consumes
// the WorkerResult status, never response prose (WBS 16.31). A protocol
// failure is distinct from a worker-reported FAILED (16.42); a context
// limit is not a failure (16.37); unknown delivery is its own token
// (16.75).
func MapDispatchOutcome(o DispatchOutcome) ResultToken {
	if o.Error != nil {
		switch {
		case o.Error.Code == worker.CodeContextLimit:
			return TokenContextLimit
		case o.Error.Delivery == worker.DeliveryUnknown:
			return TokenDeliveryUnknown
		}
		return TokenTransportFailed
	}
	if o.ProtocolFailure {
		return TokenProtocolError
	}
	switch o.Status {
	case workerresult.StatusComplete:
		return TokenComplete
	case workerresult.StatusIncomplete:
		return TokenIncomplete
	case workerresult.StatusBlocked:
		return TokenBlocked
	case workerresult.StatusFailed:
		return TokenFailed
	}
	return TokenProtocolError
}

// RouteDecision is where a result token leads: a machine transition, or —
// when the workflow has no edge for it — a recovery reason.
type RouteDecision struct {
	Transition *state.TransitionSnapshot
	Recovery   string
}

// RouteToken resolves a result token against the machine (WBS 16.40/16.41):
// CONTEXT_LIMIT routes as INCOMPLETE; every token follows a configured
// edge from `from` when one exists; otherwise the execution requires
// recovery. A token is never converted into a terminal FAILED / COMPLETED
// project outcome by the engine itself.
func RouteToken(m *state.StateMachineSnapshot, from string, tok ResultToken) (RouteDecision, error) {
	outcome := string(tok)
	if tok == TokenContextLimit {
		outcome = string(TokenIncomplete)
	}
	if m == nil {
		return RouteDecision{Recovery: "no state machine"}, nil
	}
	t, err := selectSnapshotTransition(m.Transitions, from, outcome)
	if err != nil {
		return RouteDecision{}, err
	}
	if t == nil {
		return RouteDecision{Recovery: fmt.Sprintf("workflow has no %s transition from %s", outcome, from)}, nil
	}
	return RouteDecision{Transition: t}, nil
}

// InvariantFacts are the persisted facts invariants are checked against
// (WBS 16.84/16.85).
type InvariantFacts struct {
	// PositionPhase is the execution's recorded current phase.
	PositionPhase *phase.Phase
	// CurrentPhase is the lowest-order non-DONE phase.
	CurrentPhase *phase.Phase
	// Plan is the current phase's active plan.
	Plan              *wbs.ExecutionPlan
	CurrentWBSVersion int
	// PositionUnit is the execution's recorded current unit.
	PositionUnit *wbs.WorkUnit
	// PlanUnits are the active plan's units.
	PlanUnits []*wbs.WorkUnit
	// OpenDispatches counts non-terminal dispatches per unit.
	OpenDispatches map[uuid.UUID]int
	// UnitAction: the step acts on the current unit (dispatch,
	// validation, gates) — those require a current plan.
	UnitAction bool
}

// CheckInvariants reports every violated execution invariant (WBS 16.84):
// one current phase, the current unit belongs to the current plan, at most
// one open dispatch chain per unit, a unit action requires the plan to
// match the current WBS, and a DONE unit claimed by an execution carries
// its PASS validation. Violations block further side effects.
func CheckInvariants(f InvariantFacts) []string {
	var v []string
	if f.PositionPhase != nil && f.PositionPhase.Status != phase.StatusDONE &&
		(f.CurrentPhase == nil || f.CurrentPhase.ID != f.PositionPhase.ID) {
		v = append(v, fmt.Sprintf("execution phase %s is not the current phase", f.PositionPhase.ID))
	}
	if f.PositionUnit != nil && f.UnitAction {
		if f.Plan == nil || f.PositionUnit.ExecutionPlanID != f.Plan.ID {
			v = append(v, fmt.Sprintf("current unit %s is not in the current plan", f.PositionUnit.ID))
		}
		if f.Plan != nil && f.Plan.WBSVersion != f.CurrentWBSVersion {
			v = append(v, fmt.Sprintf("current plan was built from WBS v%d but the current WBS is v%d", f.Plan.WBSVersion, f.CurrentWBSVersion))
		}
	}
	ids := make([]uuid.UUID, 0, len(f.OpenDispatches))
	for id := range f.OpenDispatches {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		if f.OpenDispatches[id] > 1 {
			v = append(v, fmt.Sprintf("unit %s has %d open dispatches", id, f.OpenDispatches[id]))
		}
	}
	for _, u := range f.PlanUnits {
		if u != nil && u.Status == wbs.WorkUnitStatusDONE && u.ClaimedExecutionID != nil && u.DoneValidationRunID == nil {
			v = append(v, fmt.Sprintf("unit %s is DONE without a passing validation", u.ID))
		}
	}
	return v
}

// PhaseMayComplete reports whether a phase may become DONE: its current
// plan exists and every unit of it is DONE (WBS 16.85).
func PhaseMayComplete(plan *wbs.ExecutionPlan, units []*wbs.WorkUnit) bool {
	return wbs.PhaseCompletionEligible(plan, units)
}

// AttemptVerdict decides whether another bounded attempt may start.
type AttemptVerdict string

// Attempt verdicts.
const (
	AttemptAllow     AttemptVerdict = "ALLOW"
	AttemptExhausted AttemptVerdict = "EXHAUSTED"
)

// AttemptDecision bounds automatic repetition (WBS 16.38, 16.51, 16.132):
// `made` attempts already happened; another is allowed while made < max.
func AttemptDecision(made, max int) AttemptVerdict {
	if made < max {
		return AttemptAllow
	}
	return AttemptExhausted
}

// SchedulingCandidate is one schedulable execution.
type SchedulingCandidate struct {
	ExecutionID     uuid.UUID
	ProjectID       uuid.UUID
	LastScheduledAt *time.Time
}

// SelectSchedulingBatch picks up to max executions fairly (WBS 16.7):
// never-scheduled first, then the least recently scheduled; one execution
// per project; ties by execution id. A project that just ran goes to the
// back of the queue, so every runnable project gets bounded opportunities.
func SelectSchedulingBatch(cands []SchedulingCandidate, max int) []SchedulingCandidate {
	cs := append([]SchedulingCandidate(nil), cands...)
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i].LastScheduledAt, cs[j].LastScheduledAt
		switch {
		case a == nil && b != nil:
			return true
		case a != nil && b == nil:
			return false
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		}
		return cs[i].ExecutionID.String() < cs[j].ExecutionID.String()
	})
	seen := map[uuid.UUID]bool{}
	var out []SchedulingCandidate
	for _, c := range cs {
		if len(out) >= max {
			break
		}
		if seen[c.ProjectID] {
			continue
		}
		seen[c.ProjectID] = true
		out = append(out, c)
	}
	return out
}

// RepoCheckpointKind classifies an overseer-owned git checkpoint.
type RepoCheckpointKind string

// Checkpoint kinds.
const (
	RepoCheckpointUnit              RepoCheckpointKind = "UNIT"
	RepoCheckpointHardeningBaseline RepoCheckpointKind = "HARDENING_BASELINE"
	RepoCheckpointHardeningGroup    RepoCheckpointKind = "HARDENING_GROUP"
	RepoCheckpointFinal             RepoCheckpointKind = "FINAL"
)

// Valid reports a known kind.
func (k RepoCheckpointKind) Valid() bool {
	switch k {
	case RepoCheckpointUnit, RepoCheckpointHardeningBaseline, RepoCheckpointHardeningGroup, RepoCheckpointFinal:
		return true
	}
	return false
}

// Commit trailers identifying overseer checkpoints (PLAN Decision 12).
const (
	TrailerExecution  = "Overseer-Execution"
	TrailerCheckpoint = "Overseer-Checkpoint"
)

// CheckpointKey is the deterministic identity of one checkpoint: the same
// execution, kind, target and attempt always produce the same key, so a
// crash after commit is reconciled by key instead of committing twice
// (WBS 16.80, 16.143).
func CheckpointKey(executionID uuid.UUID, kind RepoCheckpointKind, target uuid.UUID, attempt int) string {
	return fmt.Sprintf("%s/%s/%s/%d", executionID, strings.ToLower(string(kind)), target, attempt)
}

// CheckpointMessage renders the commit message with identifying trailers.
func CheckpointMessage(kind RepoCheckpointKind, key string, executionID uuid.UUID, subject string) string {
	return fmt.Sprintf("overseer: %s checkpoint: %s\n\n%s: %s\n%s: %s\n",
		strings.ToLower(string(kind)), subject, TrailerExecution, executionID, TrailerCheckpoint, key)
}

// CheckpointFailureAction is what a checkpoint failure means for
// progression.
type CheckpointFailureAction string

// Failure actions.
const (
	CheckpointBlock   CheckpointFailureAction = "BLOCK"
	CheckpointProceed CheckpointFailureAction = "PROCEED_RECORDED"
)

// OnCheckpointFailure applies the policy (WBS 16.55/16.56): REQUIRED
// blocks progression; BEST_EFFORT proceeds with the failure recorded.
// DISABLED never attempts a checkpoint, so it never fails.
func OnCheckpointFailure(p CheckpointPolicy) CheckpointFailureAction {
	if p == CheckpointBestEffort || p == CheckpointDisabled {
		return CheckpointProceed
	}
	return CheckpointBlock
}
