package role

import (
	"testing"
)

// T5.6.1 — Hash generated.
func TestT561HashGenerated(t *testing.T) {
	h := HashSystemPromptContent("You are the implementation engineer.")
	if h == "" {
		t.Fatalf("hash must not be empty")
	}
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64 (sha256 hex)", len(h))
	}
}

// T5.6.2 — Same content produces same hash.
func TestT562SameContentSameHash(t *testing.T) {
	content := "You are the implementation engineer.\nImplement only the assigned work unit."
	a := HashSystemPromptContent(content)
	b := HashSystemPromptContent(content)
	if a != b {
		t.Fatalf("identical content produced different hashes: %s vs %s", a, b)
	}
	// Stability across calls with Unicode + multiline content.
	uni := "Rôle — 実装エンジニア\n- implement work units\n- café"
	if HashSystemPromptContent(uni) != HashSystemPromptContent(uni) {
		t.Fatalf("unicode content not stable")
	}
}

// T5.6.3 — Changed content produces different hash.
func TestT563ChangedContentDifferentHash(t *testing.T) {
	base := HashSystemPromptContent("v1 prompt")
	edits := []string{"v2 prompt", "V1 prompt", "v1 prompt ", "v1 prompt\n"}
	for _, e := range edits {
		if HashSystemPromptContent(e) == base {
			t.Fatalf("changed content %q produced same hash as base", e)
		}
	}
}

// T5.6.4 — Stored hash corresponds to stored content (domain half):
// the hash of known content is a fixed, reproducible value — if a caller
// persists (content, hash), re-deriving from content must yield the stored
// hash.
func TestT564StoredHashMatchesContent(t *testing.T) {
	// Known vector: the hash is a pure function, so the "stored" hash is
	// whatever the function returns for the content, and it must be stable.
	content := "You are the implementation engineer.\n\nImplement only the assigned work unit."
	stored := HashSystemPromptContent(content)
	derived := HashSystemPromptContent(content)
	if stored != derived {
		t.Fatalf("re-derived hash differs from stored: %s vs %s", stored, derived)
	}
	// The empty string has a well-defined hash (SHA-256 of empty input).
	if HashSystemPromptContent("") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty-content hash mismatch (expected SHA-256 of empty string)")
	}
}
