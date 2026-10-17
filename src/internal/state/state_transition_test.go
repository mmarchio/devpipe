package state

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validTransitionInput(machineID uuid.UUID, fromID, toID uuid.UUID) TransitionInput {
	return TransitionInput{
		StateMachineID: machineID,
		FromStateID:    fromID,
		ToStateID:      toID,
		Outcome:        "COMPLETE",
		Priority:       0,
	}
}

// T6.8.1 — Valid transition created.
func TestT681ValidTransitionCreated(t *testing.T) {
	mid := uuid.New()
	in := validTransitionInput(mid, uuid.New(), uuid.New())
	tr, err := NewTransition(in)
	if err != nil {
		t.Fatalf("NewTransition: %v", err)
	}
	if tr.ID == uuid.Nil {
		t.Fatalf("new transition has no ID")
	}
	if tr.StateMachineID != mid {
		t.Fatalf("machine association = %v, want %v", tr.StateMachineID, mid)
	}
	if tr.FromStateID != in.FromStateID || tr.ToStateID != in.ToStateID {
		t.Fatalf("from/to not preserved: %v -> %v", tr.FromStateID, tr.ToStateID)
	}
	if tr.Outcome != "COMPLETE" {
		t.Fatalf("outcome = %q, want %q", tr.Outcome, "COMPLETE")
	}
	if !tr.Enabled {
		t.Fatalf("new transition must start enabled")
	}
	if tr.CreatedAt.IsZero() || tr.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be set on creation")
	}
}

// T6.8.2 — Source state required.
func TestT682SourceStateRequired(t *testing.T) {
	mid := uuid.New()
	in := validTransitionInput(mid, uuid.Nil, uuid.New())
	if _, err := NewTransition(in); err == nil {
		t.Fatalf("zero FromStateID: expected rejection, got none")
	}
}

// T6.8.3 — Destination state required.
func TestT683DestinationStateRequired(t *testing.T) {
	mid := uuid.New()
	in := validTransitionInput(mid, uuid.New(), uuid.Nil)
	if _, err := NewTransition(in); err == nil {
		t.Fatalf("zero ToStateID: expected rejection, got none")
	}
}

// T6.8.4 — States must belong to the same machine (domain half).
func TestT684StatesMustBelongToSameMachine(t *testing.T) {
	mid := uuid.New()
	from, err := NewState(validStateInput(mid))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	to, err := NewState(validStateInput(mid))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	in := validTransitionInput(mid, from.ID, to.ID)
	if err := MachineMismatch(from, to, in); err != nil {
		t.Fatalf("states in the candidate machine rejected: %v", err)
	}
	foreign, err := NewState(validStateInput(uuid.New()))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	if err := MachineMismatch(foreign, to, in); err == nil {
		t.Fatalf("source in a different machine: expected rejection, got none")
	}
	if err := MachineMismatch(from, foreign, in); err == nil {
		t.Fatalf("destination in a different machine: expected rejection, got none")
	}
}

// T6.8.5 — Duplicate transition rejected where ambiguous (domain half).
func TestT685DuplicateTransitionRejected(t *testing.T) {
	mid := uuid.New()
	from, err := NewState(validStateInput(mid))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	existing, err := NewTransition(validTransitionInput(mid, from.ID, uuid.New()))
	if err != nil {
		t.Fatalf("NewTransition: %v", err)
	}
	// Same machine + same source + same outcome (any case): ambiguous.
	for _, outcome := range []string{"COMPLETE", "complete", " Complete "} {
		in := validTransitionInput(mid, from.ID, uuid.New())
		in.Outcome = outcome
		if err := DuplicateTransition(existing, in); err == nil {
			t.Fatalf("outcome %q same machine/source: expected rejection, got none", outcome)
		}
	}
	// Different source: distinct transition.
	in := validTransitionInput(mid, uuid.New(), uuid.New())
	if err := DuplicateTransition(existing, in); err != nil {
		t.Fatalf("different source must be allowed, got: %v", err)
	}
	// Different outcome: distinct transition.
	in = validTransitionInput(mid, from.ID, uuid.New())
	in.Outcome = "FAIL"
	if err := DuplicateTransition(existing, in); err != nil {
		t.Fatalf("different outcome must be allowed, got: %v", err)
	}
	// Same source + outcome in a different machine: distinct transition.
	in = validTransitionInput(uuid.New(), from.ID, uuid.New())
	if err := DuplicateTransition(existing, in); err != nil {
		t.Fatalf("different machine must be allowed, got: %v", err)
	}
}

// T6.8.6 — Transition priority persisted.
func TestT686TransitionPriorityPersisted(t *testing.T) {
	mid := uuid.New()
	in := validTransitionInput(mid, uuid.New(), uuid.New())
	in.Priority = 42
	tr, err := NewTransition(in)
	if err != nil {
		t.Fatalf("NewTransition: %v", err)
	}
	if tr.Priority != 42 {
		t.Fatalf("priority = %d, want 42", tr.Priority)
	}
	tr2, err := NewTransition(validTransitionInput(mid, uuid.New(), uuid.New()))
	if err != nil {
		t.Fatalf("NewTransition: %v", err)
	}
	if tr2.Priority != 0 {
		t.Fatalf("default priority = %d, want 0", tr2.Priority)
	}
}

// T6.9.1 — Standard outcomes accepted (data set, no branching on names).
func TestT691StandardOutcomeAccepted(t *testing.T) {
	mid := uuid.New()
	if len(StandardOutcomes) == 0 {
		t.Fatalf("StandardOutcomes must not be empty")
	}
	for _, oc := range StandardOutcomes {
		in := validTransitionInput(mid, uuid.New(), uuid.New())
		in.Outcome = oc
		if _, err := NewTransition(in); err != nil {
			t.Fatalf("standard outcome %q rejected: %v", oc, err)
		}
	}
	// The set is the WBS 6.9 list.
	want := []string{"SUCCESS", "COMPLETE", "INCOMPLETE", "PASS", "FAIL", "BLOCKED", "RETRY", "CONTINUE", "RESET", "YES", "NO", "ERROR", "CANCEL"}
	for _, w := range want {
		if !containsString(StandardOutcomes, w) {
			t.Fatalf("StandardOutcomes missing %q", w)
		}
	}
}

// T6.9.2 — Custom outcomes accepted.
func TestT692CustomOutcomeAccepted(t *testing.T) {
	mid := uuid.New()
	for _, oc := range []string{"DEPLOY-REVIEW", "ship-it", "42-CHECK"} {
		in := validTransitionInput(mid, uuid.New(), uuid.New())
		in.Outcome = oc
		tr, err := NewTransition(in)
		if err != nil {
			t.Fatalf("custom outcome %q rejected: %v", oc, err)
		}
		if tr.Outcome != oc {
			t.Fatalf("custom outcome stored as %q, want %q (stored as given)", tr.Outcome, oc)
		}
	}
}

// T6.9.3 — Empty outcome rejected.
func TestT693EmptyOutcomeRejected(t *testing.T) {
	mid := uuid.New()
	for _, oc := range []string{"", "   ", "\t\n"} {
		in := validTransitionInput(mid, uuid.New(), uuid.New())
		in.Outcome = oc
		if _, err := NewTransition(in); err == nil {
			t.Fatalf("outcome %q: expected rejection, got none", oc)
		}
	}
}

// T6.9.4 — Documented case policy: outcomes are stored as given (trimmed)
// and matched case-insensitively.
func TestT694OutcomeCasePolicy(t *testing.T) {
	mid := uuid.New()
	in := validTransitionInput(mid, uuid.New(), uuid.New())
	in.Outcome = "success"
	tr, err := NewTransition(in)
	if err != nil {
		t.Fatalf("NewTransition: %v", err)
	}
	if tr.Outcome != "success" {
		t.Fatalf("stored as given: outcome = %q, want %q", tr.Outcome, "success")
	}
	if !strings.EqualFold(tr.Outcome, "SUCCESS") {
		t.Fatalf("stored value must be comparable case-insensitively to SUCCESS")
	}
	for _, probe := range []string{"SUCCESS", "Success", "success"} {
		if !MatchOutcome(tr.Outcome, probe) {
			t.Fatalf("MatchOutcome(%q, %q) = false, want true", tr.Outcome, probe)
		}
	}
	if MatchOutcome(tr.Outcome, "CANCEL") {
		t.Fatalf("MatchOutcome must be exact modulo case")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
