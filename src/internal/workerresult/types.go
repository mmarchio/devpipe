package workerresult

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// WorkerResult is the normalized worker report (WBS 13.4) — input to later
// orchestration, never authoritative workflow state.
type WorkerResult struct {
	ProtocolVersion ProtocolVersion

	Status WorkerResultStatus

	Summary string

	Completed []string
	Remaining []string
	Blocking  []string

	NextAction string

	// Optional v1 claims (WBS 13.25-13.28, 13.53).
	Tests            *WorkerTestClaim
	Artifacts        []ArtifactClaim
	AcceptanceClaims []AcceptanceClaim

	// Plan is a planner's plan candidate (P16-80): the compact JSON of the
	// optional `plan` object, nil when absent. It is a report, never a
	// plan — the engine validates it before anything is activated.
	Plan []byte

	Metadata   WorkerResultMetadata
	Provenance ResultParseProvenance

	// NormalizedHash is SHA-256 of the canonical serialization (WBS 13.42).
	NormalizedHash string
}

// WorkerResultMetadata is overseer-derived provenance (WBS 13.5); the
// worker can never supply it.
type WorkerResultMetadata struct {
	DispatchID uuid.UUID
	RequestID  uuid.UUID
	SessionID  uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	StateKey string
	RoleKey  string

	RawResponseHash string

	ParsedAt time.Time
}

// WorkerTestClaim is a worker-reported test claim (WBS 13.26): not run ≠
// failed, so the pass result is optional. Never a deterministic test pass.
type WorkerTestClaim struct {
	ReportedRun    bool
	ReportedPassed *bool
	Summary        string
}

// ArtifactAction is a claimed repository change (WBS 13.28).
type ArtifactAction string

// Artifact actions.
const (
	ArtifactCreated  ArtifactAction = "created"
	ArtifactModified ArtifactAction = "modified"
	ArtifactDeleted  ArtifactAction = "deleted"
	ArtifactRenamed  ArtifactAction = "renamed"
)

// ParseArtifactAction accepts one of the four actions.
func ParseArtifactAction(s string) (ArtifactAction, error) {
	switch a := ArtifactAction(s); a {
	case ArtifactCreated, ArtifactModified, ArtifactDeleted, ArtifactRenamed:
		return a, nil
	}
	return "", fmt.Errorf("workerresult: invalid artifact action %q", s)
}

// ArtifactClaim is a worker-reported artifact change — a claim, not a
// filesystem observation (WBS 13.27).
type ArtifactClaim struct {
	Path         string
	Action       ArtifactAction
	PreviousPath string
}

// AcceptanceClaimStatus is a worker-reported criterion status (WBS 13.54);
// there is deliberately no authoritative "passed".
type AcceptanceClaimStatus string

// Acceptance claim statuses.
const (
	ClaimSatisfied   AcceptanceClaimStatus = "satisfied"
	ClaimUnsatisfied AcceptanceClaimStatus = "unsatisfied"
	ClaimNotChecked  AcceptanceClaimStatus = "not_checked"
)

// ParseAcceptanceClaimStatus accepts one of the three statuses.
func ParseAcceptanceClaimStatus(s string) (AcceptanceClaimStatus, error) {
	switch c := AcceptanceClaimStatus(s); c {
	case ClaimSatisfied, ClaimUnsatisfied, ClaimNotChecked:
		return c, nil
	}
	return "", fmt.Errorf("workerresult: invalid acceptance claim status %q", s)
}

// AcceptanceClaim is a worker claim about an acceptance criterion (WBS
// 13.53). Recognized is overseer-derived: whether the id belongs to the
// dispatched work unit. A claim never verifies a criterion.
type AcceptanceClaim struct {
	ID         string
	Status     AcceptanceClaimStatus
	Evidence   string
	Recognized bool
}

// ExtractionKind records how the protocol object was located.
type ExtractionKind string

// Extraction kinds.
const (
	ExtractRaw    ExtractionKind = "RAW_JSON"
	ExtractFenced ExtractionKind = "FENCED_JSON"
)

// ResultParseProvenance records how a result was obtained (WBS 13.35).
// SourceHash always identifies the original raw response.
type ResultParseProvenance struct {
	Mode       ParseMode
	RepairMode RepairMode
	Extraction ExtractionKind

	Repaired     bool
	RepairMethod *string

	SourceHash string
	ParsedHash string

	ParserVersion string
}
