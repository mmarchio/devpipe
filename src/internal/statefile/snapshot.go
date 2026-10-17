package statefile

import (
	"time"

	"github.com/google/uuid"

	"overseer/internal/role"
)

// StateFileSnapshot pins one state file at an EXACT immutable version inside an
// execution-context snapshot (WBS 7.30). Whether the content is duplicated or
// later reconstructed from the immutable version reference is an implementation
// choice, provided historical reconstruction is guaranteed. This domain pins
// BOTH the version/hash AND the exact content bytes of that version (the
// content is duplicated in the snapshot); reconstruction is therefore trivially
// guaranteed because the pinned version's immutable bytes are carried verbatim
// and the hash is pinned per file. Classification (WBS 7.36) is the source
// tag of the component, persisted with the pin so it survives
// reconstruction without re-derivation.
type StateFileSnapshot struct {
	StateFileID uuid.UUID

	TypeKey string
	Scope   StateFileScope

	Version int
	Hash    string

	Content string

	Classification role.Classification
}

// ExecutionContextSnapshot is the central Phase 7 artifact: the exact context a
// later prompt will use (WBS 7.29). It captures the single runtime position
// (project/phase/work-unit association, runtime state, state machine + state
// key), the resolved role (reused by value from the Phase 5/6 domain type), and
// the pinned state-file versions. It is a pure domain record: no persistence,
// no service (the creation service is P7-18; the read API is P7-23).
type ExecutionContextSnapshot struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	RuntimeStateID      uuid.UUID
	RuntimeStateVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int

	StateKey string

	RoleSnapshot role.RoleSnapshot

	StateFiles []StateFileSnapshot

	CreatedAt time.Time
}

// SnapshotSize aggregates the snapshot's component token estimates (WBS
// 7.35): the pinned state files' estimates summed, the role snapshot's
// estimate (system prompt + assigned instructions) included where
// available, and their sum. It is preparatory metadata for prompt
// composition/compression, NOT an authoritative tokenization (each
// component estimate is the bytes/4 TokenEstimate heuristic).
type SnapshotSize struct {
	StateFileTokens int
	RoleTokens      int
	EstimatedTotal  int
}

// Size returns the snapshot's aggregate token budget (WBS 7.35). Empty
// optional files (no captured content) estimate 0 and do not disturb the
// aggregate; a snapshot with no role content carries RoleTokens 0.
func (s *ExecutionContextSnapshot) Size() SnapshotSize {
	var files, roleTokens int
	for _, f := range s.StateFiles {
		files += TokenEstimate(f.Content)
	}
	roleTokens += TokenEstimate(s.RoleSnapshot.SystemPrompt.Content)
	for _, in := range s.RoleSnapshot.Instructions {
		roleTokens += TokenEstimate(in.Content)
	}
	return SnapshotSize{StateFileTokens: files, RoleTokens: roleTokens, EstimatedTotal: files + roleTokens}
}
