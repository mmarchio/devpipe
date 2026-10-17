package wbs

import (
	"errors"
	"reflect"
	"testing"
)

// Granularity note (task-level choice, recorded in ACTIVE_TASK.md NOTES): the
// diff is LINE-BASED — content strings are split on "\n" and the LCS alignment
// runs over lines. This fits markdown WBS documents and keeps the primitive
// pure and byte-deterministic.

// T4.17.1 — Added content identified: lines present only in the new version
// surface as added hunks carrying exactly those lines, and repeated calls
// yield byte-identical output (determinism).
func TestT4171_AddedContentIdentified(t *testing.T) {
	from := "# Phase 4 WBS v1\n- unit A\n- unit B"
	to := "# Phase 4 WBS v1\n- unit A\n- unit B\n- unit C"
	d, err := DiffContents(&from, &to)
	if err != nil {
		t.Fatalf("T4.17.1: unexpected error: %v", err)
	}
	if len(d.Hunks) != 1 {
		t.Fatalf("T4.17.1: expected exactly one hunk, got %d: %+v", len(d.Hunks), d.Hunks)
	}
	if d.Hunks[0].Operation != OpAdded {
		t.Fatalf("T4.17.1: expected added hunk, got %q", d.Hunks[0].Operation)
	}
	want := []string{"- unit C"}
	if !reflect.DeepEqual(d.Hunks[0].ToLines, want) {
		t.Fatalf("T4.17.1: added lines = %q, want %q", d.Hunks[0].ToLines, want)
	}
	if len(d.Hunks[0].FromLines) != 0 {
		t.Fatalf("T4.17.1: added hunk must carry no old-side lines, got %q", d.Hunks[0].FromLines)
	}
	// Determinism: same inputs must yield byte-identical results across runs.
	for i := 0; i < 10; i++ {
		again, err := DiffContents(&from, &to)
		if err != nil {
			t.Fatalf("T4.17.1: repeat %d: unexpected error: %v", i, err)
		}
		if !reflect.DeepEqual(d, again) {
			t.Fatalf("T4.17.1: repeat %d produced a different diff: %+v vs %+v", i, again, d)
		}
	}
}

// T4.17.2 — Removed content identified: lines present only in the old version
// surface as removed hunks carrying exactly those lines.
func TestT4172_RemovedContentIdentified(t *testing.T) {
	from := "# Phase 4 WBS v2\n- unit A\n- unit B"
	to := "# Phase 4 WBS v2\n- unit A"
	d, err := DiffContents(&from, &to)
	if err != nil {
		t.Fatalf("T4.17.2: unexpected error: %v", err)
	}
	if len(d.Hunks) != 1 {
		t.Fatalf("T4.17.2: expected exactly one hunk, got %d: %+v", len(d.Hunks), d.Hunks)
	}
	if d.Hunks[0].Operation != OpRemoved {
		t.Fatalf("T4.17.2: expected removed hunk, got %q", d.Hunks[0].Operation)
	}
	want := []string{"- unit B"}
	if !reflect.DeepEqual(d.Hunks[0].FromLines, want) {
		t.Fatalf("T4.17.2: removed lines = %q, want %q", d.Hunks[0].FromLines, want)
	}
	if len(d.Hunks[0].ToLines) != 0 {
		t.Fatalf("T4.17.2: removed hunk must carry no new-side lines, got %q", d.Hunks[0].ToLines)
	}
}

// T4.17.3 — Modified content represented: a changed line is expressed as a
// single modified hunk pairing the old-side lines with the new-side lines
// (not as two separate hunks), including multi-line modifications.
func TestT4173_ModifiedContentRepresented(t *testing.T) {
	from := "## Dependencies\n- use pgx v5\n- pin Go 1.24"
	to := "## Dependencies\n- use pgx v6\n- pin Go 1.25"
	d, err := DiffContents(&from, &to)
	if err != nil {
		t.Fatalf("T4.17.3: unexpected error: %v", err)
	}
	if len(d.Hunks) != 1 {
		t.Fatalf("T4.17.3: expected exactly one modified hunk, got %d: %+v", len(d.Hunks), d.Hunks)
	}
	if d.Hunks[0].Operation != OpModified {
		t.Fatalf("T4.17.3: expected modified hunk, got %q", d.Hunks[0].Operation)
	}
	if want := []string{"- use pgx v5", "- pin Go 1.24"}; !reflect.DeepEqual(d.Hunks[0].FromLines, want) {
		t.Fatalf("T4.17.3: old-side lines = %q, want %q", d.Hunks[0].FromLines, want)
	}
	if want := []string{"- use pgx v6", "- pin Go 1.25"}; !reflect.DeepEqual(d.Hunks[0].ToLines, want) {
		t.Fatalf("T4.17.3: new-side lines = %q, want %q", d.Hunks[0].ToLines, want)
	}
}

// T4.17.4 — Same versions produce empty diff: identical contents (and the
// same content value passed for both sides) yield zero hunks.
func TestT4174_SameVersionEmptyDiff(t *testing.T) {
	content := "## Phase 4\n- unit A\n- unit B\n"
	d, err := DiffContents(&content, &content)
	if err != nil {
		t.Fatalf("T4.17.4: unexpected error: %v", err)
	}
	if len(d.Hunks) != 0 {
		t.Fatalf("T4.17.4: identical content must produce an empty diff, got %+v", d.Hunks)
	}
	// Two distinct variables holding the same value (two versions with
	// identical content) are covered by the same expectation.
	from, to := content, content
	d2, err := DiffContents(&from, &to)
	if err != nil {
		t.Fatalf("T4.17.4: unexpected error: %v", err)
	}
	if len(d2.Hunks) != 0 {
		t.Fatalf("T4.17.4: equal-valued contents must produce an empty diff, got %+v", d2.Hunks)
	}
}

// T4.17.5 — Invalid/missing version content rejected (primitive half; the
// REST version-pair rejection is P4-24's endpoint half): a nil content pointer
// means the version content is missing and must be rejected, not diffed.
func TestT4175_MissingVersionContentRejected(t *testing.T) {
	content := "## Phase 4\n- unit A"
	if _, err := DiffContents(nil, &content); !errors.Is(err, ErrMissingContent) {
		t.Fatalf("T4.17.5: missing from-content: err = %v, want ErrMissingContent", err)
	}
	if _, err := DiffContents(&content, nil); !errors.Is(err, ErrMissingContent) {
		t.Fatalf("T4.17.5: missing to-content: err = %v, want ErrMissingContent", err)
	}
	if _, err := DiffContents(nil, nil); !errors.Is(err, ErrMissingContent) {
		t.Fatalf("T4.17.5: both contents missing: err = %v, want ErrMissingContent", err)
	}
}
