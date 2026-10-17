package continuation

import (
	"errors"
	"fmt"
)

// ErrIllegalTransition reports a lifecycle edge that is not allowed.
var ErrIllegalTransition = errors.New("continuation: illegal status transition")

// edges are the explicit lifecycle edges (WBS 14.52, PLAN Decision 2).
// FAILED and CANCELLED are added for every nonterminal state.
var edges = map[Status][]Status{
	StatusPending:          {StatusDeciding},
	StatusDeciding:         {StatusSameSession, StatusResetRequired, StatusRecoveryRequired},
	StatusSameSession:      {StatusRestoringCWD, StatusResetRequired},
	StatusResetRequired:    {StatusCreatingSession, StatusRecoveryRequired},
	StatusCreatingSession:  {StatusRestoringContext, StatusResetRequired, StatusRecoveryRequired},
	StatusRestoringContext: {StatusRestoringCWD},
	StatusRestoringCWD:     {StatusReady, StatusRecoveryRequired},
	StatusReady:            {StatusDispatching, StatusRestoringCWD, StatusResetRequired, StatusRecoveryRequired},
	StatusDispatching:      {StatusWaiting, StatusReady, StatusResetRequired, StatusRecoveryRequired},
	StatusWaiting:          {StatusCompleted, StatusBlocked, StatusRecoveryRequired, StatusResetRequired},
}

// CanTransition reports whether from → to is an allowed lifecycle edge.
func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() || from.IsTerminal() {
		return false
	}
	if to == StatusFailed || to == StatusCancelled {
		return true
	}
	for _, t := range edges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrIllegalTransition for a disallowed edge.
func ValidateTransition(from, to Status) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// Edges returns every allowed edge as (from, to) pairs in declaration order.
func Edges() [][2]Status {
	var out [][2]Status
	for _, f := range allStatuses {
		for _, t := range allStatuses {
			if CanTransition(f, t) {
				out = append(out, [2]Status{f, t})
			}
		}
	}
	return out
}
