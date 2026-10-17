package prompt

import (
	"fmt"

	"github.com/google/uuid"
)

// PromptComponentType names the class of a prompt component (WBS 8.8).
// The WBS list is illustrative ("might include"); the accepted set is
// pinned exactly to the nine types below (P8-06 NOTES).
type PromptComponentType string

// The known component type set (P8-06 NOTES — the accepted WBS 8.8 list).
const (
	ComponentSystem              PromptComponentType = "SYSTEM"
	ComponentRoleInstruction     PromptComponentType = "ROLE_INSTRUCTION"
	ComponentStateInstruction    PromptComponentType = "STATE_INSTRUCTION"
	ComponentProjectContext      PromptComponentType = "PROJECT_CONTEXT"
	ComponentPhaseContext        PromptComponentType = "PHASE_CONTEXT"
	ComponentTaskContext         PromptComponentType = "TASK_CONTEXT"
	ComponentTestContext         PromptComponentType = "TEST_CONTEXT"
	ComponentContinuationContext PromptComponentType = "CONTINUATION_CONTEXT"
	ComponentResponseProtocol    PromptComponentType = "RESPONSE_PROTOCOL"
)

var knownComponentTypes = map[PromptComponentType]struct{}{
	ComponentSystem:              {},
	ComponentRoleInstruction:     {},
	ComponentStateInstruction:    {},
	ComponentProjectContext:      {},
	ComponentPhaseContext:        {},
	ComponentTaskContext:         {},
	ComponentTestContext:         {},
	ComponentContinuationContext: {},
	ComponentResponseProtocol:    {},
}

// Known reports whether the type is part of the accepted set (T8.8.5
// construction check).
func (t PromptComponentType) Known() bool {
	_, ok := knownComponentTypes[t]
	return ok
}

// PromptSource names where a component's content originated (WBS 8.8
// "Source" field) so provenance survives composition and later
// compression.
type PromptSource string

const (
	SourceRoleSystemPrompt   PromptSource = "ROLE_SYSTEM_PROMPT"
	SourceRoleInstruction    PromptSource = "ROLE_INSTRUCTION"
	SourceStateInstruction   PromptSource = "STATE_INSTRUCTION"
	SourceStateFile          PromptSource = "STATE_FILE"
	SourceWorkUnit           PromptSource = "WORK_UNIT"
	SourceContinuation       PromptSource = "CONTINUATION_STATUS"
	SourceResponseProtocol   PromptSource = "RESPONSE_PROTOCOL"
	SourceDependencyContract PromptSource = "DEPENDENCY_CONTRACT"
	SourceSystemDefault      PromptSource = "SYSTEM_DEFAULT"
)

// CompressionPolicy states whether a later compression pass (Phase 9)
// may summarize the component's content (WBS 8.30).
type CompressionPolicy string

const (
	PolicyPreserveExact     CompressionPolicy = "PRESERVE_EXACT"
	PolicyPreserveSemantics CompressionPolicy = "PRESERVE_SEMANTICS"
	PolicyCompressible      CompressionPolicy = "COMPRESSIBLE"
	PolicyOptional          CompressionPolicy = "OPTIONAL"
)

var knownCompressionPolicies = map[CompressionPolicy]struct{}{
	PolicyPreserveExact:     {},
	PolicyPreserveSemantics: {},
	PolicyCompressible:      {},
	PolicyOptional:          {},
}

// Known reports whether the policy is one of the WBS 8.30 values.
func (p CompressionPolicy) Known() bool {
	_, ok := knownCompressionPolicies[p]
	return ok
}

// DefaultPolicy applies the WBS 8.30 example mapping: identity-bearing
// content (work-unit context with acceptance criteria and unit
// identifiers, required tests) defaults to PRESERVE_EXACT; narrative
// context defaults to COMPRESSIBLE; control flow and protocol
// components default to PRESERVE_SEMANTICS (T8.30.2/T8.30.3/T8.30.4).
func DefaultPolicy(typ PromptComponentType) CompressionPolicy {
	switch typ {
	case ComponentTaskContext, ComponentTestContext:
		return PolicyPreserveExact
	case ComponentProjectContext, ComponentPhaseContext:
		return PolicyCompressible
	default:
		return PolicyPreserveSemantics
	}
}

// PromptComponent is one typed piece of prompt content that retains
// its provenance (source type/id/version/hash) and ordering (WBS 8.8).
type PromptComponent struct {
	Type    PromptComponentType
	Source  PromptSource
	Order   int
	Content string

	SourceID      uuid.UUID
	SourceVersion int
	SourceHash    string

	// Policy carries the compression marker (WBS 8.30). A component
	// built via NewPromptComponent defaults to DefaultPolicy for its
	// type (identity-bearing classes PRESERVE_EXACT); it is retained
	// through copies and Components.ByOrder (T8.30.5).
	Policy CompressionPolicy
}

// NewPromptComponent validates and builds a component. An unknown type
// is rejected (T8.8.5); all other fields are retained verbatim (T8.8.1,
// T8.8.2, T8.8.3, T8.8.4).
func NewPromptComponent(t PromptComponentType, source PromptSource, order int, content string, sourceID uuid.UUID, sourceVersion int, sourceHash string) (PromptComponent, error) {
	if !t.Known() {
		return PromptComponent{}, fmt.Errorf("prompt: unknown component type %q", string(t))
	}
	return PromptComponent{
		Type:          t,
		Source:        source,
		Order:         order,
		Content:       content,
		SourceID:      sourceID,
		SourceVersion: sourceVersion,
		SourceHash:    sourceHash,
		Policy:        DefaultPolicy(t),
	}, nil
}

// SetPolicy replaces the component's compression policy after
// construction; an unknown policy is rejected (T8.30.1) and leaves the
// component unchanged.
func (c *PromptComponent) SetPolicy(p CompressionPolicy) error {
	if !p.Known() {
		return fmt.Errorf("prompt: unknown compression policy %q", string(p))
	}
	c.Policy = p
	return nil
}

// Components is an ordered collection of prompt components; it preserves
// insertion/declared order (T8.8.4 "Ordering retained") and carries no
// re-sorting side effects.
type Components []PromptComponent

// ByOrder returns a copy sorted by the declared Order field, breaking
// ties by original slice position (stable), so repeated calls over the
// same input are byte-identical.
func (c Components) ByOrder() Components {
	out := make(Components, len(c))
	copy(out, c)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Order > out[j].Order; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
