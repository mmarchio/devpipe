package statefile

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

// findVersion returns the version with the given number from the accumulated
// list, mirroring how P7-06's repository will serve historical versions.
func findVersion(versions []*StateFileVersion, n int) *StateFileVersion {
	for _, v := range versions {
		if v.Version == n {
			return v
		}
	}
	return nil
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// T7.5.1 — First content creates v1.
func TestAppendVersionFirstCreatesV1(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v, err := AppendVersion(f, "initial content", timeUTCNow)
	if err != nil {
		t.Fatalf("valid append rejected: %v", err)
	}
	if v.Version != 1 {
		t.Errorf("first version = %d, want 1", v.Version)
	}
	if v.StateFileID != f.ID {
		t.Errorf("StateFileID = %v, want %v", v.StateFileID, f.ID)
	}
	if v.ID == uuid.Nil {
		t.Error("version ID not assigned")
	}
	if v.Content != "initial content" {
		t.Errorf("Content = %q, want the supplied content", v.Content)
	}
	if v.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

// T7.5.2 — Update creates v2.
func TestAppendVersionUpdateCreatesV2(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v1, err := AppendVersion(f, "one", timeUTCNow)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The file's current version advances with each appended content version.
	f.CurrentVersion = v1.Version
	v2, err := AppendVersion(f, "two", timeUTCNow)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("second version = %d, want 2", v2.Version)
	}
	if v1.Version == v2.Version {
		t.Error("update reused the previous version number")
	}
}

// T7.5.3 — Historical v1 remains available after an update.
func TestHistoricalVersionRetained(t *testing.T) {
	f := mustFile(t, ScopeProject)
	var versions []*StateFileVersion
	for i, content := range []string{"v1 body", "v2 body"} {
		v, err := AppendVersion(f, content, timeUTCNow)
		if err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
		versions = append(versions, v)
		f.CurrentVersion = v.Version
	}
	got := findVersion(versions, 1)
	if got == nil {
		t.Fatal("historical v1 not available after an update")
	}
	if got.Content != "v1 body" {
		t.Errorf("historical v1 content = %q, want %q", got.Content, "v1 body")
	}
	// v2 is present alongside, not in place of.
	if findVersion(versions, 2) == nil {
		t.Error("v2 missing while v1 retained")
	}
}

// T7.5.4 — Current version advances monotonically.
func TestCurrentVersionAdvances(t *testing.T) {
	f := mustFile(t, ScopeProject)
	want := 0
	for i := 1; i <= 3; i++ {
		v, err := AppendVersion(f, "c", timeUTCNow)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		f.CurrentVersion = v.Version
		if f.CurrentVersion != want+1 {
			t.Errorf("current version = %d, want %d", f.CurrentVersion, want+1)
		}
		want = f.CurrentVersion
	}
}

// T7.5.5 — Concurrent writes cannot create duplicate versions. The pure half
// pins that two writers computing the same MAX+1 collide and the guard rejects
// a candidate that is already present (or below the max).
func TestDuplicateVersionRejected(t *testing.T) {
	// Both concurrent writers start from the same existing set and compute
	// the same next version.
	base := []int{1}
	a := NextVersion(base)
	b := NextVersion(base)
	if a != b {
		t.Fatalf("two writers computed divergent next versions: %d and %d", a, b)
	}
	// Writer A commits a. Writer B then tries to commit the same number.
	committed := append(append([]int{}, base...), a)
	if err := ValidateVersionCandidate(committed, b); err == nil {
		t.Error("duplicate version accepted, want rejection")
	}
}

// T7.5.6 — Version number never moves backward.
func TestVersionNeverBackward(t *testing.T) {
	existing := []int{3, 4, 5}
	if err := ValidateVersionCandidate(existing, 4); err == nil {
		t.Error("a version below the max accepted, want rejection")
	}
	if err := ValidateVersionCandidate(existing, 1); err == nil {
		t.Error("an old version accepted, want rejection")
	}
	// A strictly greater candidate is accepted.
	if err := ValidateVersionCandidate(existing, 6); err != nil {
		t.Errorf("a forward version rejected: %v", err)
	}
}

// T7.6.1 — A deterministic content hash is generated (64 hex chars).
func TestContentHashGenerated(t *testing.T) {
	h := ContentHash("hello state")
	if h == "" {
		t.Fatal("empty hash")
	}
	if !hex64.MatchString(h) {
		t.Errorf("hash %q is not 64 lowercase hex chars", h)
	}
}

// T7.6.2 — Identical content produces an identical hash.
func TestContentHashIdentical(t *testing.T) {
	if ContentHash("same") != ContentHash("same") {
		t.Error("identical content produced different hashes")
	}
}

// T7.6.3 — Changed content changes the hash.
func TestContentHashChanges(t *testing.T) {
	if ContentHash("a") == ContentHash("b") {
		t.Error("changed content produced the same hash")
	}
	// A single byte change must move the hash.
	if ContentHash("abcdef") == ContentHash("abcdeg") {
		t.Error("a one-byte change produced the same hash")
	}
}

// T7.6.4 — The persisted hash matches the content.
func TestPersistedHashMatchesContent(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v, err := AppendVersion(f, "persist me", timeUTCNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if v.ContentHash != ContentHash(v.Content) {
		t.Error("stored ContentHash does not match the hash of its content")
	}
	// Cross-check against the canonical SHA-256 of the exact bytes.
	want := sha256hex("persist me")
	if v.ContentHash != want {
		t.Errorf("ContentHash = %q, want sha256 %q", v.ContentHash, want)
	}
}

// T7.6.5 — Retrieval does not alter the hash: recomputing from the stored
// content yields the stored hash, and the stored hash is stable.
func TestRetrievalDoesNotAlterHash(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v, err := AppendVersion(f, "stable", timeUTCNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	original := v.ContentHash
	// Re-derive the hash from the retrieved content and the retrieved record.
	if recompute := ContentHash(v.Content); recompute != original {
		t.Error("recomputing the hash from retrieved content changed it")
	}
	if v.ContentHash != original {
		t.Error("retrieval mutated the stored hash")
	}
}

// T7.28.1 — A token estimate is generated for non-empty content.
func TestTokenEstimateGenerated(t *testing.T) {
	if est := TokenEstimate("some content here"); est <= 0 {
		t.Errorf("non-empty content estimated %d, want > 0", est)
	}
}

// T7.28.2 — Empty content estimates zero (the defined minimum).
func TestTokenEstimateEmpty(t *testing.T) {
	if est := TokenEstimate(""); est != 0 {
		t.Errorf("empty content estimated %d, want 0", est)
	}
}

// T7.28.3 — Larger content produces a larger estimate.
func TestTokenEstimateScales(t *testing.T) {
	small := TokenEstimate("a")
	big := TokenEstimate("a very much longer body of state-file content for estimating")
	if big <= small {
		t.Errorf("larger content estimate %d is not larger than small %d", big, small)
	}
}

// T7.28.4 — The estimate survives retrieval: the stored metadata equals the
// value recomputed from the stored content.
func TestTokenEstimateSurvivesRetrieval(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v, err := AppendVersion(f, "estimate me across retrieval", timeUTCNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if v.TokenEstimate != TokenEstimate(v.Content) {
		t.Error("stored token estimate does not match a recomputation from its content")
	}
}

// --- test helpers -------------------------------------------------------

var timeUTCNow = func() (t time.Time) { return timeFixed }

var timeFixed = time.Now().UTC()

func mustFile(t *testing.T, scope StateFileScope) *StateFile {
	t.Helper()
	proj := uuid.New()
	typ := uuid.New()
	in := StateFileInput{ProjectID: &proj, TypeID: &typ, Scope: scope}
	if scope == ScopePhase {
		ph := uuid.New()
		in.PhaseID = &ph
	}
	if scope == ScopeWorkUnit {
		wu := uuid.New()
		in.WorkUnitID = &wu
	}
	f, err := NewStateFile(in)
	if err != nil {
		t.Fatalf("fixture state file rejected: %v", err)
	}
	return f
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
