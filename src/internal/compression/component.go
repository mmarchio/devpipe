package compression

import (
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/prompt"
)

// CompressionComponent is one canonical prompt component translated into its
// compression-aware form (WBS 9.4): the component type, exact content, the
// compression policy carried from the canonical prompt, and the source
// provenance (id/version/hash). Ordinal is the component's position in the
// canonical (ByOrder) sequence, so the component order is deterministic and
// survives framing, validation, and persistence.
type CompressionComponent struct {
	Ordinal int

	Type   prompt.PromptComponentType
	Source prompt.PromptSource

	Content string

	Policy prompt.CompressionPolicy

	SourceID      uuid.UUID
	SourceVersion int
	SourceHash    string
}

// FromPromptComponents translates Phase 8 prompt components into compression
// components (WBS 9.4). The input is read in its declared order (ByOrder —
// stable, so repeated conversion is deep-equal); the policy and source
// metadata are copied verbatim (T9.4.1/T9.4.2); an unknown policy or
// component type is rejected (T9.4.3). The input is never mutated.
func FromPromptComponents(in prompt.Components) ([]CompressionComponent, error) {
	ordered := in.ByOrder()
	out := make([]CompressionComponent, 0, len(ordered))
	for i, c := range ordered {
		if !c.Type.Known() {
			return nil, fmt.Errorf("compression: component %d: unknown component type %q", i, string(c.Type))
		}
		if !c.Policy.Known() {
			return nil, fmt.Errorf("compression: component %d (%s): unknown compression policy %q", i, c.Type, string(c.Policy))
		}
		out = append(out, CompressionComponent{
			Ordinal:       i,
			Type:          c.Type,
			Source:        c.Source,
			Content:       c.Content,
			Policy:        c.Policy,
			SourceID:      c.SourceID,
			SourceVersion: c.SourceVersion,
			SourceHash:    c.SourceHash,
		})
	}
	return out, nil
}

// ValidateComponents checks a component set loaded from storage: every
// policy and type must be known and ordinals must be strictly increasing
// (the deterministic canonical order, T9.4.4).
func ValidateComponents(comps []CompressionComponent) error {
	for i, c := range comps {
		if !c.Type.Known() {
			return fmt.Errorf("compression: component %d: unknown component type %q", c.Ordinal, string(c.Type))
		}
		if !c.Policy.Known() {
			return fmt.Errorf("compression: component %d: unknown compression policy %q", c.Ordinal, string(c.Policy))
		}
		if i > 0 && comps[i-1].Ordinal >= c.Ordinal {
			return fmt.Errorf("compression: component ordinals not strictly increasing at %d", c.Ordinal)
		}
	}
	return nil
}

// copyComponents returns an independent copy of the slice.
func copyComponents(in []CompressionComponent) []CompressionComponent {
	out := make([]CompressionComponent, len(in))
	copy(out, in)
	return out
}
