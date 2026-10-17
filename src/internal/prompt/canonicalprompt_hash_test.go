package prompt

// P8-31 — WBS 8.27 canonical prompt hash, TDD T8.27.1-T8.27.5.
//
// CanonicalPromptHash is the deterministic hash over the final canonical
// prompt and its semantic metadata. Exact hash-input set (stated per the
// task NOTES so TESTER/PM can verify):
//
//  1. canonical content          (the rendered prompt, byte-for-byte)
//  2. template version           (pinned integer)
//  3. snapshot hash              (the context snapshot's content hash — it
//     stands in for the state files the snapshot pins, T8.27.2)
//  4. response protocol version  (pinned integer)
//  5. role version               (pinned integer — role-instruction changes
//     advance it, T8.27.4)
//  6. state-machine version + state key (pinned identity of the state the
//     prompt was composed for)
//
// Continuation status changes alter the rendered canonical content itself
// (the continuation component is part of the render), so T8.27.5 is pinned
// through the content input. The serialization is length-prefixed per field
// (a field name prefix + a 8-byte length + the bytes) so that no field
// boundary can be ambiguous and no input can cancel another's effect.
// Deterministic: same inputs → same hash; each input change → new hash.

import "testing"

func cpHashFixture() CanonicalPromptHashInput {
	return CanonicalPromptHashInput{
		Content:                 "CANONICAL PROMPT CONTENT v1",
		TemplateVersion:         3,
		SnapshotHash:            "aa" + "11",
		ResponseProtocolVersion: 2,
		RoleVersion:             1,
		StateMachineVersion:     4,
		StateKey:                "IMPLEMENT",
	}
}

// T8.27.1 — Same inputs produce same hash. Repeated hashing of identical
// inputs must be byte-identical.
func TestT8271_SameInputsSameHash(t *testing.T) {
	first, err := CanonicalPromptHash(cpHashFixture())
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := CanonicalPromptHash(cpHashFixture())
		if err != nil {
			t.Fatalf("CanonicalPromptHash repeat %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("same inputs produced different hashes: %q vs %q", first, again)
		}
	}
	if len(first) != 64 {
		t.Fatalf("expected a 64-hex-char sha-256 digest, got %q (len %d)", first, len(first))
	}
}

// T8.27.2 — State-file change changes hash. State files ride on the context
// snapshot: a state-file edit moves the snapshot's content hash, and the
// snapshot hash is a hash input, so the prompt hash must move with it.
func TestT8272_StateFileChangeChangesHash(t *testing.T) {
	base := cpHashFixture()
	h0, err := CanonicalPromptHash(base)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	changed := base
	changed.SnapshotHash = "bb" + "22"
	h1, err := CanonicalPromptHash(changed)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("snapshot (state-file) hash change did not move the prompt hash: %q", h0)
	}
}

// T8.27.3 — Template change changes hash. A template version bump is a
// direct hash input.
func TestT8273_TemplateChangeChangesHash(t *testing.T) {
	base := cpHashFixture()
	h0, err := CanonicalPromptHash(base)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	changed := base
	changed.TemplateVersion = 4
	h1, err := CanonicalPromptHash(changed)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("template version change did not move the prompt hash: %q", h0)
	}
}

// T8.27.4 — Role instruction change changes hash. Role-instruction edits
// advance the role's version, and the role version is a hash input.
func TestT8274_RoleInstructionChangeChangesHash(t *testing.T) {
	base := cpHashFixture()
	h0, err := CanonicalPromptHash(base)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	changed := base
	changed.RoleVersion = 2
	h1, err := CanonicalPromptHash(changed)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("role version change did not move the prompt hash: %q", h0)
	}
}

// T8.27.5 — Continuation status change changes hash. A continuation-status
// change alters the rendered canonical content (the continuation component),
// so the content input moves the hash.
func TestT8275_ContinuationStatusChangeChangesHash(t *testing.T) {
	base := cpHashFixture()
	h0, err := CanonicalPromptHash(base)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	changed := base
	changed.Content = base.Content + "\n\n[continuation: MORE_WORK 55%]"
	h1, err := CanonicalPromptHash(changed)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("continuation status (content) change did not move the prompt hash: %q", h0)
	}
}

// Determinism pin (companion to T8.27.1): each remaining hash input also
// moves the hash on its own.
func TestT827X_EachRemainingInputMovesHash(t *testing.T) {
	base := cpHashFixture()
	h0, err := CanonicalPromptHash(base)
	if err != nil {
		t.Fatalf("CanonicalPromptHash: %v", err)
	}
	variants := []struct {
		name   string
		modify func(*CanonicalPromptHashInput)
	}{
		{"content", func(i *CanonicalPromptHashInput) { i.Content = "OTHER CONTENT" }},
		{"response protocol version", func(i *CanonicalPromptHashInput) { i.ResponseProtocolVersion = 3 }},
		{"state-machine version", func(i *CanonicalPromptHashInput) { i.StateMachineVersion = 5 }},
		{"state key", func(i *CanonicalPromptHashInput) { i.StateKey = "IMPLEMENT_NEXT" }},
	}
	for _, v := range variants {
		changed := base
		v.modify(&changed)
		h1, err := CanonicalPromptHash(changed)
		if err != nil {
			t.Fatalf("CanonicalPromptHash (%s): %v", v.name, err)
		}
		if h1 == h0 {
			t.Fatalf("changing %s did not move the prompt hash: %q", v.name, h0)
		}
	}
}
