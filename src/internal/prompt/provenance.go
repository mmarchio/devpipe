package prompt

import (
	"github.com/google/uuid"
)

// ProvenanceEntry maps one major component of a canonical prompt to its
// source (WBS 8.31): the component's type plus the source ID, version, and
// hash taken VERBATIM from the component's captured provenance fields. The
// WBS example shows byte offsets, but exact offsets are optional — semantic
// component mappings are sufficient (P8-34 NOTES; the goal is traceability).
type ProvenanceEntry struct {
	ComponentType PromptComponentType
	SourceID      uuid.UUID
	SourceVersion int
	SourceHash    string
}

// ProvenanceMap derives the WBS 8.31 provenance map over a composed
// prompt's component set. A "major component" is every component with
// non-empty Content (P8-34 NOTES — an empty placeholder, e.g. the
// control-only state-instruction composition of WBS 8.12, yields no entry);
// entries follow the components' declared order. The derivation is a
// pure projection: it reads only the components' captured provenance
// fields and copies them verbatim (no re-derivation, no content parsing),
// so repeated derivation over the same input is byte-identical and the
// map never mutates the components.
func ProvenanceMap(components Components) []ProvenanceEntry {
	out := make([]ProvenanceEntry, 0, len(components))
	for _, c := range components {
		if c.Content == "" {
			continue
		}
		out = append(out, ProvenanceEntry{
			ComponentType: c.Type,
			SourceID:      c.SourceID,
			SourceVersion: c.SourceVersion,
			SourceHash:    c.SourceHash,
		})
	}
	return out
}
