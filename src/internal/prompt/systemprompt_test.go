package prompt

import (
	"errors"
	"testing"

	"overseer/internal/role"
)

// P8-16 — WBS 8.10 system-prompt resolution, TDD T8.10.1-T8.10.4.
//
// Composition must use the exact system-prompt version captured in the
// RoleSnapshot (Phase 5, WBS 5.14), never "latest": a later role edit is
// ignored, a missing historical version fails composition, and the
// snapshot's source hash is retained on the resulting component.

func snap816(version int, content, hash string) role.RoleSnapshot {
	return role.RoleSnapshot{
		RoleKey: "ENGINEER",
		SystemPrompt: role.PromptSnapshot{
			Version:        version,
			Content:        content,
			ContentHash:    hash,
			Classification: role.ClassificationSystem,
		},
	}
}

// T8.10.1 — Snapshot version used: composition uses the snapshot's
// captured version and content, even when a different current version
// exists.
func TestT8101_SnapshotVersionUsed(t *testing.T) {
	snap := snap816(4, "engineer-prompt-v4", "hash-v4")
	comp, err := ResolveSystemPrompt(snap)
	if err != nil {
		t.Fatalf("ResolveSystemPrompt(v4): %v", err)
	}
	if comp.SourceVersion != 4 {
		t.Fatalf("snapshot version must be used; want SourceVersion 4, got %d", comp.SourceVersion)
	}
	if comp.Content != "engineer-prompt-v4" {
		t.Fatalf("snapshot content not used; got %q", comp.Content)
	}
	if comp.Type != ComponentSystem {
		t.Fatalf("want a SYSTEM component, got %s", comp.Type)
	}
}

// T8.10.2 — Later role edit ignored: resolution reads only the snapshot;
// a later v6 current prompt has no input to the resolution and cannot
// leak into the result.
func TestT8102_LaterRoleEditIgnored(t *testing.T) {
	snap := snap816(4, "engineer-prompt-v4", "hash-v4")
	comp, err := ResolveSystemPrompt(snap)
	if err != nil {
		t.Fatalf("ResolveSystemPrompt(v4): %v", err)
	}
	if comp.SourceVersion != 4 || comp.Content != "engineer-prompt-v4" {
		t.Fatalf("later role edit (v6) leaked into composition: %+v", comp)
	}
}

// T8.10.3 — Missing historical version fails composition: a zero/cleared
// captured version resolves to a composition failure, not an implicit
// fallback to any other content.
func TestT8103_MissingHistoricalVersionFailsComposition(t *testing.T) {
	for _, snap := range []role.RoleSnapshot{
		snap816(0, "", ""),        // version never captured
		{RoleKey: "ENGINEER"},     // zero-value snapshot
		snap816(4, "", "hash-v4"), // captured version, no content
	} {
		_, err := ResolveSystemPrompt(snap)
		if err == nil {
			t.Fatalf("missing historical version must fail composition: %+v", snap.SystemPrompt)
		}
		if !errors.Is(err, ErrMissingSystemPromptVersion) {
			t.Fatalf("want ErrMissingSystemPromptVersion, got %v", err)
		}
	}
}

// T8.10.4 — Correct source hash retained: the component keeps the
// snapshot's content hash verbatim.
func TestT8104_CorrectSourceHashRetained(t *testing.T) {
	snap := snap816(4, "engineer-prompt-v4", "abc123hash")
	comp, err := ResolveSystemPrompt(snap)
	if err != nil {
		t.Fatalf("ResolveSystemPrompt(v4): %v", err)
	}
	if comp.SourceHash != "abc123hash" {
		t.Fatalf("source hash not retained; want %q, got %q", "abc123hash", comp.SourceHash)
	}
}

// Determinism: repeated resolution over the same snapshot is
// byte-identical (same composition-input requirement as T8.9.6).
func TestT8105_RepeatedResolutionByteIdentical(t *testing.T) {
	snap := snap816(4, "engineer-prompt-v4", "hash-v4")
	first, err := ResolveSystemPrompt(snap)
	if err != nil {
		t.Fatalf("ResolveSystemPrompt(v4): %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ResolveSystemPrompt(snap)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("repeated resolution not byte-identical at %d: %+v vs %+v", i, again, first)
		}
	}
}
