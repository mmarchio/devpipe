package state

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StateTransition is a directed, outcome-labelled edge between two workflow
// states of one machine (WBS 6.8). Pure domain: no persistence.
type StateTransition struct {
	ID             uuid.UUID
	StateMachineID uuid.UUID

	FromStateID uuid.UUID
	ToStateID   uuid.UUID

	Outcome  string
	Priority int

	// HasCondition marks a transition that carries at least one
	// structured condition (WBS 6.18): the condition is a context/progress
	// guard, so a loop using this transition depends on external progress
	// and is not zero-progress.
	HasCondition bool

	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StandardOutcomes is the initial generic outcome vocabulary (WBS 6.9). It is
// documentation/data: the domain never branches on individual outcome names,
// and any custom outcome string is accepted (T6.9.2).
var StandardOutcomes = []string{
	"SUCCESS", "COMPLETE", "INCOMPLETE", "PASS", "FAIL", "BLOCKED", "RETRY",
	"CONTINUE", "RESET", "YES", "NO", "ERROR", "CANCEL",
}

// Case policy (T6.9.4, documented here per WBS 6.9): outcomes are stored AS
// GIVEN (surrounding whitespace trimmed, original casing preserved) and
// matched case-insensitively. MatchOutcome implements the matching half;
// the storage half is asserted by the T6.9.x tests.
const outcomeCasePolicy = "stored as given; matched case-insensitively"

// TransitionInput carries the caller-supplied fields for NewTransition. ID
// and timestamps are assigned by the domain; Enabled defaults to true unless
// an explicit value is supplied (nil means "not specified").
type TransitionInput struct {
	StateMachineID uuid.UUID
	FromStateID    uuid.UUID
	ToStateID      uuid.UUID
	Outcome        string
	Priority       int
	Enabled        *bool
}

// NewTransition validates and constructs a new transition (WBS 6.8): machine
// association required, source state required (T6.8.2), destination state
// required (T6.8.3), non-empty outcome (T6.9.3; standard and custom outcomes
// both accepted, T6.9.1/T6.9.2), priority persisted on the value (T6.8.6),
// Enabled true by default, timestamps set (T6.8.1).
func NewTransition(in TransitionInput) (*StateTransition, error) {
	return newTransition(in, time.Now)
}

func newTransition(in TransitionInput, now func() time.Time) (*StateTransition, error) {
	if in.StateMachineID == uuid.Nil {
		return nil, fmt.Errorf("state: machine association is required")
	}
	if in.FromStateID == uuid.Nil {
		return nil, fmt.Errorf("state: source state is required")
	}
	if in.ToStateID == uuid.Nil {
		return nil, fmt.Errorf("state: destination state is required")
	}
	outcome := strings.TrimSpace(in.Outcome)
	if outcome == "" {
		return nil, fmt.Errorf("state: outcome must not be empty")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ts := now().UTC()
	return &StateTransition{
		ID:             uuid.New(),
		StateMachineID: in.StateMachineID,
		FromStateID:    in.FromStateID,
		ToStateID:      in.ToStateID,
		Outcome:        outcome,
		Priority:       in.Priority,
		Enabled:        enabled,
		CreatedAt:      ts,
		UpdatedAt:      ts,
	}, nil
}

// MachineMismatch reports an error when the transition's endpoints do not
// both belong to the candidate's machine (T6.8.4) — the same scope-check
// shape as the state-key rule: same-machine required, the pair is evaluated
// against the candidate's StateMachineID.
func MachineMismatch(from, to *WorkflowState, candidate TransitionInput) error {
	for _, s := range []*WorkflowState{from, to} {
		if s == nil {
			return fmt.Errorf("state: transition endpoint must belong to a machine")
		}
		if s.StateMachineID != candidate.StateMachineID {
			return fmt.Errorf("state: state %s belongs to machine %s, transition targets machine %s",
				s.ID, s.StateMachineID, candidate.StateMachineID)
		}
	}
	return nil
}

// DuplicateTransition reports an error when a candidate transition is
// ambiguous with an existing one (T6.8.5): same machine, same source state,
// and the same outcome under the documented case policy (T6.9.4).
func DuplicateTransition(existing *StateTransition, candidate TransitionInput) error {
	if existing == nil {
		return nil
	}
	if candidate.StateMachineID != existing.StateMachineID {
		return nil
	}
	if candidate.FromStateID != existing.FromStateID {
		return nil
	}
	if !MatchOutcome(existing.Outcome, candidate.Outcome) {
		return nil
	}
	return fmt.Errorf("state: ambiguous transition: state %s already transitions on outcome %q in machine %s",
		existing.FromStateID, existing.Outcome, existing.StateMachineID)
}

// MatchOutcome compares two outcome strings under the documented case policy
// (T6.9.4): case-insensitive equality after trimming.
func MatchOutcome(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
