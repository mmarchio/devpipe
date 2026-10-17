package prompt

import (
	"sort"
	"strings"

	"github.com/google/uuid"
)

// DependencyContract is the stored contract (summary) of a dependency, as
// captured at the snapshot-pinned state (WBS 8.15): the dependency's name,
// purpose, input, output, and guarantees. The Source field carries the
// dependency's full source where it is known; the composition renders the
// summary only and never implicitly includes the source (T8.15.5).
type DependencyContract struct {
	Name       string
	Purpose    string
	Input      string
	Output     string
	Guarantees []string
	Source     string
}

// ComposeDependencyContracts renders the stored dependency contracts of the
// current work unit's dependencies (WBS 8.15): each contract whose
// dependency name is in required (the unit's captured dependency set)
// becomes exactly one TASK_CONTEXT component with a DEPENDENCY_CONTRACT
// source, carrying the contract summary (name/purpose/input/output/
// guarantees) only — never the dependency's full source (T8.15.5).
//
// Inclusion: contracts not in the required set are excluded (unrelated
// dependencies, T8.15.2). A required dependency with no stored contract is
// a missing optional — it composes to no component and no failure, no
// invented content (T8.15.4, the P8-19/P8-20 skip-and-continue choice).
//
// Determinism: the included contracts are rendered in sorted-by-name order,
// so any input order of the same required set + contract set yields
// byte-identical output (T8.15.3). The composition consumes ONLY the
// caller-supplied captured values — there is no "latest" input.
//
// Provenance: no dependency-contract persistence exists yet, so no stable
// dependency id/version/hash is captured; the component rides its
// DEPENDENCY_CONTRACT source type and the dependency name rendered in the
// content.
func ComposeDependencyContracts(required []string, contracts []DependencyContract) (Components, error) {
	reqSet := make(map[string]bool, len(required))
	for _, name := range required {
		reqSet[name] = true
	}
	selected := make([]DependencyContract, 0, len(contracts))
	for _, c := range contracts {
		if reqSet[c.Name] {
			selected = append(selected, c)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	out := make(Components, 0, len(selected))
	for _, c := range selected {
		pc, err := NewPromptComponent(
			ComponentTaskContext,
			SourceDependencyContract,
			0,
			renderDependencyContract(c),
			uuid.Nil,
			0,
			"",
		)
		if err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

// renderDependencyContract renders one contract's summary in the fixed
// WBS 8.15 field order (name/purpose/input/output/guarantees). The Source
// field is never rendered (T8.15.5); an empty guarantees list renders the
// "(none)" placeholder (no failure, no invented content).
func renderDependencyContract(c DependencyContract) string {
	var b strings.Builder
	b.WriteString("Dependency: " + c.Name + "\n")
	b.WriteString("Purpose: " + c.Purpose + "\n")
	b.WriteString("Input: " + c.Input + "\n")
	b.WriteString("Output: " + c.Output + "\n")
	b.WriteString("Guarantees:")
	if len(c.Guarantees) == 0 {
		b.WriteString(" (none)")
	} else {
		for _, g := range c.Guarantees {
			b.WriteString("\n- " + g)
		}
	}
	return b.String()
}
