package state

import (
	"testing"

	"github.com/google/uuid"
)

func validStateInput(machineID uuid.UUID) StateInput {
	return StateInput{
		StateMachineID: machineID,
		Key:            "implement",
		Name:           "Implement",
		Description:    "Implement the current task.",
		Kind:           KindAction,
	}
}

// T6.2.1 — Valid state created.
func TestT621ValidStateCreated(t *testing.T) {
	mid := uuid.New()
	s, err := NewState(validStateInput(mid))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	if s.ID == uuid.Nil {
		t.Fatalf("new state has no ID")
	}
	if s.StateMachineID != mid {
		t.Fatalf("state machine association = %v, want %v", s.StateMachineID, mid)
	}
	if s.Key != "IMPLEMENT" {
		t.Fatalf("key not normalized: %q", s.Key)
	}
	if s.Name != "Implement" || s.Description != "Implement the current task." {
		t.Fatalf("name/description not preserved: %q / %q", s.Name, s.Description)
	}
	if s.Kind != KindAction {
		t.Fatalf("kind = %q, want %q", s.Kind, KindAction)
	}
	if !s.Enabled {
		t.Fatalf("new state must start enabled")
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be set on creation")
	}
}

// T6.2.2 — Empty state key rejected.
func TestT622EmptyStateKeyRejected(t *testing.T) {
	mid := uuid.New()
	for _, key := range []string{"", "   ", "\t\n"} {
		in := validStateInput(mid)
		in.Key = key
		if _, err := NewState(in); err == nil {
			t.Fatalf("key %q: expected rejection, got none", key)
		}
	}
}

// T6.2.3 — Duplicate key within machine rejected (domain half).
func TestT623DuplicateKeyWithinMachineRejected(t *testing.T) {
	mid := uuid.New()
	existing, err := NewState(validStateInput(mid))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	for _, key := range []string{"IMPLEMENT", "implement", " Implement "} {
		candidate := StateInput{StateMachineID: mid, Key: key, Name: "Dup", Kind: KindAction}
		if err := ExistingKeyInScope(existing, candidate); err == nil {
			t.Fatalf("key %q in same machine: expected rejection, got none", key)
		}
	}
}

// T6.2.4 — Same key in different machines allowed (domain half).
func TestT624SameKeyInDifferentMachinesAllowed(t *testing.T) {
	existing, err := NewState(validStateInput(uuid.New()))
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	candidate := StateInput{
		StateMachineID: uuid.New(),
		Key:            "IMPLEMENT",
		Name:           "Implement",
		Kind:           KindAction,
	}
	if err := ExistingKeyInScope(existing, candidate); err != nil {
		t.Fatalf("same key in a different machine must be allowed, got: %v", err)
	}
}

// T6.2.5 — Unknown state kind rejected.
func TestT625UnknownStateKindRejected(t *testing.T) {
	mid := uuid.New()
	in := validStateInput(mid)
	in.Kind = StateKind("BOGUS")
	if _, err := NewState(in); err == nil {
		t.Fatalf("unknown kind: expected rejection, got none")
	}
	for _, kind := range []StateKind{KindAction, KindDecision, KindValidation, KindControl, KindTerminal} {
		in := validStateInput(mid)
		in.Kind = kind
		if _, err := NewState(in); err != nil {
			t.Fatalf("known kind %q rejected: %v", kind, err)
		}
	}
}

// T6.2.6 — Terminal flag persisted on the value.
func TestT626TerminalFlagPersisted(t *testing.T) {
	mid := uuid.New()
	in := validStateInput(mid)
	in.Terminal = true
	s, err := NewState(in)
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	if !s.Terminal {
		t.Fatalf("terminal flag not persisted on the value")
	}
	if _, err := NewState(validStateInput(mid)); err != nil {
		t.Fatalf("NewState (default): %v", err)
	}
}
