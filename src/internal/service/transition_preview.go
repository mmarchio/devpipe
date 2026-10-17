// P6-31 (WBS 6.41): transition preview — evaluate a transition WITHOUT
// applying it. The op mirrors the P6-21 apply path's READ half only:
// it reads the project's runtime state, loads the machine version's
// persisted transitions (conditions included via the repository's
// LEFT JOIN), runs the P6-14 priority selection over the condition-
// gated eligibility set (P6-10 condition evaluation over the
// caller-supplied context), and returns the selected transition (or an
// explicit nil selection) — no writes, no version bump, no history
// append (T6.41.2–T6.41.4). Read-only; the nil-pool / txPool parity
// follows the established read-model pattern (one transaction when
// pooled, sequential fallback otherwise).
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// PreviewTransitionInput is one preview evaluation (WBS 6.41): the
// outcome token the caller wants to evaluate, and the caller-supplied
// context values (context usage + threshold — the WBS 6.41 example
// shape) used for the condition evaluation. No version guard: a
// preview does not mutate, so there is no optimistic-concurrency
// stake (T6.41.3 is that the stored version is UNCHANGED, not
// verified).
type PreviewTransitionInput struct {
	Outcome string

	ContextUsage     float64
	ContextThreshold float64
}

// PreviewTransitionSelection is the selected transition (T6.41.1):
// the destination state + outcome the evaluation picked.
type PreviewTransitionSelection struct {
	ToStateID string
	Outcome   string
}

// PreviewTransitionResult is the assembled preview (WBS 6.41): the
// runtime's current state (the evaluation's source), the caller's
// outcome token, and the selected transition (nil when the selection
// is empty — no match, or condition-gated out: a defined, non-error
// outcome of the P6-14 selection).
type PreviewTransitionResult struct {
	CurrentState string
	Outcome      string
	Selected     *PreviewTransitionSelection
}

// TransitionPreviewService evaluates transitions without applying them
// (WBS 6.41). It composes the P6-20 runtime-state read and the
// P6-05 transition repository + the P6-14 pure selection — read-only,
// no new state.
type TransitionPreviewService struct {
	runtime persistence.RuntimeStateRepository
	trans   persistence.StateTransitionRepository
	txPool  *pgxpool.Pool
}

// NewTransitionPreviewService wires the preview service to the
// runtime-state and transition repositories.
func NewTransitionPreviewService(
	runtimeRepo persistence.RuntimeStateRepository,
	transRepo persistence.StateTransitionRepository,
) *TransitionPreviewService {
	return &TransitionPreviewService{runtime: runtimeRepo, trans: transRepo}
}

// WithTxPool wires the transaction pool so the whole evaluation runs
// in ONE transaction (the read-model pattern); with a nil pool the
// reads run sequentially (the op is read-only — no atomicity stake).
func (s *TransitionPreviewService) WithTxPool(pool *pgxpool.Pool) *TransitionPreviewService {
	s.txPool = pool
	return s
}

// Preview evaluates the given outcome over the project's current
// runtime state (WBS 6.41, T6.41.1–T6.41.4):
//
//  1. read the project's runtime state (ErrNotFound when unprepared);
//  2. load the machine version's persisted transitions, version-scoped
//     to the runtime row's binding (the binding is authoritative);
//  3. filter to the ELIGIBLE transitions — enabled, from the
//     runtime's current state, outcome-matching under the documented
//     case policy, and (when a condition is attached) the condition
//     evaluating true over the caller-supplied context (P6-10);
//  4. run the P6-14 priority selection over the eligible set;
//  5. return the selected transition (or a nil selection when the
//     set is empty) — no write, no version bump, no history append.
//
// Ambiguity (an equal-priority tie at the minimum priority) is the
// P6-14 selection's defined rejection, surfaced as service.ErrConflict
// (the REST surface's established conflict sentinel).
func (s *TransitionPreviewService) Preview(ctx context.Context, projectID string, in *PreviewTransitionInput) (*PreviewTransitionResult, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if in == nil || in.Outcome == "" {
		return nil, ErrInvalidInput
	}
	do := func(
		runtimeRepo persistence.RuntimeStateRepository,
		transRepo persistence.StateTransitionRepository,
	) (*PreviewTransitionResult, error) {
		rec, err := runtimeRepo.Get(ctx, proj)
		if err != nil {
			return nil, translate(err)
		}
		recs, err := transRepo.ListByMachine(ctx, rec.StateMachineID, rec.StateMachineVersion)
		if err != nil {
			return nil, translate(err)
		}
		trs := make([]*state.StateTransition, 0, len(recs))
		for _, r := range recs {
			if !p631Eligible(r, rec.CurrentStateID, in) {
				continue
			}
			trs = append(trs, &state.StateTransition{
				ID: r.ID, StateMachineID: r.MachineID,
				FromStateID: r.FromStateID, ToStateID: r.ToStateID,
				Outcome: r.Outcome, Priority: r.Priority, Enabled: r.Enabled,
			})
		}
		sel, err := state.SelectTransition(trs, rec.CurrentStateID, in.Outcome)
		if err != nil {
			// The P6-14 selection returns a single error class:
			// equal-priority ambiguity (a nil, nil is the no-match
			// outcome, not an error). Map it to the conflict sentinel.
			return nil, ErrConflict
		}
		out := &PreviewTransitionResult{
			CurrentState: rec.CurrentStateID.String(),
			Outcome:      in.Outcome,
		}
		if sel != nil {
			out.Selected = &PreviewTransitionSelection{
				ToStateID: sel.ToStateID.String(),
				Outcome:   sel.Outcome,
			}
		}
		return out, nil
	}
	if s.txPool == nil {
		return do(s.runtime, s.trans)
	}
	var out *PreviewTransitionResult
	e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = do(s.runtime.WithTx(tx), s.trans.WithTx(tx))
		return ee
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// p631Eligible reports whether a persisted transition participates in
// the selection for this preview: enabled, from the runtime's current
// state, outcome-matching under the documented case policy, and — when
// the transition carries a condition — the condition evaluating true
// over the caller-supplied context (the P6-10 condition evaluation).
// A condition that fails to re-validate (a persisted row that predates
// the validation surface) makes the transition ineligible — the
// conservative choice: a preview must not select a transition it
// cannot verify.
func p631Eligible(r *persistence.StateTransitionRecord, currentState uuid.UUID, in *PreviewTransitionInput) bool {
	if !r.Enabled || r.FromStateID != currentState {
		return false
	}
	if !state.MatchOutcome(r.Outcome, in.Outcome) {
		return false
	}
	if r.Condition == nil {
		return true
	}
	c, err := state.NewCondition(state.ConditionInput{
		Field:    r.Condition.Field,
		Operator: state.ConditionOperator(r.Condition.Operator),
		Value: state.ConditionValue{
			Literal: r.Condition.Literal, ContextField: r.Condition.ContextField,
		},
	})
	if err != nil {
		return false
	}
	return c.Evaluate(map[string]any{
		"context_usage":             in.ContextUsage,
		"project.context_threshold": in.ContextThreshold,
	})
}
