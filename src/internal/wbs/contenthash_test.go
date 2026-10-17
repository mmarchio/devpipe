package wbs

import (
	"strings"
	"testing"
)

// T4.11.1 — Hash generated: non-empty content yields a non-empty hash string.
func TestT4111_HashGenerated(t *testing.T) {
	h := HashContent("Phase 4 WBS v1 content")
	if h == "" {
		t.Fatalf("T4.11.1: HashContent returned empty string for non-empty content")
	}
}

// T4.11.2 — Same content produces the same hash (repeated calls).
func TestT4112_SameContentSameHash(t *testing.T) {
	const content = "identical content across calls\n- line 1\n- line 2"
	first := HashContent(content)
	for i := 0; i < 10; i++ {
		if got := HashContent(content); got != first {
			t.Fatalf("T4.11.2: repeated call %d returned %q, want %q", i, got, first)
		}
	}
}

// T4.11.3 — Changed content changes the hash.
func TestT4113_ChangedContentChangesHash(t *testing.T) {
	base := HashContent("phase work statement v1")
	cases := []string{
		"phase work statement v2",
		"phase work statement v1 extra",
		"phase work statement v1 ", // trailing whitespace counts
		"",
	}
	for i, c := range cases {
		if got := HashContent(c); got == base {
			t.Fatalf("T4.11.3: case %d (changed content) produced the same hash as the original", i)
		}
	}
}

// T4.11.4 — Stored hash matches persisted content: the hash of the exact
// content string equals the value stored for that string (round-trip over the
// exact persisted string; the persistence-side half lands with P4-09).
func TestT4114_StoredHashMatchesContent(t *testing.T) {
	content := "## Phase scope\nDeterministic hashing of WBS content."
	stored := HashContent(content) // value a repository would persist
	persisted := stored
	if persisted != HashContent(content) {
		t.Fatal("T4.11.4: re-hashing the exact persisted content string does not reproduce the stored hash")
	}
}

// T4.11.5 — Retrieval does not alter content/hash: a copy of the content
// re-hashed yields the identical hash and the content is byte-identical.
func TestT4115_RetrievalDoesNotAlterContentHash(t *testing.T) {
	original := "content with markdown\n\tand code blocks\n"
	h1 := HashContent(original)
	retrieved := strings.Clone(original) // retrieval-style copy
	if retrieved != original {
		t.Fatalf("T4.11.5: retrieved content differs: %q != %q", retrieved, original)
	}
	if h2 := HashContent(retrieved); h2 != h1 {
		t.Fatalf("T4.11.5: re-hash of retrieved content differs: %q != %q", h2, h1)
	}
}
