// P6-27 (WBS 6.37): the transition-configuration service — the service half
// of the Transition Configuration API. It exposes transition management
// within an editable state-machine definition: create transition, update
// transition (incl. disable), configure condition, configure priority.
//
// It composes the prior-task repositories (P6-05 machines/states,
// P6-06/P6-27 transitions + conditions) — no parallel path. Every op
// resolves the project's machine family the same way as P6-25/P6-26 (the
// family whose MAX-version row is highest), operates on that family's
// CURRENT version rows, and runs in ONE transaction when a pool is wired
// (WBS 2.21; nil-pool sequential fallback for unit scope). The ambiguity
// rejection reuses the P6-14 detector (same from + normalized outcome +
// equal priority → conflict) and the condition is the P6-04 domain shape.
package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// TransitionConfigService implements the WBS 6.37 transition-configuration
// surface.
type TransitionConfigService struct {
	machines   persistence.StateMachineRepository
	states     persistence.WorkflowStateRepository
	trans      persistence.StateTransitionRepository
	conditions persistence.TransitionConditionRepository
	txPool     *pgxpool.Pool
}

// NewTransitionConfigService wires the service to its repositories.
func NewTransitionConfigService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	conditions persistence.TransitionConditionRepository,
) *TransitionConfigService {
	return &TransitionConfigService{
		machines: machines, states: states, trans: transitions,
		conditions: conditions,
	}
}

// WithTxPool wires the transaction pool for the atomic path (WBS 2.21);
// with a nil pool the ops run sequentially (single-goroutine unit scope).
func (s *TransitionConfigService) WithTxPool(pool *pgxpool.Pool) *TransitionConfigService {
	s.txPool = pool
	return s
}

// TransitionConditionInput is the caller-supplied structured condition for
// the condition ops (WBS 6.10/P6-04 shape): the value is exactly one of a
// literal or a context-field reference.
type TransitionConditionInput struct {
	Field        string
	Operator     state.ConditionOperator
	Literal      any
	ContextField string
}

// CreateTransitionInput is the payload for the create-transition op
// (T6.37.1). Enabled defaults to true when nil (the P6-02 domain
// convention); Condition is the structured condition to persist
// (T6.37.2) and may be nil.
type CreateTransitionInput struct {
	FromKey   string
	ToKey     string
	Outcome   string
	Priority  int
	Enabled   *bool
	Condition *TransitionConditionInput
}

// UpdateTransitionInput carries the editable configuration of an update:
// pointer fields distinguish "not supplied" (keep stored value) from an
// explicit value.
type UpdateTransitionInput struct {
	Outcome  *string
	Priority *int
	Enabled  *bool
}

// TransitionConfigView is the service-level view of a configured
// transition: the persisted definition (addressed by state keys, never row
// ids) plus its priority, liveness, and condition.
type TransitionConfigView struct {
	ID        string
	From      string
	To        string
	Outcome   string
	Priority  int
	Enabled   bool
	Condition *TransitionConditionInput
}

type p627Repos struct {
	machines   persistence.StateMachineRepository
	states     persistence.WorkflowStateRepository
	trans      persistence.StateTransitionRepository
	conditions persistence.TransitionConditionRepository
}

func (s *TransitionConfigService) bind(tx persistence.Transaction) p627Repos {
	if tx == nil {
		return p627Repos{
			machines: s.machines, states: s.states, trans: s.trans,
			conditions: s.conditions,
		}
	}
	return p627Repos{
		machines: s.machines.WithTx(tx), states: s.states.WithTx(tx),
		trans: s.trans.WithTx(tx), conditions: s.conditions.WithTx(tx),
	}
}

// runOnce executes fn inside one transaction when a pool is wired, else
// sequentially.
func (s *TransitionConfigService) runOnce(ctx context.Context, fn func(tx persistence.Transaction) error) error {
	if s.txPool == nil {
		return fn(nil)
	}
	return dbrepo.Run(ctx, s.txPool, fn)
}

// resolveCurrent returns the family's current machine record (the
// highest-version row of the resolved family) — the op target.
func (s *TransitionConfigService) resolveCurrent(ctx context.Context, r p627Repos, projectID string) (*persistence.MachineRecord, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	list, err := r.machines.ListByProject(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	var best *persistence.MachineRecord
	for _, m := range list {
		if best == nil ||
			m.Version > best.Version ||
			(m.Version == best.Version && m.FamilyID.String() < best.FamilyID.String()) {
			best = m
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// stateKeyMap maps the current machine's state ids to their keys.
func stateKeyMap(ctx context.Context, r p627Repos, machine *persistence.MachineRecord) (map[uuid.UUID]string, error) {
	states, err := r.states.ListByMachine(ctx, machine.ID, machine.Version)
	if err != nil {
		return nil, translate(err)
	}
	m := make(map[uuid.UUID]string, len(states))
	for _, st := range states {
		m[st.ID] = st.Key
	}
	return m, nil
}

// findTransition locates the transition within the current machine version;
// unknown ids report ErrNotFound.
func findTransition(ctx context.Context, r p627Repos, machine *persistence.MachineRecord, id string) (*persistence.StateTransitionRecord, error) {
	tid, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	rec, err := r.trans.Get(ctx, tid)
	if err != nil {
		return nil, translate(err)
	}
	if rec.MachineID != machine.ID || rec.Version != machine.Version {
		return nil, ErrNotFound
	}
	return rec, nil
}

// toDomain converts a persistence record to the P6-14 domain transition
// (the detector's input shape).
func toDomain(rec *persistence.StateTransitionRecord) *state.StateTransition {
	return &state.StateTransition{
		ID:             rec.ID,
		StateMachineID: rec.MachineID,
		FromStateID:    rec.FromStateID,
		ToStateID:      rec.ToStateID,
		Outcome:        rec.Outcome,
		Priority:       rec.Priority,
		HasCondition:   rec.Condition != nil,
		Enabled:        rec.Enabled,
		CreatedAt:      rec.CreatedAt,
		UpdatedAt:      rec.UpdatedAt,
	}
}

// checkAmbiguity runs the P6-14 detector (T6.37.5) over the current
// version's transitions with the candidate row substituted in: same from +
// normalized outcome + equal priority → conflict.
func checkAmbiguity(existing []*persistence.StateTransitionRecord, candidate *persistence.StateTransitionRecord) error {
	// A disabled candidate never participates in selection, so it can
	// never be ambiguous (the detector excludes disabled rows).
	if !candidate.Enabled {
		return nil
	}
	domain := make([]*state.StateTransition, 0, len(existing)+1)
	for _, rec := range existing {
		if rec.ID == candidate.ID {
			continue
		}
		domain = append(domain, toDomain(rec))
	}
	domain = append(domain, toDomain(candidate))
	report := state.DetectAmbiguity(domain)
	if !report.Valid {
		return &AmbiguousTransitionsErr{Groups: len(report.Groups)}
	}
	return nil
}

// AmbiguousTransitionsErr is the T6.37.5 rejection: a same-source,
// same-outcome, equal-priority configuration. It maps to 409 on the REST
// surface (its Is matches ErrConflict).
type AmbiguousTransitionsErr struct {
	Groups int
}

func (e *AmbiguousTransitionsErr) Error() string {
	return "ambiguous transition configuration: same source state, outcome and priority"
}
func (e *AmbiguousTransitionsErr) Is(target error) bool { return target == ErrConflict }

// validateCondition constructs the P6-04 domain condition (field declared,
// operator in the fixed set, value exactly one of literal / context-field);
// violations report ErrInvalidInput.
func validateCondition(in *TransitionConditionInput) (*state.Condition, error) {
	if in == nil {
		return nil, nil
	}
	return state.NewCondition(state.ConditionInput{
		Field: in.Field, Operator: in.Operator,
		Value: state.ConditionValue{Literal: in.Literal, ContextField: in.ContextField},
	})
}

// normalizeOutcome trims surrounding whitespace, preserving the original
// casing (the documented case policy, T6.9.4 — stored as given).
func normalizeOutcome(o string) string {
	return strings.TrimSpace(o)
}

// toConditionInput converts a persisted condition to the view shape.
func toConditionInput(c *persistence.TransitionCondition) *TransitionConditionInput {
	if c == nil {
		return nil
	}
	return &TransitionConditionInput{
		Field: c.Field, Operator: state.ConditionOperator(c.Operator),
		Literal: c.Literal, ContextField: c.ContextField,
	}
}

func transitionView(ctx context.Context, r p627Repos, machine *persistence.MachineRecord, rec *persistence.StateTransitionRecord) (*TransitionConfigView, error) {
	keys, err := stateKeyMap(ctx, r, machine)
	if err != nil {
		return nil, err
	}
	return &TransitionConfigView{
		ID:        rec.ID.String(),
		From:      keys[rec.FromStateID],
		To:        keys[rec.ToStateID],
		Outcome:   rec.Outcome,
		Priority:  rec.Priority,
		Enabled:   rec.Enabled,
		Condition: toConditionInput(rec.Condition),
	}, nil
}

// CreateTransition adds a new transition to the current machine definition
// (T6.37.1): the row joins the current version (a transition edit is not a
// full-definition version bump — WBS 6.37 operates WITHIN the editable
// definition). Endpoints are addressed by state key (unknown keys →
// ErrNotFound); the ambiguity detector fires before the write (T6.37.5);
// the condition is persisted when supplied (T6.37.2).
func (s *TransitionConfigService) CreateTransition(ctx context.Context, projectID string, in *CreateTransitionInput) (*TransitionConfigView, error) {
	if in == nil || in.Outcome == "" {
		return nil, ErrInvalidInput
	}
	cond, err := validateCondition(in.Condition)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var out *TransitionConfigView
	e := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		states, err := r.states.ListByMachine(ctx, machine.ID, machine.Version)
		if err != nil {
			return translate(err)
		}
		var from, to *persistence.WorkflowStateRecord
		for _, st := range states {
			if st.Key == state.NormalizeKey(in.FromKey) {
				from = st
			}
			if st.Key == state.NormalizeKey(in.ToKey) {
				to = st
			}
		}
		if from == nil || to == nil {
			return ErrNotFound
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		now := time.Now().UTC()
		rec := &persistence.StateTransitionRecord{
			ID: uuid.New(), MachineID: machine.ID, Version: machine.Version,
			FromStateID: from.ID, ToStateID: to.ID,
			Outcome: normalizeOutcome(in.Outcome), Priority: in.Priority,
			Enabled: enabled, CreatedAt: now, UpdatedAt: now,
		}
		existing, err := r.trans.ListByMachine(ctx, machine.ID, machine.Version)
		if err != nil {
			return translate(err)
		}
		if err := checkAmbiguity(existing, rec); err != nil {
			return err
		}
		if err := r.trans.Create(ctx, rec); err != nil {
			return translate(err)
		}
		if cond != nil {
			if err := r.conditions.Create(ctx, rec.ID, &persistence.TransitionCondition{
				Field: cond.Field, Operator: string(cond.Operator),
				Literal: cond.Value.Literal, ContextField: cond.Value.ContextField,
			}); err != nil {
				return translate(err)
			}
			rec.Condition = &persistence.TransitionCondition{
				Field: cond.Field, Operator: string(cond.Operator),
				Literal: cond.Value.Literal, ContextField: cond.Value.ContextField,
			}
		}
		view, err := transitionView(ctx, r, machine, rec)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// UpdateTransition mutates the transition's editable configuration
// (outcome, priority, enabled) in place within the current machine version.
// The ambiguity detector fires on the updated row (T6.37.5); unknown ids
// report ErrNotFound.
func (s *TransitionConfigService) UpdateTransition(ctx context.Context, projectID, transitionID string, in *UpdateTransitionInput) (*TransitionConfigView, error) {
	if in == nil {
		return nil, ErrInvalidInput
	}
	var out *TransitionConfigView
	e := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		rec, err := findTransition(ctx, r, machine, transitionID)
		if err != nil {
			return err
		}
		if in.Outcome != nil {
			if *in.Outcome == "" {
				return ErrInvalidInput
			}
			rec.Outcome = normalizeOutcome(*in.Outcome)
		}
		if in.Priority != nil {
			rec.Priority = *in.Priority
		}
		if in.Enabled != nil {
			rec.Enabled = *in.Enabled
		}
		existing, err := r.trans.ListByMachine(ctx, machine.ID, machine.Version)
		if err != nil {
			return translate(err)
		}
		if err := checkAmbiguity(existing, rec); err != nil {
			return err
		}
		upd, err := r.trans.Update(ctx, rec)
		if err != nil {
			return translate(err)
		}
		view, err := transitionView(ctx, r, machine, upd)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// DisableTransition flips the transition's liveness off (T6.37.1 disable op).
func (s *TransitionConfigService) DisableTransition(ctx context.Context, projectID, transitionID string) (*TransitionConfigView, error) {
	enabled := false
	return s.UpdateTransition(ctx, projectID, transitionID, &UpdateTransitionInput{Enabled: &enabled})
}

// ConfigureCondition sets the transition's structured condition
// (T6.37.2): the condition is constructed through the P6-04 domain (field
// declared, operator in the fixed set, value exactly one of literal /
// context-field) and PERSISTED (not just returned); a nil input clears it.
func (s *TransitionConfigService) ConfigureCondition(ctx context.Context, projectID, transitionID string, in *TransitionConditionInput) (*TransitionConfigView, error) {
	cond, err := validateCondition(in)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var out *TransitionConfigView
	e := s.runOnce(ctx, func(tx persistence.Transaction) error {
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		rec, err := findTransition(ctx, r, machine, transitionID)
		if err != nil {
			return err
		}
		if cond == nil {
			if err := r.conditions.Remove(ctx, rec.ID); err != nil {
				return translate(err)
			}
			rec.Condition = nil
		} else {
			if err := r.conditions.Create(ctx, rec.ID, &persistence.TransitionCondition{
				Field: cond.Field, Operator: string(cond.Operator),
				Literal: cond.Value.Literal, ContextField: cond.Value.ContextField,
			}); err != nil {
				return translate(err)
			}
			rec.Condition = &persistence.TransitionCondition{
				Field: cond.Field, Operator: string(cond.Operator),
				Literal: cond.Value.Literal, ContextField: cond.Value.ContextField,
			}
		}
		view, err := transitionView(ctx, r, machine, rec)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// ConfigurePriority sets the transition's priority (T6.37.3): the value is
// stored and the ambiguity detector fires on the updated row (T6.37.5).
func (s *TransitionConfigService) ConfigurePriority(ctx context.Context, projectID, transitionID string, priority int) (*TransitionConfigView, error) {
	return s.UpdateTransition(ctx, projectID, transitionID, &UpdateTransitionInput{Priority: &priority})
}

// ListTransitions returns the current machine version's transitions (the
// read surface for the configuration state).
func (s *TransitionConfigService) ListTransitions(ctx context.Context, projectID string) ([]*TransitionConfigView, error) {
	var out []*TransitionConfigView
	err := s.runOnce(ctx, func(tx persistence.Transaction) error {
		out = nil // the closure re-runs on a retried transaction
		r := s.bind(tx)
		machine, err := s.resolveCurrent(ctx, r, projectID)
		if err != nil {
			return err
		}
		list, err := r.trans.ListByMachine(ctx, machine.ID, machine.Version)
		if err != nil {
			return translate(err)
		}
		for _, rec := range list {
			view, err := transitionView(ctx, r, machine, rec)
			if err != nil {
				return err
			}
			out = append(out, view)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
