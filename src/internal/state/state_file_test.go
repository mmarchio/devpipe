package state

import (
	"reflect"
	"testing"
)

// T6.6.5 — the union-dedup resolution helper combines role-level requirements
// (first, in their stored order) with state-level requirements (appended in
// their stored order), keeping the first occurrence of each category.

func TestResolveStateFileRequirementsOverlap(t *testing.T) {
	got := ResolveStateFileRequirements(
		[]string{"PROJECT_STATE", "ARCHITECTURE"},
		[]string{"ARCHITECTURE", "ACTIVE_TASK"},
	)
	want := []string{"PROJECT_STATE", "ARCHITECTURE", "ACTIVE_TASK"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolution = %v, want %v", got, want)
	}
}

func TestResolveStateFileRequirementsDisjoint(t *testing.T) {
	got := ResolveStateFileRequirements([]string{"A"}, []string{"B", "C"})
	want := []string{"A", "B", "C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolution = %v, want %v", got, want)
	}
}

func TestResolveStateFileRequirementsEmptySides(t *testing.T) {
	// Role-only.
	got := ResolveStateFileRequirements([]string{"A"}, nil)
	want := []string{"A"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("role-only resolution = %v, want %v", got, want)
	}
	// State-only.
	got = ResolveStateFileRequirements(nil, []string{"B"})
	want = []string{"B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state-only resolution = %v, want %v", got, want)
	}
	// Neither: the empty result is an empty (non-nil) slice.
	got = ResolveStateFileRequirements(nil, nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("empty resolution = %v, want empty non-nil slice", got)
	}
}

func TestResolveStateFileRequirementsRepeatedWithinInput(t *testing.T) {
	// A category repeated within one input list is deduplicated too — the
	// result is a set in first-seen order.
	got := ResolveStateFileRequirements([]string{"A", "A"}, []string{"A", "B"})
	want := []string{"A", "B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolution = %v, want %v", got, want)
	}
}
