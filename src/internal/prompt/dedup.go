package prompt

import (
	"github.com/google/uuid"
)

// Deduplication (WBS 8.33): the prompt ARTIFACT is separated from the prompt
// INVOCATION so identical canonical content is never duplicated. The artifact
// is the persisted content identity — its (Hash, Content) pair. The invocation
// is a reference that points at an artifact while retaining its OWN execution
// association (project/phase/work-unit); the association lives on the
// invocation, never on the artifact.

// DedupAssociation is the execution association a prompt invocation carries.
// It is copied verbatim from the candidate prompt: reuse points the invocation
// at an existing artifact WITHOUT inheriting that artifact's association
// (T8.33.2 — reuse does not lose the candidate's execution association).
type DedupAssociation struct {
	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
}

// DedupDecision is the outcome of a deduplication over a candidate prompt and
// the set of already-persisted artifacts.
//
//   - Reuse is true when an existing artifact has the SAME dedup key
//     (identical Hash AND byte-identical Content); ArtifactID then identifies
//     the persisted artifact to reference.
//   - Reuse is false when no existing artifact matches (novel content);
//     ArtifactID carries the candidate's own ID (the artifact to create).
//
// Association always carries the CANDIDATE's execution association, copied
// (not referenced) so a later mutation of the candidate cannot affect the
// decision. The decision is a pure projection: it never mutates the candidate
// or the persisted artifacts.
type DedupDecision struct {
	Reuse       bool
	ArtifactID  uuid.UUID
	Association DedupAssociation
}

// DedupeCanonicalPrompt decides, for a candidate canonical prompt, whether an
// identical artifact already exists among the persisted artifacts (WBS 8.33).
//
// The dedup key is the (Hash, Content) PAIR, not the hash alone: T8.33.3's
// "same hash claimed over different content" half requires that an identical
// Hash over a DIFFERENT Content is treated as novel, never deduplicated. The
// hash is checked first (a cheap pre-filter), then the content is compared
// byte-for-byte — only a pair with both equal is a reuse. On reuse the
// invocation points at the first matching artifact's ID; on novelty it carries
// the candidate's own ID. The candidate's execution association is copied into
// the decision (T8.33.2). Repeated decisions over the same input are
// byte-identical (the pure-domain determinism contract), and the decision
// never mutates its inputs (a pure projection).
func DedupeCanonicalPrompt(candidate CanonicalPrompt, artifacts []CanonicalPrompt) DedupDecision {
	assoc := DedupAssociation{
		ProjectID:  candidate.ProjectID,
		PhaseID:    candidate.PhaseID,
		WorkUnitID: candidate.WorkUnitID,
	}
	for _, a := range artifacts {
		if a.Hash == candidate.Hash && a.Content == candidate.Content {
			return DedupDecision{Reuse: true, ArtifactID: a.ID, Association: assoc}
		}
	}
	return DedupDecision{Reuse: false, ArtifactID: candidate.ID, Association: assoc}
}
