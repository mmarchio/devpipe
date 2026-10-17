package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/recovery"
	"overseer/internal/worker"
)

// ErrActionNotAvailable is a 409: the requested recovery action is not in
// the backend-approved set for the execution (WBS 17.98).
var ErrActionNotAvailable = fmt.Errorf("%w: recovery action not available", ErrConflict)

// recoveryResolver is the engine's recovery resolution (P16-116).
type recoveryResolver interface {
	ResolveRecovery(ctx context.Context, projectID, executionID uuid.UUID, r RecoveryResolution) (*persistence.ExecutionRecord, error)
}

// sessionReconciler is the Phase 12 session reconciliation; it never
// sends a worker request.
type sessionReconciler interface {
	Reconnect(ctx context.Context, projectID uuid.UUID) (worker.Reconciliation, error)
}

// executionGetter reads one execution.
type executionGetter interface {
	Get(ctx context.Context, ex persistence.Executor, projectID, id uuid.UUID) (*persistence.ExecutionRecord, error)
}

// RecoveryActionService offers and performs typed recovery operations
// (WBS 17.98–17.102, P17-43; Phase 18: 18.11 idempotency, 18.182 audit,
// 18.183 approval, 18.184 dry run). Only actions returned by Available can
// be performed; each maps onto an existing engine / Phase 12 operation.
// Only RESOLVE_DELIVERY with RESEND_AFTER_REVIEW can lead to a new worker
// dispatch, and only through an explicit, confirmed, audited operator
// decision recorded once per dispatch (PLAN P18 Decision 8).
type RecoveryActionService struct {
	execs      executionGetter
	resolver   recoveryResolver
	reconciler sessionReconciler
	store      persistence.RecoveryStore
	ops        recoveryOps
	now        func() time.Time
}

// recoveryOps are the Phase 18 engine recovery operations.
type recoveryOps interface {
	// ValidateRepositoryState runs the current unit's deterministic
	// validation against the repository as evidence only.
	ValidateRepositoryState(ctx context.Context, projectID, executionID uuid.UUID) (map[string]any, error)
	// AcceptRepositoryHead records the current HEAD as expected.
	AcceptRepositoryHead(ctx context.Context, projectID, executionID uuid.UUID, by, note string) (map[string]any, error)
}

// NewRecoveryActionService builds the service; reconciler may be nil (no
// worker configured: RECONCILE_DISPATCH then fails with a conflict).
func NewRecoveryActionService(execs executionGetter, resolver recoveryResolver, reconciler sessionReconciler) *RecoveryActionService {
	return &RecoveryActionService{execs: execs, resolver: resolver, reconciler: reconciler, now: time.Now}
}

// WithStore enables idempotent, persisted recovery actions and plans.
func (s *RecoveryActionService) WithStore(st persistence.RecoveryStore) *RecoveryActionService {
	s.store = st
	return s
}

// WithOps installs the Phase 18 engine recovery operations.
func (s *RecoveryActionService) WithOps(o recoveryOps) *RecoveryActionService {
	s.ops = o
	return s
}

// RecoveryRequest is an operator's typed recovery request.
type RecoveryRequest struct {
	Action execution.RecoveryActionKind `json:"action"`
	Reason string                       `json:"reason"`
	// Confirmed is the explicit confirmation of a high-impact action.
	Confirmed bool `json:"confirm"`
	// CheckIndexes are the acceptance criteria approved (APPROVE_CRITERIA;
	// default: every unverified criterion in the recovery evidence).
	CheckIndexes []int `json:"check_indexes,omitempty"`
	// IdempotencyKey identifies the action: a repeat with the same key
	// returns the recorded result without a second effect (WBS 18.11).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// EvidenceHash is the plan hash of the evidence the operator viewed
	// (WBS 18.182).
	EvidenceHash string `json:"evidence_hash,omitempty"`
	// DryRun describes the action without performing it (WBS 18.184).
	DryRun bool `json:"dry_run,omitempty"`
	// Resolution is required for RESOLVE_DELIVERY.
	Resolution    string `json:"resolution,omitempty"`
	By            string `json:"-"`
	ByName        string `json:"-"`
	CorrelationID string `json:"-"`
}

// DryRunPreview is what an action would change (WBS 18.184).
type DryRunPreview struct {
	Records            []string `json:"records"`
	ExternalOperations []string `json:"external_operations"`
}

// RecoveryOutcome reports what was performed.
type RecoveryOutcome struct {
	Action          execution.RecoveryActionKind `json:"action"`
	ExecutionStatus string                       `json:"execution_status"`
	Reconciliation  string                       `json:"reconciliation,omitempty"`
	ActionID        string                       `json:"action_id,omitempty"`
	PlanHash        string                       `json:"plan_hash,omitempty"`
	Replayed        bool                         `json:"replayed,omitempty"`
	DryRun          *DryRunPreview               `json:"dry_run,omitempty"`
	Detail          map[string]any               `json:"detail,omitempty"`
}

// Available returns the backend-approved actions (empty unless the
// execution requires recovery).
func (s *RecoveryActionService) Available(ctx context.Context, projectID, executionID uuid.UUID) ([]execution.RecoveryAction, *persistence.ExecutionRecord, error) {
	rec, err := s.execs.Get(ctx, nil, projectID, executionID)
	if err != nil {
		return nil, nil, translate(err)
	}
	if rec.Status != execution.ExecutionRecovery {
		return []execution.RecoveryAction{}, rec, nil
	}
	return planFiltered(execution.RecoveryActions(execution.RecoveryFactsFrom(rec.RecoveryReason, rec.RecoveryEvidence)), CurrentPlan(rec)), rec, nil
}

// planFiltered makes the offered actions agree with the deterministic
// recovery plan (WBS 18.229): an action the plan marks unsafe is never
// offered, and a plan-safe operator action the reason-based catalogue
// missed is added.
func planFiltered(acts []execution.RecoveryAction, plan recovery.Plan) []execution.RecoveryAction {
	unsafe := map[string]bool{}
	for _, a := range plan.UnsafeActions {
		unsafe[string(a)] = true
	}
	out := make([]execution.RecoveryAction, 0, len(acts))
	have := map[execution.RecoveryActionKind]bool{}
	for _, a := range acts {
		if unsafe[string(a.Kind)] {
			continue
		}
		have[a.Kind] = true
		out = append(out, a)
	}
	for _, a := range plan.SafeActions {
		k := execution.RecoveryActionKind(a)
		if !have[k] && execution.KnownRecoveryAction(k) {
			have[k] = true
			out = append(out, execution.RecoveryActionOf(k))
		}
	}
	return out
}

// CurrentPlan is the deterministic recovery plan of a RECOVERY_REQUIRED
// execution (WBS 18.8): same durable evidence → same plan hash.
func CurrentPlan(rec *persistence.ExecutionRecord) recovery.Plan {
	ev := map[string]any{"reason": rec.RecoveryReason}
	for k, v := range rec.RecoveryEvidence {
		ev[k] = v
	}
	return recovery.PlanFor(recovery.Facts{ExecutionID: rec.ID.String(), Condition: recovery.ConditionForReason(rec.RecoveryReason, rec.RecoveryEvidence), Evidence: ev})
}

// previewOf describes an action's effects (dry run).
func previewOf(kind execution.RecoveryActionKind, resolution string) *DryRunPreview {
	resume := []string{"executions: RECOVERY_REQUIRED → RUNNING", "execution_events: EXECUTION_RECOVERY_RESOLVED", "recovery_actions: one row", "audit_events: one row"}
	switch kind {
	case execution.RecoveryReconcileDispatch:
		return &DryRunPreview{Records: []string{"worker_sessions: last seen / LOST status", "recovery_actions: one row", "audit_events: one row"},
			ExternalOperations: []string{"Phase 12 session reconciliation query (no worker request is sent)"}}
	case execution.RecoveryRunValidation:
		return &DryRunPreview{Records: resume, ExternalOperations: []string{"the next engine step re-runs the deterministic validation commands"}}
	case execution.RecoveryRetryCheckpoint:
		return &DryRunPreview{Records: resume, ExternalOperations: []string{"the next engine step reconciles the checkpoint by its trailer and commits at most once"}}
	case execution.RecoveryApproveCriteria:
		return &DryRunPreview{Records: append([]string{"acceptance_approvals: one row per approved criterion"}, resume...),
			ExternalOperations: []string{"none until the next engine step evaluates the unit gates"}}
	case execution.RecoveryManualResume:
		return &DryRunPreview{Records: resume, ExternalOperations: []string{"none; the next engine step re-evaluates persisted state (an unknown delivery is never resent)"}}
	case execution.RecoveryValidateRepositoryState:
		return &DryRunPreview{Records: []string{"validation_runs: one evidence run (execution stays RECOVERY_REQUIRED)", "recovery_actions: one row", "audit_events: one row"},
			ExternalOperations: []string{"the unit's validation commands run against the current repository"}}
	case execution.RecoveryResolveDelivery:
		ext := "the next engine step routes the unit to deterministic validation (worker COMPLETE is not assumed)"
		if resolution == execution.ResolutionResendAfterReview {
			ext = "the next engine step sends ONE new worker request (attempt + 1); the unknown dispatch keeps DELIVERY_UNKNOWN"
		}
		return &DryRunPreview{Records: append([]string{"dispatch_resolutions: one row (the dispatch row is unchanged)"}, resume...), ExternalOperations: []string{ext}}
	case execution.RecoveryAcceptRepositoryHead:
		return &DryRunPreview{Records: append([]string{"execution_expected_heads: one OPERATOR_ACCEPT row appended (current HEAD / branch, actor, reason)"}, resume...), ExternalOperations: []string{"none"}}
	}
	return &DryRunPreview{Records: []string{}, ExternalOperations: []string{}}
}

// Perform validates and performs one action.
func (s *RecoveryActionService) Perform(ctx context.Context, projectID, executionID uuid.UUID, req RecoveryRequest) (*RecoveryOutcome, error) {
	avail, rec, err := s.Available(ctx, projectID, executionID)
	if err != nil {
		return nil, err
	}
	var act *execution.RecoveryAction
	for i := range avail {
		if avail[i].Kind == req.Action {
			act = &avail[i]
		}
	}
	if act == nil {
		// An idempotent repeat of an action that already succeeded (the
		// execution left recovery) returns the recorded result.
		if prev := s.replay(ctx, executionID, req); prev != nil {
			return prev, nil
		}
		return nil, ErrActionNotAvailable
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if act.HighImpact && (!req.Confirmed || req.Reason == "") {
		return nil, fmt.Errorf("%w: %s requires explicit confirmation and a reason", ErrInvalidInput, act.Kind)
	}
	if act.Kind == execution.RecoveryResolveDelivery && req.Resolution != execution.ResolutionResendAfterReview &&
		req.Resolution != execution.ResolutionAcceptRepositoryState {
		return nil, fmt.Errorf("%w: resolution must be %s or %s", ErrInvalidInput, execution.ResolutionResendAfterReview, execution.ResolutionAcceptRepositoryState)
	}
	plan := CurrentPlan(rec)
	out := &RecoveryOutcome{Action: act.Kind, PlanHash: plan.Hash(), ExecutionStatus: string(rec.Status)}
	if req.DryRun {
		out.DryRun = previewOf(act.Kind, req.Resolution)
		return out, nil
	}
	var actionID uuid.UUID
	if s.store != nil {
		if len(req.IdempotencyKey) < 8 {
			return nil, fmt.Errorf("%w: idempotency_key (8–200 characters) is required", ErrInvalidInput)
		}
		raw, _ := json.Marshal(plan)
		if err := s.store.RecordPlan(ctx, &persistence.RecoveryPlanRecord{ID: uuid.New(), ExecutionID: rec.ID, ProjectID: projectID, PlanHash: out.PlanHash,
			Condition: string(plan.Condition), Classification: string(plan.Classification), Plan: raw, Source: "ACTION", CreatedAt: s.now()}); err != nil {
			return nil, translate(err)
		}
		a := &persistence.RecoveryActionRecord{ID: uuid.New(), ExecutionID: rec.ID, ProjectID: projectID, IdempotencyKey: req.IdempotencyKey,
			Action: string(act.Kind), PlanHash: out.PlanHash, EvidenceHash: req.EvidenceHash, ActorID: nonEmpty(req.By, "operator"),
			ActorName: req.ByName, Reason: req.Reason, Confirmed: req.Confirmed, CorrelationID: req.CorrelationID, CreatedAt: s.now()}
		prev, err := s.store.BeginAction(ctx, a)
		if errors.Is(err, persistence.ErrConflict) {
			return s.replayed(prev, executionID, act.Kind)
		} else if err != nil {
			return nil, translate(err)
		}
		actionID = a.ID
		out.ActionID = a.ID.String()
	}
	err = s.perform(ctx, projectID, rec, act.Kind, req, out, actionID)
	if s.store != nil {
		status, errText := "SUCCEEDED", ""
		if err != nil {
			status, errText = "FAILED", err.Error()
		}
		res := map[string]any{"execution_status": out.ExecutionStatus}
		if out.Reconciliation != "" {
			res["reconciliation"] = out.Reconciliation
		}
		for k, v := range out.Detail {
			res[k] = v
		}
		_ = s.store.FinishAction(ctx, actionID, status, res, errText)
	}
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, translate(err)
	}
	return out, nil
}

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// replay returns the recorded outcome of an earlier action with the same
// idempotency key, or nil.
func (s *RecoveryActionService) replay(ctx context.Context, executionID uuid.UUID, req RecoveryRequest) *RecoveryOutcome {
	if s.store == nil || req.IdempotencyKey == "" || req.DryRun {
		return nil
	}
	prev, err := s.store.GetActionByKey(ctx, req.IdempotencyKey)
	if err != nil || prev.ExecutionID != executionID || prev.Action != string(req.Action) || prev.Status != "SUCCEEDED" {
		return nil
	}
	out, _ := s.replayed(prev, executionID, req.Action)
	return out
}

func (s *RecoveryActionService) replayed(prev *persistence.RecoveryActionRecord, executionID uuid.UUID, kind execution.RecoveryActionKind) (*RecoveryOutcome, error) {
	switch {
	case prev == nil:
		return nil, ErrConflict
	case prev.ExecutionID != executionID || prev.Action != string(kind):
		return nil, fmt.Errorf("%w: idempotency key already used for another action", ErrConflict)
	case prev.Status == "RUNNING":
		return nil, fmt.Errorf("%w: the same recovery action is already in progress", ErrConflict)
	case prev.Status == "FAILED":
		return nil, fmt.Errorf("%w: this action already failed: %s (use a new request to retry)", ErrConflict, prev.Error)
	}
	st, _ := prev.Result["execution_status"].(string)
	rc, _ := prev.Result["reconciliation"].(string)
	return &RecoveryOutcome{Action: kind, ExecutionStatus: st, Reconciliation: rc, ActionID: prev.ID.String(), PlanHash: prev.PlanHash,
		Replayed: true, Detail: prev.Result}, nil
}

// perform executes the action.
func (s *RecoveryActionService) perform(ctx context.Context, projectID uuid.UUID, rec *persistence.ExecutionRecord, kind execution.RecoveryActionKind,
	req RecoveryRequest, out *RecoveryOutcome, actionID uuid.UUID) error {
	executionID := rec.ID
	resolve := func(r RecoveryResolution) error {
		r.By, r.Note = req.By, req.Reason
		updated, err := s.resolver.ResolveRecovery(ctx, projectID, executionID, r)
		if err != nil {
			return err
		}
		out.ExecutionStatus = string(updated.Status)
		return nil
	}
	switch kind {
	case execution.RecoveryReconcileDispatch:
		if s.reconciler == nil {
			return fmt.Errorf("%w: no worker configured", ErrConflict)
		}
		res, err := s.reconciler.Reconnect(ctx, projectID)
		if err != nil {
			return fmt.Errorf("%w: reconciliation failed: %v", ErrConflict, err)
		}
		out.Reconciliation, out.ExecutionStatus = string(res), string(rec.Status)
		return nil
	case execution.RecoveryRunValidation, execution.RecoveryRetryCheckpoint:
		return resolve(RecoveryResolution{Action: RecoveryRetryStep})
	case execution.RecoveryApproveCriteria:
		if rec.CurrentWorkUnitID == nil {
			return fmt.Errorf("%w: no work unit awaiting review", ErrConflict)
		}
		idx := req.CheckIndexes
		if len(idx) == 0 {
			idx = unverifiedIndexes(rec.RecoveryEvidence)
		}
		if len(idx) == 0 {
			return fmt.Errorf("%w: no criteria to approve", ErrInvalidInput)
		}
		return resolve(RecoveryResolution{Action: RecoveryApproveCriteria, WorkUnitID: rec.CurrentWorkUnitID.String(), Indexes: idx})
	case execution.RecoveryManualResume:
		return resolve(RecoveryResolution{Action: RecoveryResume})
	case execution.RecoveryValidateRepositoryState:
		if s.ops == nil {
			return fmt.Errorf("%w: repository validation is not available", ErrConflict)
		}
		d, err := s.ops.ValidateRepositoryState(ctx, projectID, executionID)
		if err != nil {
			return err
		}
		out.Detail, out.ExecutionStatus = d, string(rec.Status)
		return nil
	case execution.RecoveryResolveDelivery:
		if s.store == nil {
			return fmt.Errorf("%w: delivery resolution requires the recovery store", ErrConflict)
		}
		ds, _ := rec.RecoveryEvidence["dispatch_id"].(string)
		did, err := uuid.Parse(ds)
		if err != nil {
			return fmt.Errorf("%w: the recovery evidence names no dispatch", ErrConflict)
		}
		if err := s.store.InsertResolution(ctx, &persistence.DispatchResolutionRecord{ID: uuid.New(), DispatchID: did, ExecutionID: executionID,
			ProjectID: projectID, Resolution: req.Resolution, ActionID: actionID, ActorID: nonEmpty(req.By, "operator"), Reason: req.Reason,
			Evidence: map[string]any{"plan_hash": out.PlanHash, "evidence_hash": req.EvidenceHash}, CreatedAt: s.now()}); err != nil {
			if errors.Is(err, persistence.ErrConflict) {
				return fmt.Errorf("%w: this dispatch was already resolved", ErrConflict)
			}
			return err
		}
		out.Detail = map[string]any{"dispatch_id": did.String(), "resolution": req.Resolution}
		return resolve(RecoveryResolution{Action: RecoveryResume})
	case execution.RecoveryAcceptRepositoryHead:
		if s.ops == nil {
			return fmt.Errorf("%w: repository acceptance is not available", ErrConflict)
		}
		d, err := s.ops.AcceptRepositoryHead(ctx, projectID, executionID, req.By, req.Reason)
		if err != nil {
			return err
		}
		out.Detail = d
		return resolve(RecoveryResolution{Action: RecoveryResume})
	}
	return ErrActionNotAvailable
}

// unverifiedIndexes reads the unverified acceptance criteria from recovery
// evidence ("acceptance": [{index, verdict}...]).
func unverifiedIndexes(ev map[string]any) []int {
	list, _ := ev["acceptance"].([]any)
	var out []int
	for _, v := range list {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		verdict, _ := m["verdict"].(string)
		idx, ok := m["index"].(float64)
		if ok && verdict != string(execution.VerdictPass) && verdict != string(execution.VerdictFail) {
			out = append(out, int(idx))
		}
	}
	return out
}

// RecoveryPlans is the plan view (WBS 18.8–18.10).
type RecoveryPlans struct {
	ExecutionID     string                             `json:"execution_id"`
	ExecutionStatus string                             `json:"execution_status"`
	Current         *recovery.Plan                     `json:"current,omitempty"`
	CurrentHash     string                             `json:"current_hash,omitempty"`
	Recorded        []persistence.RecoveryPlanRecord   `json:"-"`
	RecordedPlans   []map[string]any                   `json:"recorded_plans"`
	Actions         []persistence.RecoveryActionRecord `json:"-"`
	ActionRows      []map[string]any                   `json:"actions"`
}

// Plans returns the current plan (when recovery is required) and the
// recorded plans / actions. It never writes.
func (s *RecoveryActionService) Plans(ctx context.Context, projectID, executionID uuid.UUID) (*RecoveryPlans, error) {
	rec, err := s.execs.Get(ctx, nil, projectID, executionID)
	if err != nil {
		return nil, translate(err)
	}
	out := &RecoveryPlans{ExecutionID: rec.ID.String(), ExecutionStatus: string(rec.Status), RecordedPlans: []map[string]any{}, ActionRows: []map[string]any{}}
	if rec.Status == execution.ExecutionRecovery {
		p := CurrentPlan(rec)
		out.Current, out.CurrentHash = &p, p.Hash()
	}
	if s.store == nil {
		return out, nil
	}
	plans, err := s.store.ListPlans(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	for _, p := range plans {
		var body any
		_ = json.Unmarshal(p.Plan, &body)
		out.RecordedPlans = append(out.RecordedPlans, map[string]any{"plan_hash": p.PlanHash, "condition": p.Condition,
			"classification": p.Classification, "source": p.Source, "created_at": p.CreatedAt.UTC(), "plan": body})
	}
	acts, err := s.store.ListActions(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	for _, a := range acts {
		out.ActionRows = append(out.ActionRows, map[string]any{"id": a.ID.String(), "action": a.Action, "status": a.Status, "actor": a.ActorID,
			"reason": a.Reason, "evidence_hash": a.EvidenceHash, "plan_hash": a.PlanHash, "correlation_id": a.CorrelationID,
			"result": a.Result, "error": a.Error, "created_at": a.CreatedAt.UTC(), "finished_at": a.FinishedAt})
	}
	return out, nil
}
