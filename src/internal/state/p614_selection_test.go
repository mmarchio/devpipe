// P6-14 (WBS 6.14 / 6.15): deterministic priority-based transition selection
// and ambiguity detection. Pure functions over declared inputs — no DB in
// the selection/detection logic so the architecture guard stays green.
// Integration tests in internal/integration drive them with real persisted
// transition rows.
package state

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// p614Machine returns a fresh machine ID and two state IDs for test fixtures.
func p614Machine(t *testing.T) (machineID, stateA, stateB uuid.UUID) {
	t.Helper()
	return uuid.New(), uuid.New(), uuid.New()
}

// p614Transition builds a StateTransition for test fixtures.
func p614Transition(machineID, from, to uuid.UUID, outcome string, priority int, enabled bool) *StateTransition {
	tr, err := NewTransition(TransitionInput{
		StateMachineID: machineID,
		FromStateID:    from,
		ToStateID:      to,
		Outcome:        outcome,
		Priority:       priority,
	})
	if err != nil {
		panic(err)
	}
	tr.Enabled = enabled
	return tr
}

// T6.14.1 — Highest-priority matching transition selected.
// Priority policy: lower numeric = higher priority.
func TestT6141HighestPrioritySelected(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "ERROR", 30, true),
		p614Transition(mid, a, b, "BLOCKED", 20, true),
		p614Transition(mid, a, b, "COMPLETE", 10, true),
	}
	got, err := SelectTransition(transitions, a, "COMPLETE")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil {
		t.Fatal("no transition selected")
	}
	if got.Outcome != "COMPLETE" || got.Priority != 10 {
		t.Fatalf("selected = %q (pri %d), want COMPLETE (pri 10)", got.Outcome, got.Priority)
	}
}

// T6.14.1 — Lower numeric priority wins when outcomes match.
func TestT6141LowerNumericWins(t *testing.T) {
	mid, a, b := p614Machine(t)
	// Three outcomes all match the context outcome via case-insensitive match.
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "pass", 20, true),
		p614Transition(mid, a, b, "PASS", 10, true),
	}
	got, err := SelectTransition(transitions, a, "PASS")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil {
		t.Fatal("no transition selected")
	}
	if got.Priority != 10 {
		t.Fatalf("selected priority = %d, want 10 (lower numeric = higher priority)", got.Priority)
	}
}

// T6.14.2 — Nonmatching transitions skipped.
func TestT6142NonmatchingSkipped(t *testing.T) {
	mid, a, b := p614Machine(t)
	// ERROR is the only match; BLOCKED and COMPLETE have lower priority
	// numbers but do not match the context outcome.
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "ERROR", 10, true),
		p614Transition(mid, a, b, "BLOCKED", 5, true),
		p614Transition(mid, a, b, "COMPLETE", 1, true),
	}
	got, err := SelectTransition(transitions, a, "ERROR")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil {
		t.Fatal("no transition selected")
	}
	if got.Outcome != "ERROR" {
		t.Fatalf("selected = %q, want ERROR (nonmatching must be skipped)", got.Outcome)
	}
}

// T6.14.2 — Disabled transitions are skipped even when they match.
func TestT6142DisabledSkipped(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 1, false), // disabled
		p614Transition(mid, a, b, "pass", 10, true), // enabled, matches
	}
	got, err := SelectTransition(transitions, a, "PASS")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil {
		t.Fatal("no transition selected")
	}
	if got.Priority != 10 {
		t.Fatalf("selected priority = %d, want 10 (disabled transition must be skipped)", got.Priority)
	}
}

// T6.14.3 — Priority ties causing ambiguity rejected.
func TestT6143PriorityTieRejected(t *testing.T) {
	mid, a, b := p614Machine(t)
	// Two transitions with the same outcome (case variant = same under
	// the case policy) and the same priority.
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 10, true),
		p614Transition(mid, a, b, "pass", 10, true),
	}
	_, err := SelectTransition(transitions, a, "PASS")
	if err == nil {
		t.Fatal("equal-priority tie: expected ambiguity rejection, got none")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ambig") {
		t.Fatalf("error should mention ambiguity: %v", err)
	}
}

// T6.14.3 — No match at all returns (nil, nil).
func TestT6143NoMatchReturnsNil(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 10, true),
	}
	got, err := SelectTransition(transitions, a, "FAIL")
	if err != nil {
		t.Fatalf("SelectTransition (no match): %v", err)
	}
	if got != nil {
		t.Fatalf("no match: got %v, want nil", got)
	}
}

// T6.14.4 — Ordering stable across restart: the selection is a pure function
// over persisted inputs; calling it repeatedly on the same data yields the
// same result. The tie-breaking rule is: ambiguity is rejected (T6.14.3), so
// the stable ordering follows from (priority ascending) alone.
func TestT6144StableOrdering(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "COMPLETE", 30, true),
		p614Transition(mid, a, b, "BLOCKED", 20, true),
		p614Transition(mid, a, b, "ERROR", 10, true),
	}
	for i := 0; i < 20; i++ {
		got, err := SelectTransition(transitions, a, "COMPLETE")
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if got.Outcome != "COMPLETE" {
			t.Fatalf("iteration %d: selected = %q, want COMPLETE", i, got.Outcome)
		}
	}
}

// T6.15.1 — Unambiguous transition sets accepted.
func TestT6151UnambiguousAccepted(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "ERROR", 10, true),
		p614Transition(mid, a, b, "BLOCKED", 20, true),
		p614Transition(mid, a, b, "COMPLETE", 30, true),
	}
	rep := DetectAmbiguity(transitions)
	if !rep.Valid {
		t.Fatalf("unambiguous set rejected: groups = %v", rep.Groups)
	}
}

// T6.15.1 — Empty set is trivially unambiguous.
func TestT6151EmptySetValid(t *testing.T) {
	rep := DetectAmbiguity(nil)
	if !rep.Valid {
		t.Fatal("empty set: expected valid, got invalid")
	}
}

// T6.15.2 — Duplicate outcome/condition/priority (equal-priority ambiguity)
// rejected.
func TestT6152DuplicateRejected(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 10, true),
		p614Transition(mid, a, b, "pass", 10, true),
	}
	rep := DetectAmbiguity(transitions)
	if rep.Valid {
		t.Fatal("duplicate outcome/priority: expected rejection, got valid")
	}
}

// T6.15.2 — Disabled transitions do not participate in ambiguity detection.
func TestT6152DisabledIgnored(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 10, true),
		p614Transition(mid, a, b, "pass", 10, false), // disabled — ignored
	}
	rep := DetectAmbiguity(transitions)
	if !rep.Valid {
		t.Fatalf("disabled transition must not cause ambiguity: groups = %v", rep.Groups)
	}
}

// T6.15.3 — Distinct priorities accepted (same outcome, different priority).
func TestT6153DistinctPrioritiesAccepted(t *testing.T) {
	mid, a, b := p614Machine(t)
	transitions := []*StateTransition{
		p614Transition(mid, a, b, "PASS", 10, true),
		p614Transition(mid, a, b, "pass", 20, true),
	}
	rep := DetectAmbiguity(transitions)
	if !rep.Valid {
		t.Fatalf("distinct priorities rejected: groups = %v", rep.Groups)
	}
}

// T6.15.4 — Validation reports the offending state and transitions, not just
// an opaque error.
func TestT6154ReportsOffending(t *testing.T) {
	mid, a, b := p614Machine(t)
	t1 := p614Transition(mid, a, b, "PASS", 10, true)
	t2 := p614Transition(mid, a, b, "pass", 10, true)
	transitions := []*StateTransition{t1, t2}
	rep := DetectAmbiguity(transitions)
	if rep.Valid {
		t.Fatal("expected ambiguity, got valid")
	}
	if len(rep.Groups) == 0 {
		t.Fatal("no offending groups reported")
	}
	g := rep.Groups[0]
	if g.SourceStateID != a {
		t.Fatalf("offending state = %v, want %v", g.SourceStateID, a)
	}
	if len(g.Transitions) != 2 {
		t.Fatalf("offending transitions = %d, want 2", len(g.Transitions))
	}
	found := map[uuid.UUID]bool{}
	for _, at := range g.Transitions {
		found[at.ID] = true
	}
	if !found[t1.ID] || !found[t2.ID] {
		t.Fatal("offending transition IDs not fully reported")
	}
}
