// Deterministic priority-based transition selection (WBS 6.14) and
// ambiguity detection (WBS 6.15). Pure functions over declared inputs —
// no DB access so the architecture guard stays green.
package state

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// PriorityPolicy documents the priority ordering used throughout selection
// and ambiguity detection: LOWER numeric value = HIGHER priority (WBS 6.14
// example: priority 10 > 20 > 30). This is the single, consistent policy;
// the selection and detection functions share it.
const PriorityPolicy = "lower numeric value = higher priority"

// AmbiguityGroup is one ambiguous (source state, outcome, priority) group
// reported by DetectAmbiguity (WBS 6.15.4): the offending state and every
// transition that shares the group key.
type AmbiguityGroup struct {
	SourceStateID uuid.UUID
	Transitions   []*StateTransition
}

// AmbiguityReport is the result of DetectAmbiguity. Valid is true when no
// ambiguous group exists; Groups lists every offending group (deterministic
// order: sorted by source state, then outcome, then priority).
type AmbiguityReport struct {
	Valid  bool
	Groups []AmbiguityGroup
}

// groupKey is the internal grouping key for ambiguity detection.
type groupKey struct {
	SourceStateID     uuid.UUID
	NormalizedOutcome string // lowercased, trimmed
	Priority          int
}

// SelectTransition deterministically selects the highest-priority enabled
// transition matching the source state and outcome under the documented case
// policy (T6.9.4). Priority policy: lower numeric value = higher priority
// (WBS 6.14). Nonmatching transitions (different source state, disabled, or
// outcome mismatch under MatchOutcome) are skipped.
//
// Returns:
//   - the selected transition and nil error on a unique match;
//   - nil, nil when no transition matches (no ambiguity, no selection);
//   - nil and an ambiguity error when two or more matching transitions share
//     the same (minimum) priority (T6.14.3 / T6.15.2).
//
// The ordering is stable across restart (T6.14.4): the result depends only on
// the persisted (outcome, priority, enabled) data. The ambiguity rejection
// eliminates ties at the minimum priority, so the selected transition is
// uniquely determined.
func SelectTransition(transitions []*StateTransition, sourceStateID uuid.UUID, outcome string) (*StateTransition, error) {
	var matches []*StateTransition
	for _, tr := range transitions {
		if tr == nil {
			continue
		}
		if !tr.Enabled {
			continue
		}
		if tr.FromStateID != sourceStateID {
			continue
		}
		if !MatchOutcome(tr.Outcome, outcome) {
			continue
		}
		matches = append(matches, tr)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	// Sort by priority ascending (lower numeric = higher priority).
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Priority < matches[j].Priority
	})
	// T6.14.3 / T6.15.2: equal-priority tie at the minimum → ambiguity.
	if len(matches) > 1 && matches[0].Priority == matches[1].Priority {
		var offenders []*StateTransition
		for _, m := range matches {
			if m.Priority == matches[0].Priority {
				offenders = append(offenders, m)
			}
		}
		return nil, fmt.Errorf("state: ambiguous transition selection at priority %d for state %s outcome %q: %d transitions share the minimum priority (policy: %s)",
			matches[0].Priority, sourceStateID, outcome, len(offenders), PriorityPolicy)
	}
	return matches[0], nil
}

// DetectAmbiguity validates a set of transitions for equal-priority ambiguity
// (WBS 6.15). Two enabled transitions are ambiguous when they share the same
// source state, the same outcome under the documented case policy (T6.9.4 —
// case-insensitive after trim), and the same priority. Disabled transitions
// are excluded (they never participate in selection).
//
// The report lists every offending group with the source state ID and the
// full set of transitions in the group (T6.15.4). Groups are ordered
// deterministically (source state, normalized outcome, priority) so the
// output is stable across calls and restarts.
//
// Valid is true when no group has more than one transition.
func DetectAmbiguity(transitions []*StateTransition) AmbiguityReport {
	type acc struct {
		key         groupKey
		transitions []*StateTransition
	}
	byKey := make(map[groupKey]*acc)
	var order []groupKey
	for _, tr := range transitions {
		if tr == nil || !tr.Enabled {
			continue
		}
		k := groupKey{
			SourceStateID:     tr.FromStateID,
			NormalizedOutcome: strings.ToLower(strings.TrimSpace(tr.Outcome)),
			Priority:          tr.Priority,
		}
		if a, ok := byKey[k]; ok {
			a.transitions = append(a.transitions, tr)
		} else {
			byKey[k] = &acc{key: k, transitions: []*StateTransition{tr}}
			order = append(order, k)
		}
	}
	// Deterministic order: source state, normalized outcome, priority.
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.SourceStateID != b.SourceStateID {
			return a.SourceStateID.String() < b.SourceStateID.String()
		}
		if a.NormalizedOutcome != b.NormalizedOutcome {
			return a.NormalizedOutcome < b.NormalizedOutcome
		}
		return a.Priority < b.Priority
	})
	var groups []AmbiguityGroup
	for _, k := range order {
		a := byKey[k]
		if len(a.transitions) > 1 {
			groups = append(groups, AmbiguityGroup{
				SourceStateID: k.SourceStateID,
				Transitions:   a.transitions,
			})
		}
	}
	return AmbiguityReport{
		Valid:  len(groups) == 0,
		Groups: groups,
	}
}
