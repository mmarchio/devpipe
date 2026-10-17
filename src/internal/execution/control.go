package execution

import (
	"errors"
	"fmt"
	"sort"

	"overseer/internal/state"
)

// ReachableStates returns the enabled states reachable from the machine's
// initial state over enabled transitions, in key order (WBS 15.31/15.34).
func ReachableStates(m *state.StateMachineSnapshot) []state.StateSnapshot {
	if m == nil {
		return nil
	}
	byKey := map[string]state.StateSnapshot{}
	for _, s := range m.States {
		if s.Enabled {
			byKey[s.Key] = s
		}
	}
	if _, ok := byKey[m.InitialState.Key]; !ok {
		if !m.InitialState.Enabled {
			return nil
		}
		byKey[m.InitialState.Key] = m.InitialState
	}
	seen := map[string]bool{m.InitialState.Key: true}
	queue := []string{m.InitialState.Key}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		for _, t := range m.Transitions {
			if !t.Enabled || t.From != k || seen[t.To] {
				continue
			}
			if _, ok := byKey[t.To]; !ok {
				continue
			}
			seen[t.To] = true
			queue = append(queue, t.To)
		}
	}
	var out []state.StateSnapshot
	for k := range seen {
		out = append(out, byKey[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ControlPath is the deterministic bootstrap traversal (WBS 15.59).
type ControlPath struct {
	InitialState    state.StateSnapshot
	Transitions     []state.TransitionSnapshot
	FirstActionable state.StateSnapshot
}

// ErrControlResolution reports a malformed/unsupported workflow for
// bootstrap (STATE_MACHINE_INVALID).
var ErrControlResolution = errors.New("execution: control-state resolution failed")

// IsControl reports a pure control state (WBS 15.58).
func IsControl(s state.StateSnapshot) bool { return s.Kind == string(state.KindControl) }

// ResolveFirstActionable walks control states from the machine's
// configured initial state (WBS 15.30) using the routing facts' outcome
// preference until the first non-control state (WBS 15.59–15.63). A control
// state without a matching enabled transition, a terminal state, or more
// than maxTransitions steps (cycle, WBS 15.60) is an error.
func ResolveFirstActionable(m *state.StateMachineSnapshot, facts RoutingFacts, maxTransitions int) (*ControlPath, error) {
	if m == nil || m.InitialState.Key == "" {
		return nil, fmt.Errorf("%w: machine has no initial state", ErrControlResolution)
	}
	states := map[string]state.StateSnapshot{}
	for _, s := range m.States {
		if s.Enabled {
			states[s.Key] = s
		}
	}
	cur, ok := states[m.InitialState.Key]
	if !ok {
		return nil, fmt.Errorf("%w: initial state %s is not enabled", ErrControlResolution, m.InitialState.Key)
	}
	path := &ControlPath{InitialState: cur}
	prefs := facts.OutcomePreference()
	for steps := 0; ; steps++ {
		if cur.Terminal || cur.Kind == string(state.KindTerminal) {
			return nil, fmt.Errorf("%w: control path reached terminal state %s", ErrControlResolution, cur.Key)
		}
		if !IsControl(cur) {
			path.FirstActionable = cur
			return path, nil
		}
		if steps >= maxTransitions {
			return nil, fmt.Errorf("%w: control-state transition limit %d exceeded at %s", ErrControlResolution, maxTransitions, cur.Key)
		}
		var chosen *state.TransitionSnapshot
		for _, outcome := range prefs {
			t, err := selectSnapshotTransition(m.Transitions, cur.Key, outcome)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrControlResolution, err)
			}
			if t != nil {
				chosen = t
				break
			}
		}
		if chosen == nil {
			return nil, fmt.Errorf("%w: control state %s has no transition for outcomes %v", ErrControlResolution, cur.Key, prefs)
		}
		next, ok := states[chosen.To]
		if !ok {
			return nil, fmt.Errorf("%w: transition %s -> %s targets a disabled state", ErrControlResolution, chosen.From, chosen.To)
		}
		path.Transitions = append(path.Transitions, *chosen)
		cur = next
	}
}

// selectSnapshotTransition applies the P6-14 rule (enabled, matching
// outcome, lowest priority; tie = ambiguity) over snapshot transitions.
func selectSnapshotTransition(ts []state.TransitionSnapshot, from, outcome string) (*state.TransitionSnapshot, error) {
	var matches []state.TransitionSnapshot
	for _, t := range ts {
		if t.Enabled && t.From == from && state.MatchOutcome(t.Outcome, outcome) {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Priority < matches[j].Priority })
	if len(matches) > 1 && matches[0].Priority == matches[1].Priority {
		return nil, fmt.Errorf("ambiguous transitions from %s on %q", from, outcome)
	}
	t := matches[0]
	return &t, nil
}
