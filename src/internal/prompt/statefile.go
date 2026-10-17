package prompt

import (
	"errors"
	"fmt"
	"sort"

	"overseer/internal/role"
	"overseer/internal/statefile"
)

// ErrMissingStateFileVersion reports a composition failure: the snapshot
// pins a REQUIRED state file with no usable captured version (WBS 8.13,
// T8.13.5 — the P8-19 choice, mirroring the P8-16/P8-17
// missing-captured-version failure contract). An OPTIONAL file with no
// captured version composes to no component instead (T8.13.5).
var ErrMissingStateFileVersion = errors.New("prompt: snapshot carries a required state file with no captured version")

// ComposeStateFiles renders a Phase 7 state-file snapshot's pinned files
// as prompt components grouped by semantic classification (WBS 8.13).
//
// Each pin with a captured version becomes exactly one STATE_FILE
// component in its classification's WBS 8.13 section — Project / Phase /
// Task / Test context — at the snapshot-captured version, with the
// snapshot's source id/version/hash retained verbatim (no "latest"
// input: the pins are the immutable Phase 7 read model, so a later file
// edit has no input to the composition).
//
// The output is a pure function of the pin SET: the database retrieval
// order (the snapshot file-list order) cannot alter the prompt
// (T8.13.4). Sections appear in the fixed WBS 8.13 order (Project,
// Phase, Task, Test — mirroring the WBS 8.9 canonical ranks 4 < 5 < 7 <
// 8), and components within a section are ordered deterministically by
// (type key, file id) — no implicit concatenation in retrieval order
// (the WBS forbids it explicitly).
//
// Missing-captured-content policy (stated per ACTIVE_TASK NOTES): an
// optional file (its type key not in required) with a cleared captured
// version composes to no component and no section body — no failure, no
// placeholder, mirroring the P8-18 zero-component choice; a REQUIRED
// file with a cleared captured version is a composition failure
// (ErrMissingStateFileVersion).
func ComposeStateFiles(files []statefile.StateFileSnapshot, required []string) (Components, error) {
	requiredSet := make(map[string]bool, len(required))
	for _, key := range required {
		requiredSet[key] = true
	}
	out := make(Components, 0, len(files))
	for _, f := range files {
		if f.Version <= 0 || f.Content == "" {
			if requiredSet[f.TypeKey] {
				return nil, fmt.Errorf("state file %s: %w", f.TypeKey, ErrMissingStateFileVersion)
			}
			continue
		}
		c, err := NewPromptComponent(
			classificationComponentType(f.Classification),
			SourceStateFile,
			0,
			f.Content,
			f.StateFileID,
			f.Version,
			f.Hash,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sortStateFileComponents(out, files)
	return out, nil
}

// classificationComponentType maps the WBS 8.13 section classifications
// to their component types (PROJECT_CONTEXT -> PROJECT_CONTEXT, etc.).
func classificationComponentType(c role.Classification) PromptComponentType {
	switch c {
	case role.ClassificationProjectContext:
		return ComponentProjectContext
	case role.ClassificationPhaseContext:
		return ComponentPhaseContext
	case role.ClassificationTaskContext:
		return ComponentTaskContext
	case role.ClassificationTestContext:
		return ComponentTestContext
	}
	return PromptComponentType(c)
}

// stateFileSectionRank is the WBS 8.13 section order (Project, Phase,
// Task, Test — mirroring the WBS 8.9 canonical ranks 4 < 5 < 7 < 8).
var stateFileSectionRank = map[PromptComponentType]int{
	ComponentProjectContext: 4,
	ComponentPhaseContext:   5,
	ComponentTaskContext:    7,
	ComponentTestContext:    8,
}

// typeKeyOf returns the pin's type key for a component, by source id.
func typeKeyOf(files []statefile.StateFileSnapshot, c PromptComponent) string {
	for _, f := range files {
		if f.StateFileID == c.SourceID {
			return f.TypeKey
		}
	}
	return c.SourceID.String()
}

// sortStateFileComponents orders the composed components
// deterministically: WBS 8.13 section rank first, then the pin's type
// key, then the file id — so any retrieval order of the same pin set
// yields a byte-identical output (T8.13.4).
func sortStateFileComponents(out Components, files []statefile.StateFileSnapshot) {
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := stateFileSectionRank[out[i].Type], stateFileSectionRank[out[j].Type]
		if ri != rj {
			return ri < rj
		}
		if ki, kj := typeKeyOf(files, out[i]), typeKeyOf(files, out[j]); ki != kj {
			return ki < kj
		}
		return out[i].SourceID.String() < out[j].SourceID.String()
	})
}
