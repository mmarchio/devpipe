// Package recovery is the pure recovery model of Phase 18 (WBS 18.7–18.11,
// 18.16, 18.211, 18.223): conditions discovered from durable evidence are
// classified, and a deterministic Plan lists the safe and unsafe actions.
// Planning never mutates anything; executing an action is a separate,
// idempotent operation owned by the services.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Classification is the recovery class of a condition (WBS 18.7).
type Classification string

const (
	SafeToResume           Classification = "SAFE_TO_RESUME"
	SafeToRetry            Classification = "SAFE_TO_RETRY"
	ReconciliationNeeded   Classification = "RECONCILIATION_REQUIRED"
	ManualReviewRequired   Classification = "MANUAL_REVIEW_REQUIRED"
	TerminalClassification Classification = "TERMINAL"
)

// Condition is a discovered recovery condition.
type Condition string

// Conditions (PLAN P18-04).
const (
	Resumable                  Condition = "RESUMABLE"
	ExecutionTerminal          Condition = "EXECUTION_TERMINAL"
	ExpiredLease               Condition = "EXPIRED_LEASE"
	InflightDispatch           Condition = "INFLIGHT_DISPATCH"
	DispatchNotSent            Condition = "DISPATCH_NOT_SENT"
	DeliveryUnknown            Condition = "DELIVERY_UNKNOWN"
	ResponseUnpersisted        Condition = "RESPONSE_UNPERSISTED"
	RunningValidation          Condition = "RUNNING_VALIDATION"
	ValidationInfra            Condition = "VALIDATION_INFRA"
	PendingCheckpoint          Condition = "PENDING_CHECKPOINT"
	CheckpointAmbiguous        Condition = "CHECKPOINT_AMBIGUOUS"
	CheckpointRejected         Condition = "CHECKPOINT_REJECTED"
	RepositoryMissing          Condition = "REPOSITORY_MISSING"
	RepositoryIdentityMismatch Condition = "REPOSITORY_IDENTITY_MISMATCH"
	RepositoryHeadChanged      Condition = "REPOSITORY_HEAD_CHANGED"
	RepositoryPermission       Condition = "REPOSITORY_PERMISSION"
	DiskFull                   Condition = "DISK_FULL"
	ProtocolViolation          Condition = "PROTOCOL_VIOLATION"
	ManualCriteria             Condition = "MANUAL_CRITERIA"
	InvariantViolation         Condition = "INVARIANT_VIOLATION"
	ProviderUnavailable        Condition = "PROVIDER_UNAVAILABLE"
	PromptTooLarge             Condition = "PROMPT_TOO_LARGE"
	ArtifactCorrupt            Condition = "ARTIFACT_CORRUPT"
	ConfigDrift                Condition = "CONFIG_DRIFT"
	Panic                      Condition = "PANIC"
	OrphanPrecompute           Condition = "ORPHAN_PRECOMPUTE"
	// ManualReview is a recovery condition without a more specific class.
	ManualReview Condition = "MANUAL_REVIEW"
)

// Action is a recovery operation (automatic or operator).
type Action string

// Actions. Operator actions mirror the Phase 17 typed recovery set plus
// the Phase 18 additions; automatic ones are engine operations.
const (
	ActResume                  Action = "RESUME"
	ActWait                    Action = "WAIT"
	ActResend                  Action = "RESEND"
	ActReconcileDispatch       Action = "RECONCILE_DISPATCH"
	ActRunValidation           Action = "RUN_VALIDATION"
	ActRetryCheckpoint         Action = "RETRY_CHECKPOINT"
	ActApproveCriteria         Action = "APPROVE_CRITERIA"
	ActManualResume            Action = "MANUAL_RESUME"
	ActValidateRepositoryState Action = "VALIDATE_REPOSITORY_STATE"
	ActResolveDelivery         Action = "RESOLVE_DELIVERY"
	ActAcceptRepositoryHead    Action = "ACCEPT_REPOSITORY_HEAD"
	ActDiscardSpeculative      Action = "DISCARD_SPECULATIVE"
)

// rule is the static policy of one condition.
type rule struct {
	class    Classification
	safe     []Action
	unsafe   []Action
	auto     Action
	approval bool
}

var rules = map[Condition]rule{
	Resumable:                  {class: SafeToResume, safe: []Action{ActResume}, auto: ActResume},
	ExecutionTerminal:          {class: TerminalClassification},
	ExpiredLease:               {class: SafeToResume, safe: []Action{ActResume}, auto: ActResume},
	InflightDispatch:           {class: ReconciliationNeeded, safe: []Action{ActReconcileDispatch, ActWait}, unsafe: []Action{ActResend}, auto: ActReconcileDispatch},
	DispatchNotSent:            {class: SafeToRetry, safe: []Action{ActResend}, auto: ActResend},
	DeliveryUnknown:            {class: ReconciliationNeeded, safe: []Action{ActReconcileDispatch, ActValidateRepositoryState, ActResolveDelivery}, unsafe: []Action{ActResend, ActManualResume}, approval: true},
	ResponseUnpersisted:        {class: ReconciliationNeeded, safe: []Action{ActReconcileDispatch, ActValidateRepositoryState, ActResolveDelivery}, unsafe: []Action{ActResend}, approval: true},
	RunningValidation:          {class: SafeToRetry, safe: []Action{ActRunValidation}, auto: ActRunValidation},
	ValidationInfra:            {class: SafeToRetry, safe: []Action{ActRunValidation, ActManualResume}, auto: ActRunValidation},
	PendingCheckpoint:          {class: ReconciliationNeeded, safe: []Action{ActRetryCheckpoint}, auto: ActRetryCheckpoint},
	CheckpointAmbiguous:        {class: ManualReviewRequired, safe: []Action{ActAcceptRepositoryHead, ActManualResume}, unsafe: []Action{ActRetryCheckpoint}, approval: true},
	CheckpointRejected:         {class: ManualReviewRequired, safe: []Action{ActRetryCheckpoint, ActManualResume}, approval: true},
	RepositoryMissing:          {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	RepositoryIdentityMismatch: {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	RepositoryHeadChanged:      {class: ManualReviewRequired, safe: []Action{ActAcceptRepositoryHead}, unsafe: []Action{ActResend}, approval: true},
	RepositoryPermission:       {class: ManualReviewRequired, safe: []Action{ActManualResume}, approval: true},
	DiskFull:                   {class: ManualReviewRequired, safe: []Action{ActManualResume}, approval: true},
	ProtocolViolation:          {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	ManualCriteria:             {class: ManualReviewRequired, safe: []Action{ActApproveCriteria}, approval: true},
	InvariantViolation:         {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend, ActResume}, approval: true},
	ProviderUnavailable:        {class: SafeToResume, safe: []Action{ActWait}, auto: ActWait},
	PromptTooLarge:             {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	ArtifactCorrupt:            {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	ConfigDrift:                {class: SafeToResume, safe: []Action{ActResume}, auto: ActResume},
	Panic:                      {class: ReconciliationNeeded, safe: []Action{ActReconcileDispatch, ActManualResume}, unsafe: []Action{ActResend}, approval: true},
	OrphanPrecompute:           {class: SafeToResume, safe: []Action{ActDiscardSpeculative}, auto: ActDiscardSpeculative},
	ManualReview:               {class: ManualReviewRequired, safe: []Action{ActManualResume}, unsafe: []Action{ActResend}, approval: true},
}

// Conditions lists every condition (sorted).
func Conditions() []Condition {
	out := make([]Condition, 0, len(rules))
	for c := range rules {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ClassOf returns a condition's classification ("" when unknown).
func ClassOf(c Condition) Classification { return rules[c].class }

// Facts is the durable evidence for one condition of one execution.
type Facts struct {
	ExecutionID string         `json:"execution_id"`
	Condition   Condition      `json:"condition"`
	Evidence    map[string]any `json:"evidence,omitempty"`
}

// Plan is the deterministic recovery plan of a condition (WBS 18.9).
type Plan struct {
	ExecutionID          string         `json:"execution_id"`
	Condition            Condition      `json:"condition"`
	Evidence             map[string]any `json:"evidence"`
	Classification       Classification `json:"classification"`
	SafeActions          []Action       `json:"safe_actions"`
	UnsafeActions        []Action       `json:"unsafe_actions"`
	RecommendedAutomatic Action         `json:"recommended_automatic,omitempty"`
	RequiresApproval     bool           `json:"requires_human_approval"`
}

// PlanFor builds the plan for facts (WBS 18.8: same evidence → same plan).
// Unknown conditions are treated as MANUAL_REVIEW_REQUIRED with no
// automatic action (conservative).
func PlanFor(f Facts) Plan {
	r, ok := rules[f.Condition]
	if !ok {
		r = rule{class: ManualReviewRequired, safe: []Action{ActManualResume}, approval: true}
	}
	ev := f.Evidence
	if ev == nil {
		ev = map[string]any{}
	}
	return Plan{ExecutionID: f.ExecutionID, Condition: f.Condition, Evidence: ev, Classification: r.class,
		SafeActions: sorted(r.safe), UnsafeActions: sorted(r.unsafe), RecommendedAutomatic: r.auto, RequiresApproval: r.approval}
}

func sorted(a []Action) []Action {
	out := append([]Action{}, a...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Hash is the stable identity of a plan (sha256 of its canonical JSON;
// encoding/json orders map keys).
func (p Plan) Hash() string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("recovery: plan not serializable: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Safe reports whether a is one of the plan's safe actions.
func (p Plan) Safe(a Action) bool {
	for _, s := range p.SafeActions {
		if s == a {
			return true
		}
	}
	return false
}

// DispatchPhase is the WBS 18.16 view of a Phase 12 dispatch.
type DispatchPhase string

const (
	PhasePrepared         DispatchPhase = "PREPARED"
	PhaseSendStarted      DispatchPhase = "SEND_STARTED"
	PhaseDelivered        DispatchPhase = "DELIVERED"
	PhaseDeliveryUnknown  DispatchPhase = "DELIVERY_UNKNOWN"
	PhaseResponseReceived DispatchPhase = "RESPONSE_RECEIVED"
)

// DispatchPhaseOf maps a Phase 12 dispatch (status, delivery) onto the
// WBS 18.16 states. An unknown delivery is never mapped to a sendable
// state (DELIVERY_UNKNOWN ≠ FAILED, WBS 18.19).
func DispatchPhaseOf(status, delivery string) DispatchPhase {
	switch {
	case delivery == "DELIVERY_UNKNOWN" || status == "UNKNOWN":
		return PhaseDeliveryUnknown
	case delivery == "SENT_RESPONSE_RECEIVED" || status == "COMPLETED":
		return PhaseResponseReceived
	case delivery == "DEFINITELY_NOT_SENT" || status == "PENDING":
		return PhasePrepared
	case status == "SENDING":
		return PhaseSendStarted
	case status == "WAITING":
		return PhaseDelivered
	}
	// FAILED / CANCELLED without a recorded delivery: be conservative.
	return PhaseDeliveryUnknown
}

// ConditionForDispatch maps a dispatch phase to the recovery condition the
// planner uses.
func ConditionForDispatch(p DispatchPhase) Condition {
	switch p {
	case PhasePrepared:
		return DispatchNotSent
	case PhaseSendStarted, PhaseDelivered:
		return InflightDispatch
	case PhaseResponseReceived:
		return Resumable
	}
	return DeliveryUnknown
}

// ConditionForReason maps a recorded recovery reason (and its evidence)
// to a condition. Evidence written by Phase 18 code carries the condition
// explicitly ("condition"); older reasons are classified by their text.
func ConditionForReason(reason string, evidence map[string]any) Condition {
	if c, ok := evidence["condition"].(string); ok && c != "" {
		if _, known := rules[Condition(c)]; known {
			return Condition(c)
		}
	}
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "delivery unknown"):
		return DeliveryUnknown
	case strings.Contains(r, "manual review"):
		return ManualCriteria
	case strings.Contains(r, "protocol"):
		return ProtocolViolation
	case strings.Contains(r, "validation could not run"), strings.Contains(r, "validation infrastructure"):
		return ValidationInfra
	case strings.Contains(r, "checkpoint") && strings.Contains(r, "ambig"):
		return CheckpointAmbiguous
	case strings.Contains(r, "checkpoint"):
		return CheckpointRejected
	case strings.Contains(r, "invariant"):
		return InvariantViolation
	case strings.Contains(r, "repository"):
		return RepositoryHeadChanged
	case strings.Contains(r, "prompt") && (strings.Contains(r, "integrity") || strings.Contains(r, "tamper") || strings.Contains(r, "corrupt")):
		return ArtifactCorrupt
	}
	return ManualReview
}
