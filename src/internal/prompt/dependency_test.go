package prompt

import (
	"strings"
	"testing"
)

// P8-21 — WBS 8.15 dependency-contract context, TDD T8.15.1-T8.15.5.
//
// The composition renders the stored dependency CONTRACTS of the current
// work unit’s dependencies as prompt components (WBS 8.15): the
// contract summary (name/purpose/input/output/guarantees) only, never the
// dependency’s full source. The composition consumes ONLY the
// caller-supplied captured values at the snapshot-pinned state — the
// required dependency set (the unit’s captured dependencies) and the
// stored contracts — there is no "latest" input (the P8-16..P8-20
// pattern). Repeated composition over the same input is byte-identical.
//
// Inclusion rule (stated per ACTIVE_TASK NOTES): a stored contract is
// included when its dependency name is in the current unit’s captured
// dependency set; a stored contract whose name is NOT in the set
// (unrelated) is excluded (T8.15.2). A required dependency with no stored
// contract is a missing optional — it composes to no component and no
// failure, no invented content (T8.15.4, the P8-19/P8-20 skip-and-continue
// choice). The render carries the contract summary only; the dependency’s
// full source is never implicitly included even when present on the
// contract (T8.15.5).

// depContracts821 is the canonical set of stored contracts:
// ValidateEmail and ParseDate are the current unit’s dependencies (the
// required set); UnrelatedService is a stored contract NOT in the set
// (unrelated). Each contract carries a distinctive full source so the
// source-exclusion pin (T8.15.5) is non-vacuous.
func depContracts821() []DependencyContract {
	return []DependencyContract{
		{
			Name:       "ValidateEmail",
			Purpose:    "Validate normalized email address.",
			Input:      "string",
			Output:     "error",
			Guarantees: []string{"nil means valid", "invalid syntax returns ErrInvalidEmail"},
			Source:     "func ValidateEmail(s string) error { FULL SOURCE A }",
		},
		{
			Name:       "ParseDate",
			Purpose:    "Parse ISO-8601 date strings.",
			Input:      "string",
			Output:     "time.Time, error",
			Guarantees: []string{"UTC normalized"},
			Source:     "func ParseDate(s string) (time.Time, error) { FULL SOURCE B }",
		},
		{
			Name:       "UnrelatedService",
			Purpose:    "An unrelated stored contract.",
			Input:      "int",
			Output:     "string",
			Guarantees: nil,
			Source:     "func UnrelatedService() { FULL SOURCE C }",
		},
	}
}

// required821 is the current unit’s captured dependency set.
func required821() []string {
	return []string{"ValidateEmail", "ParseDate"}
}

// T8.15.1 — Required dependency contracts included: every dependency in
// the current unit’s captured set that has a stored contract is rendered
// as a DEPENDENCY_CONTRACT-sourced TASK_CONTEXT component carrying the
// contract summary.
func TestT8151_RequiredDependencyContractsIncluded(t *testing.T) {
	comps, err := ComposeDependencyContracts(required821(), depContracts821())
	if err != nil {
		t.Fatalf("ComposeDependencyContracts: %v", err)
	}
	if len(comps) != 2 {
		t.Fatalf("want 2 required-contract components, got %d", len(comps))
	}
	joined := ""
	for _, c := range comps {
		if c.Type != ComponentTaskContext {
			t.Fatalf("want TASK_CONTEXT, got %s", c.Type)
		}
		if c.Source != SourceDependencyContract {
			t.Fatalf("want DEPENDENCY_CONTRACT source, got %s", c.Source)
		}
		joined += c.Content
	}
	for _, want := range []string{
		"Dependency: ValidateEmail",
		"Validate normalized email address.",
		"invalid syntax returns ErrInvalidEmail",
		"Dependency: ParseDate",
		"Parse ISO-8601 date strings.",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("required contract content %q missing from render:\n%s", want, joined)
		}
	}
}

// T8.15.2 — Unrelated dependencies excluded: a stored contract whose name
// is not in the current unit’s captured set is present in the contract
// SET but is never rendered.
func TestT8152_UnrelatedDependenciesExcluded(t *testing.T) {
	comps, err := ComposeDependencyContracts(required821(), depContracts821())
	if err != nil {
		t.Fatalf("ComposeDependencyContracts: %v", err)
	}
	joined := ""
	for _, c := range comps {
		joined += c.Content
	}
	if strings.Contains(joined, "UnrelatedService") {
		t.Fatalf("unrelated dependency leaked into render:\n%s", joined)
	}
	if strings.Contains(joined, "An unrelated stored contract.") {
		t.Fatalf("unrelated contract content leaked:\n%s", joined)
	}
	if len(comps) != 2 {
		t.Fatalf("unrelated contract must not add a component: got %d, want 2", len(comps))
	}
}

// T8.15.3 — Contract ordering deterministic: the same required set + same
// contract SET in any input order composes to byte-identical output, in
// sorted (by dependency name) order.
func TestT8153_ContractOrderingDeterministic(t *testing.T) {
	a, err := ComposeDependencyContracts(required821(), depContracts821())
	if err != nil {
		t.Fatalf("ComposeDependencyContracts(a): %v", err)
	}
	reversed := []DependencyContract{
		depContracts821()[2],
		depContracts821()[1],
		depContracts821()[0],
	}
	b, err := ComposeDependencyContracts(required821(), reversed)
	if err != nil {
		t.Fatalf("ComposeDependencyContracts(b): %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("input order changed component count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("contract order not deterministic at %d:\n--- a ---\n%s\n--- b ---\n%s", i, a[i].Content, b[i].Content)
		}
	}
	if len(a) != 2 {
		t.Fatalf("want 2 components, got %d", len(a))
	}
	// Sorted by dependency name: ParseDate (P) before ValidateEmail (V).
	if !strings.Contains(a[0].Content, "ParseDate") {
		t.Fatalf("deterministic order: first component should be ParseDate:\n%s", a[0].Content)
	}
	if !strings.Contains(a[1].Content, "ValidateEmail") {
		t.Fatalf("deterministic order: second component should be ValidateEmail:\n%s", a[1].Content)
	}
}

// T8.15.4 — Missing optional contract doesn’t break composition: a
// required dependency with no stored contract is skipped (no component, no
// failure, no invented content); the present contract still composes; a set
// with no matching contract composes to zero components, no failure.
func TestT8154_MissingOptionalContractDoesNotBreak(t *testing.T) {
	req := []string{"ValidateEmail", "NoStoredContract"}
	contracts := []DependencyContract{
		{
			Name:       "ValidateEmail",
			Purpose:    "Validate normalized email address.",
			Input:      "string",
			Output:     "error",
			Guarantees: []string{"nil means valid"},
		},
	}
	comps, err := ComposeDependencyContracts(req, contracts)
	if err != nil {
		t.Fatalf("missing optional contract must not fail composition: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want 1 component (the present contract), got %d", len(comps))
	}
	if !strings.Contains(comps[0].Content, "ValidateEmail") {
		t.Fatalf("present contract missing:\n%s", comps[0].Content)
	}
	if strings.Contains(comps[0].Content, "NoStoredContract") {
		t.Fatalf("missing dependency must not be invented:\n%s", comps[0].Content)
	}
	none, err := ComposeDependencyContracts([]string{"GhostDep"}, contracts)
	if err != nil {
		t.Fatalf("no matching contract must not fail: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("no required dependency has a contract: want 0 components, got %d", len(none))
	}
}

// T8.15.5 — Full unrelated source isn’t implicitly included: the render
// carries the contract summary only; the full source of every contract
// (including the INCLUDED required ones) is absent, while the summary
// fields are present.
func TestT8155_FullUnrelatedSourceNotIncluded(t *testing.T) {
	comps, err := ComposeDependencyContracts(required821(), depContracts821())
	if err != nil {
		t.Fatalf("ComposeDependencyContracts: %v", err)
	}
	joined := ""
	for _, c := range comps {
		joined += c.Content
	}
	for _, src := range []string{"FULL SOURCE A", "FULL SOURCE B", "FULL SOURCE C"} {
		if strings.Contains(joined, src) {
			t.Fatalf("full source leaked into render:\n%s", joined)
		}
	}
	if !strings.Contains(joined, "Validate normalized email address.") {
		t.Fatalf("contract summary (purpose) must be present:\n%s", joined)
	}
}

// Determinism pin (same composition-input requirement as T8.9.6): repeated
// composition over the same input is byte-identical.
func TestT8156_RepeatedCompositionByteIdentical(t *testing.T) {
	first, err := ComposeDependencyContracts(required821(), depContracts821())
	if err != nil {
		t.Fatalf("ComposeDependencyContracts: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ComposeDependencyContracts(required821(), depContracts821())
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) {
			t.Fatalf("repeat %d: length %d vs %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("repeat %d: component %d not byte-identical", i, j)
			}
		}
	}
}
