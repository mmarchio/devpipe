package prompt

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// P8-34 (WBS 8.31, T8.31.x): prompt provenance map unit tests.
//
// Policy (stated here + ACTIVE_TASK NOTES so TESTER/PM can verify):
//   - "Major component" = every component with non-empty Content. Empty
//     components (zero-component placeholders such as the control-only
//     state-instruction composition of WBS 8.12) yield NO entry.
//   - The map is a semantic projection (WBS 8.31: byte offsets optional):
//     each entry carries {component type, source ID, source version,
//     source hash} taken VERBATIM from the component's captured provenance
//     fields — no re-derivation, no recomputation.
//   - Determinism: repeated derivation over the same input is byte-identical
//     (fixed-order entries, copied values).

var (
	p834RoleID   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	p834FileID   = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	p834UnitID   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	p834Protocol = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

// p834Components builds a realistic mixed component set with FIXED ids (so
// identity assertions are stable across calls).
func p834Components() Components {
	sys, _ := NewPromptComponent(ComponentSystem, SourceRoleSystemPrompt, 1,
		"Engineer system prompt v4 body", p834RoleID, 4, "hash-sys-4")
	ri, _ := NewPromptComponent(ComponentRoleInstruction, SourceRoleInstruction, 2,
		"Role instruction DOD body", p834RoleID, 9, "hash-ri-9")
	sf, _ := NewPromptComponent(ComponentTaskContext, SourceStateFile, 3,
		"ACTIVE_TASK v3 body", p834FileID, 3, "hash-file-3")
	// Empty content: a control-only placeholder (no entry expected).
	empty, _ := NewPromptComponent(ComponentStateInstruction, SourceStateInstruction, 4,
		"", p834RoleID, 1, "hash-empty-1")
	tu, _ := NewPromptComponent(ComponentTestContext, SourceWorkUnit, 5,
		"Work unit + required tests", p834UnitID, 2, "hash-unit-2")
	rp, _ := NewPromptComponent(ComponentResponseProtocol, SourceResponseProtocol, 6,
		"Response protocol v2", p834Protocol, 2, "hash-rp-2")
	return Components{sys, ri, sf, empty, tu, rp}
}

// p834Fixture is the expected map for p834Components() — five entries (the
// six components minus the empty one), in component order, verbatim fields.
func p834Fixture() []ProvenanceEntry {
	return []ProvenanceEntry{
		{ComponentType: ComponentSystem, SourceID: p834RoleID, SourceVersion: 4, SourceHash: "hash-sys-4"},
		{ComponentType: ComponentRoleInstruction, SourceID: p834RoleID, SourceVersion: 9, SourceHash: "hash-ri-9"},
		{ComponentType: ComponentTaskContext, SourceID: p834FileID, SourceVersion: 3, SourceHash: "hash-file-3"},
		{ComponentType: ComponentTestContext, SourceID: p834UnitID, SourceVersion: 2, SourceHash: "hash-unit-2"},
		{ComponentType: ComponentResponseProtocol, SourceID: p834Protocol, SourceVersion: 2, SourceHash: "hash-rp-2"},
	}
}

// T8.31.1 — Every major component has provenance: every non-empty component
// yields exactly one map entry (no missing, no duplicates), in component
// order, and an empty component yields none.
func TestT8311_EveryMajorComponentHasProvenance(t *testing.T) {
	comp := p834Components()
	got := ProvenanceMap(comp)
	want := p834Fixture()
	if len(got) != len(want) {
		t.Fatalf("map length = %d, want %d (one entry per major component; 6 components, 1 empty)", len(got), len(want))
	}
	for i := range want {
		if got[i].ComponentType != want[i].ComponentType {
			t.Fatalf("entry %d type = %s, want %s", i, got[i].ComponentType, want[i].ComponentType)
		}
	}
	// Empty-content component (order 4) must be absent.
	for _, e := range got {
		if e.ComponentType == ComponentStateInstruction {
			t.Fatalf("empty component produced an entry: %+v", e)
		}
	}
}

// T8.31.2 — Source ID/version/hash retained VERBATIM: the entries carry the
// component's captured provenance fields exactly, with no re-derivation
// (mutating the input after the derivation must not affect the map).
func TestT8312_SourceIDVersionHashRetained(t *testing.T) {
	comp := p834Components()
	got := ProvenanceMap(comp)
	want := p834Fixture()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map = %#v\nwant %#v", got, want)
	}
	// No re-derivation: the derivation COPIES the fields into the entries.
	// Mutating the component AFTER the derivation must leave the derived map
	// intact (the map holds copies, not references into the component).
	comp[0].SourceID = uuid.New()
	comp[0].SourceVersion = 99
	comp[0].SourceHash = "mutated"
	if got := got[0]; got.SourceID != p834RoleID || got.SourceVersion != 4 || got.SourceHash != "hash-sys-4" {
		t.Fatalf("first entry after input mutation = %+v, want the original captured values", got)
	}
}

// T8.31.3 (unit half) — Determinism: repeated derivation over the same
// input is byte-identical. The DB restart-survival half is in
// internal/integration (T8.31.3 integration).
func TestT8313_RepeatedCompositionIdentical(t *testing.T) {
	comp := p834Components()
	first := ProvenanceMap(comp)
	second := ProvenanceMap(p834Components())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated derivation differs:\nfirst  %#v\nsecond %#v", first, second)
	}
}

// T8.31.4 (unit half) — Historical source reconstructable: a map derived from
// the CAPTURED (historical) components is independent of later-version
// components for the same source; the historical entry is recovered exactly
// from the captured fields alone (the DB half — persisted historical prompt
// records stay intact after the source advances — is in
// internal/integration).
func TestT8314_HistoricalSourceReconstructable(t *testing.T) {
	hist, _ := NewPromptComponent(ComponentSystem, SourceRoleSystemPrompt, 1,
		"historical v2 content", p834RoleID, 2, "hash-sys-2")
	cur, _ := NewPromptComponent(ComponentSystem, SourceRoleSystemPrompt, 1,
		"current v5 content", p834RoleID, 5, "hash-sys-5")

	histMap := ProvenanceMap(Components{hist})
	curMap := ProvenanceMap(Components{cur})

	if histMap[0] != (ProvenanceEntry{ComponentType: ComponentSystem, SourceID: p834RoleID, SourceVersion: 2, SourceHash: "hash-sys-2"}) {
		t.Fatalf("historical entry = %+v, want the captured v2 provenance", histMap[0])
	}
	if histMap[0] == curMap[0] {
		t.Fatalf("historical and current maps are identical — historical source not reconstructable as distinct: %+v", histMap[0])
	}

	// Mixed set: the historical entry survives alongside newer entries.
	mixed := ProvenanceMap(Components{hist, cur})
	if mixed[0].SourceVersion != 2 || mixed[1].SourceVersion != 5 {
		t.Fatalf("mixed map = %+v, want v2 entry retained verbatim ahead of v5", mixed)
	}
}

// T8.31.x (edge) — an all-empty / empty component set derives an EMPTY (nil)
// map, not an error.
func TestT8315_EmptyInputs(t *testing.T) {
	if got := ProvenanceMap(Components{}); len(got) != 0 {
		t.Fatalf("empty input map = %#v, want none", got)
	}
	empty, _ := NewPromptComponent(ComponentStateInstruction, SourceStateInstruction, 1,
		"", p834RoleID, 1, "h")
	if got := ProvenanceMap(Components{empty}); len(got) != 0 {
		t.Fatalf("all-empty map = %#v, want none", got)
	}
}
