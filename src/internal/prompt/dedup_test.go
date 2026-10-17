package prompt

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// P8-35 (WBS 8.33, T8.33.x): prompt deduplication unit tests.
//
// Policy (stated here + ACTIVE_TASK NOTES so TESTER/PM can verify):
//   - DEDUP KEY = the (Hash, Content) PAIR, not the hash alone. T8.33.3's
//     "same hash claimed over different content" half requires that a record
//     carrying an identical Hash but a DIFFERENT Content is never deduplicated
//     against the candidate, so content must match byte-for-byte as well.
//   - ARTIFACT vs INVOCATION split: the artifact is the persisted canonical
//     content identity (its Hash + Content); the execution association
//     (project/phase/work-unit) lives on the INVOCATION and is carried
//     verbatim from the candidate. On reuse the invocation points at the
//     existing artifact's ID while retaining the CANDIDATE's own association —
//     it never inherits the existing artifact's association.
//   - Determinism: repeated deduplication over the same input is byte-identical
//     (the pure-domain determinism contract).
//   - Projection: the decision reads the candidate + persisted artifacts and
//     never mutates them (mutating the input after the decision leaves the
//     decision's copied association intact).

var (
	p835ProjectA = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	p835ProjectB = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	p835PhaseA   = uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	p835PhaseB   = uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	p835UnitA    = uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	p835UnitB    = uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
)

// p835Artifact builds a persisted canonical-prompt artifact record carrying
// an explicit association (the existing artifact's own project).
func p835Artifact(id uuid.UUID, project uuid.UUID, hash, content string) CanonicalPrompt {
	return CanonicalPrompt{
		ID:        id,
		ProjectID: project,
		Content:   content,
		Hash:      hash,
	}
}

// T8.33.1 — Identical hash (with identical content) is DETECTABLE: a candidate
// whose (Hash, Content) pair matches an existing artifact reuses that artifact
// (Reuse=true) and points at the existing artifact's ID, not its own.
func TestT8331_IdenticalHashDetectable(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "identical content")}
	candidate := CanonicalPrompt{ID: uuid.New(), ProjectID: p835ProjectA, Content: "identical content", Hash: "hash-X"}

	got := DedupeCanonicalPrompt(candidate, existing)
	if !got.Reuse {
		t.Fatalf("identical (hash, content) not detected: %+v", got)
	}
	if got.ArtifactID != existingID {
		t.Fatalf("reuse points at %s, want the existing artifact %s", got.ArtifactID, existingID)
	}
}

func p835ArtifactID() uuid.UUID {
	return uuid.MustParse("12121212-1212-4212-8212-121212121212")
}

// T8.33.2 — Reuse does NOT lose execution association: the invocation retains
// the CANDIDATE's project/phase/work-unit association, not the existing
// artifact's. The fixture gives the candidate and the existing artifact
// DIFFERENT associations so inheritance would be observable.
func TestT8332_ReuseRetainsCandidateAssociation(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "same content")}

	candidate := CanonicalPrompt{
		ID:         uuid.New(),
		ProjectID:  p835ProjectA,
		PhaseID:    ptr(p835PhaseA),
		WorkUnitID: ptr(p835UnitA),
		Content:    "same content",
		Hash:       "hash-X",
	}

	got := DedupeCanonicalPrompt(candidate, existing)
	if !got.Reuse {
		t.Fatalf("expected reuse: %+v", got)
	}
	if got.Association.ProjectID != p835ProjectA {
		t.Fatalf("project = %s, want the candidate's %s (not the existing artifact's)", got.Association.ProjectID, p835ProjectA)
	}
	if got.Association.PhaseID == nil || *got.Association.PhaseID != p835PhaseA {
		t.Fatalf("phase = %v, want the candidate's %s", got.Association.PhaseID, p835PhaseA)
	}
	if got.Association.WorkUnitID == nil || *got.Association.WorkUnitID != p835UnitA {
		t.Fatalf("work unit = %v, want the candidate's %s", got.Association.WorkUnitID, p835UnitA)
	}
}

// T8.33.3a — Different content (different hash) is never deduplicated: a
// candidate whose hash differs from every persisted artifact creates new.
func TestT8333a_DifferentHashNotDeduplicated(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "content one")}
	candidate := CanonicalPrompt{ID: uuid.New(), ProjectID: p835ProjectB, Content: "content two", Hash: "hash-Y"}

	got := DedupeCanonicalPrompt(candidate, existing)
	if got.Reuse {
		t.Fatalf("different hash was deduplicated: %+v", got)
	}
	if got.ArtifactID != candidate.ID {
		t.Fatalf("create outcome must carry the candidate's own ID, got %s want %s", got.ArtifactID, candidate.ID)
	}
}

// T8.33.3b — Same hash claimed over DIFFERENT content is never deduplicated:
// the dedup key is the (hash, content) pair, so an identical hash with a
// differing content is treated as novel (create), never a reuse.
func TestT8333b_SameHashDifferentContentNotDeduplicated(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "content one")}
	candidate := CanonicalPrompt{ID: uuid.New(), ProjectID: p835ProjectB, Content: "content two", Hash: "hash-X"}

	got := DedupeCanonicalPrompt(candidate, existing)
	if got.Reuse {
		t.Fatalf("same hash over different content was deduplicated: %+v", got)
	}
	if got.ArtifactID != candidate.ID {
		t.Fatalf("create outcome must carry the candidate's own ID, got %s want %s", got.ArtifactID, candidate.ID)
	}
}

// Determinism pin (pure-domain contract): repeated deduplication over the same
// input is byte-identical (the decision copies the association, so it must be
// stable across calls).
func TestT8334_RepeatedDedupByteIdentical(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "content")}
	mk := func() CanonicalPrompt {
		return CanonicalPrompt{ID: uuid.MustParse("99999999-9999-4999-8999-999999999999"), ProjectID: p835ProjectA, PhaseID: ptr(p835PhaseA), WorkUnitID: ptr(p835UnitA), Content: "content", Hash: "hash-X"}
	}
	first := DedupeCanonicalPrompt(mk(), existing)
	for i := 0; i < 50; i++ {
		if !reflect.DeepEqual(DedupeCanonicalPrompt(mk(), existing), first) {
			t.Fatalf("repeat %d: decision not byte-identical", i)
		}
	}
}

// Projection / no-mutation pin: the decision COPIES the candidate's
// association. Mutating the candidate's association AFTER the decision must
// not affect the already-computed decision (the association is a copy, not a
// reference into the candidate).
func TestT8335_DecisionCopiesAssociation(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "content")}
	candidate := CanonicalPrompt{ID: uuid.New(), ProjectID: p835ProjectA, PhaseID: ptr(p835PhaseA), Content: "content", Hash: "hash-X"}
	got := DedupeCanonicalPrompt(candidate, existing)

	candidate.ProjectID = p835ProjectB
	candidate.PhaseID = ptr(p835PhaseB)
	if got.Association.ProjectID != p835ProjectA || got.Association.PhaseID == nil || *got.Association.PhaseID != p835PhaseA {
		t.Fatalf("decision association mutated with its input: %+v", got.Association)
	}
}

// The dedup decision is a pure projection over persisted artifacts: it never
// mutates them. Mutating the EXISTING set after the decision leaves the
// decision intact (reuse still points at the original artifact ID).
func TestT8336_DecisionDoesNotMutateArtifacts(t *testing.T) {
	existingID := p835ArtifactID()
	existing := []CanonicalPrompt{p835Artifact(existingID, p835ProjectB, "hash-X", "content")}
	candidate := CanonicalPrompt{ID: uuid.New(), ProjectID: p835ProjectA, Content: "content", Hash: "hash-X"}
	got := DedupeCanonicalPrompt(candidate, existing)
	if !got.Reuse || got.ArtifactID != existingID {
		t.Fatalf("expected reuse of existing artifact: %+v", got)
	}
	existing[0].Content = "mutated content"
	existing[0].Hash = "mutated hash"
	if got.ArtifactID != existingID {
		t.Fatalf("decision mutated by input mutation: %+v", got)
	}
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }
