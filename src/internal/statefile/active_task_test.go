package statefile

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/wbs"
)

// P7-10 (WBS 7.11 T7.11.1-T7.11.6 pure half): the ACTIVE_TASK content
// renderer is a PURE function of the current work unit — deterministic, and
// carrying the unit id, the objective, and the acceptance criteria /
// required tests / constraints sections. The derivation service (P7-10)
// writes this content through the versioned content path, so the renderer
// is the single source of the materialized shape.

func p710RenderUnit() *wbs.WorkUnit {
	u, _ := wbs.NewWorkUnit(uuid.New(), 1, "Compose registration workflow", wbs.WorkUnitKindATOMIC)
	u.Specification = &wbs.WorkUnitSpecification{
		Objective:          "Compose registration workflow.",
		AcceptanceCriteria: []string{"criteria one", "criteria two"},
		RequiredTests:      []string{"T7.11.1", "T7.11.2"},
		Constraints:        []string{"stay in scope"},
	}
	return u
}

// T7.11.1/T7.11.2/T7.11.3 — the generated content carries the unit id and
// the objective, and is deterministic (byte-identical across calls).
func TestP710RenderActiveTaskDeterministicAndComplete(t *testing.T) {
	u := p710RenderUnit()
	a := RenderActiveTask(u)
	b := RenderActiveTask(u)
	if a != b {
		t.Fatalf("renderer not deterministic:\n%s\n---\n%s", a, b)
	}
	if !strings.Contains(a, u.ID.String()) {
		t.Fatalf("rendered content missing unit id:\n%s", a)
	}
	if !strings.Contains(a, "Compose registration workflow.") {
		t.Fatalf("rendered content missing objective:\n%s", a)
	}
	// T7.11.4-T7.11.6 — the structured sections are present.
	for _, want := range []string{
		"# Active Task",
		"## Work Unit",
		"## Objective",
		"## Acceptance Criteria",
		"## Required Tests",
		"## Constraints",
		"- criteria one",
		"- criteria two",
		"- T7.11.1",
		"- T7.11.2",
		"- stay in scope",
	} {
		if !strings.Contains(a, want) {
			t.Fatalf("rendered content missing %q:\n%s", want, a)
		}
	}
}

// A unit whose specification lists are empty omits the empty sections
// (the shape is a function of the unit's data only).
func TestP710RenderActiveTaskOmitsEmptySections(t *testing.T) {
	u, _ := wbs.NewWorkUnit(uuid.New(), 1, "name", wbs.WorkUnitKindATOMIC)
	u.Specification = &wbs.WorkUnitSpecification{Objective: "do the thing"}
	got := RenderActiveTask(u)
	if !strings.Contains(got, "do the thing") {
		t.Fatalf("rendered content missing objective:\n%s", got)
	}
	for _, absent := range []string{"## Acceptance Criteria", "## Required Tests", "## Constraints"} {
		if strings.Contains(got, absent) {
			t.Fatalf("rendered content carries empty section %q:\n%s", absent, got)
		}
	}
}

// A nil unit renders the empty content (no unit -> nothing to derive).
func TestP710RenderActiveTaskNilUnit(t *testing.T) {
	if got := RenderActiveTask(nil); got != "" {
		t.Fatalf("nil unit rendered %q, want empty", got)
	}
}

// A distinct unit yields distinct content (the sync op's version append is
// what makes the change visible; the renderer's job is the content delta).
func TestP710RenderActiveTaskDiffersPerUnit(t *testing.T) {
	a := p710RenderUnit()
	b := p710RenderUnit()
	if RenderActiveTask(a) == RenderActiveTask(b) {
		t.Fatal("two distinct units rendered identical content")
	}
}
