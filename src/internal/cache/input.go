package cache

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// PromptCacheInput is the explicit semantic cache input (WBS 10.2). Every
// field is a hash or pinned identity of something that affects prompt
// semantics; display values (names, descriptions) and timestamps are not
// representable. Phase 10 adds the inputs the canonical composer reads live
// (not pinned by the context snapshot): the bound work unit's
// specification, the state instruction, the phase WBS and the planning
// configuration (PLAN Decision 3).
type PromptCacheInput struct {
	ProjectID uuid.UUID

	ProjectConfigHash string

	ContextSnapshotHash string

	StateMachineID      uuid.UUID
	StateMachineVersion int

	StateKey string

	RoleHash string

	TemplateHash string

	ResponseProtocolHash string

	CompressionConfigHash string

	ContinuationStatusHash *string

	// Optional semantic inputs (empty = not applicable to this prompt).
	WorkUnitSpecHash     string
	StateInstructionHash string
	PhaseWBSHash         string
	PlanningConfigHash   string

	// PreparationPolicy names the preparation policy variant (e.g. the
	// same-session / reset-session continuation mode) — empty by default.
	PreparationPolicy string
}

// InputError lists every invalid / missing field of a cache input.
type InputError struct {
	Fields []string
}

func (e *InputError) Error() string {
	return "cache: invalid cache input: " + strings.Join(e.Fields, ", ")
}

// NewPromptCacheInput validates the input (T10.2.1-T10.2.3): identity
// fields and the required hashes must be present; the continuation hash is
// optional but, when present, non-empty. All failures are reported at once.
func NewPromptCacheInput(in PromptCacheInput) (PromptCacheInput, error) {
	var bad []string
	if in.ProjectID == uuid.Nil {
		bad = append(bad, "project_id")
	}
	if in.StateMachineID == uuid.Nil {
		bad = append(bad, "state_machine_id")
	}
	if in.StateMachineVersion <= 0 {
		bad = append(bad, "state_machine_version")
	}
	if strings.TrimSpace(in.StateKey) == "" {
		bad = append(bad, "state_key")
	}
	for name, v := range map[string]string{
		"project_config_hash":     in.ProjectConfigHash,
		"context_snapshot_hash":   in.ContextSnapshotHash,
		"role_hash":               in.RoleHash,
		"template_hash":           in.TemplateHash,
		"response_protocol_hash":  in.ResponseProtocolHash,
		"compression_config_hash": in.CompressionConfigHash,
	} {
		if strings.TrimSpace(v) == "" {
			bad = append(bad, name)
		}
	}
	if in.ContinuationStatusHash != nil && strings.TrimSpace(*in.ContinuationStatusHash) == "" {
		bad = append(bad, "continuation_status_hash")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return PromptCacheInput{}, &InputError{Fields: bad}
	}
	out := in
	if in.ContinuationStatusHash != nil {
		v := *in.ContinuationStatusHash
		out.ContinuationStatusHash = &v
	}
	return out, nil
}

func (in PromptCacheInput) String() string {
	return fmt.Sprintf("cache-input(project=%s state=%s@%d)", in.ProjectID, in.StateKey, in.StateMachineVersion)
}
