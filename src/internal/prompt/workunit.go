package prompt

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// WorkUnitContextInput is the captured, snapshot-pinned state of the
// current work unit fed to the composition (WBS 8.14): the caller
// supplies the unit's structured values exactly as captured — the
// composition reads ONLY these values (the P8-16..P8-19 pattern; there
// is no "latest" input). List fields carry their captured order
// verbatim; Dependencies carries the captured dependency set in any
// order (rendered deterministically).
type WorkUnitContextInput struct {
	ID            uuid.UUID
	Name          string
	Kind          string
	Objective     string
	Dependencies  []string
	Acceptance    []string
	RequiredTests []string
	Constraints   []string
}

// WorkUnitContext is the validated input retained for one composition.
type WorkUnitContext = WorkUnitContextInput

// ErrMissingWorkUnitObjective reports a composition failure: the
// captured work unit carries no usable objective (WBS 8.14; the
// P8-16/P8-17 missing-captured-value failure contract — a cleared
// captured REQUIRED value fails the composition).
var ErrMissingWorkUnitObjective = errors.New("prompt: work unit context carries no captured objective")

// NewWorkUnitContext validates a captured work unit: the unit ID is
// required and a non-empty objective is required (a cleared captured
// objective is a composition failure — see ComposeWorkUnit). Lists are
// copied into independent slices at construction so the composition is
// unaffected by later input mutation.
func NewWorkUnitContext(in WorkUnitContextInput) (WorkUnitContext, error) {
	if in.ID == uuid.Nil {
		return WorkUnitContext{}, fmt.Errorf("prompt: work unit context requires a unit ID")
	}
	if strings.TrimSpace(in.Objective) == "" {
		return WorkUnitContext{}, fmt.Errorf("prompt: work unit %s: %w", in.ID, ErrMissingWorkUnitObjective)
	}
	out := WorkUnitContextInput{
		ID:        in.ID,
		Name:      in.Name,
		Kind:      in.Kind,
		Objective: in.Objective,
	}
	out.Dependencies = append([]string(nil), in.Dependencies...)
	out.Acceptance = append([]string(nil), in.Acceptance...)
	out.RequiredTests = append([]string(nil), in.RequiredTests...)
	out.Constraints = append([]string(nil), in.Constraints...)
	return out, nil
}

// ComposeWorkUnit renders the captured current work unit as exactly one
// TASK_CONTEXT component (WBS 8.14): the unit's structured fields are
// rendered in the fixed WBS 8.14 field order, with the unit's ID as the
// component's SourceID provenance. Dependencies are rendered
// deterministically (sorted — any dependency input order yields
// byte-identical output, T8.14.5); acceptance criteria and required
// tests preserve captured input order verbatim (T8.14.3/T8.14.4); a
// cleared list renders its "(none)" placeholder (the T8.12.4 zero-content
// choice — no failure, no invented content). Only the composed unit's
// values enter the render — no unrelated work unit (T8.14.6).
func ComposeWorkUnit(unit WorkUnitContext) (Components, error) {
	if unit.ID == uuid.Nil {
		return nil, fmt.Errorf("prompt: work unit context requires a unit ID")
	}
	if strings.TrimSpace(unit.Objective) == "" {
		return nil, fmt.Errorf("prompt: work unit %s: %w", unit.ID, ErrMissingWorkUnitObjective)
	}
	var b strings.Builder
	b.WriteString("ID: " + unit.ID.String() + "\n")
	b.WriteString("name: " + unit.Name + "\n")
	b.WriteString("kind: " + unit.Kind + "\n")
	b.WriteString("objective: " + unit.Objective + "\n")
	b.WriteString("dependencies: " + renderWorkUnitList(unit.Dependencies, true) + "\n")
	b.WriteString("acceptance criteria: " + renderWorkUnitList(unit.Acceptance, false) + "\n")
	b.WriteString("required tests: " + renderWorkUnitList(unit.RequiredTests, false) + "\n")
	b.WriteString("constraints: " + renderWorkUnitList(unit.Constraints, false) + "\n")
	c, err := NewPromptComponent(
		ComponentTaskContext,
		SourceWorkUnit,
		0,
		b.String(),
		unit.ID,
		0,
		"",
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}

// renderWorkUnitList renders one captured list field: empty renders the
// "(none)" placeholder; deterministic=true sorts (dependencies —
// T8.14.5); otherwise captured order is preserved verbatim.
func renderWorkUnitList(items []string, deterministic bool) string {
	if len(items) == 0 {
		return "(none)"
	}
	if deterministic {
		items = append([]string(nil), items...)
		sort.Strings(items)
	}
	return strings.Join(items, "; ")
}
