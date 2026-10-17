package wbs

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func id() uuid.UUID { return uuid.New() }

// depsFrom builds a unit -> dependencies map from (unit, dep) pairs.
func depsFrom(pairs ...[2]uuid.UUID) map[uuid.UUID][]uuid.UUID {
	m := map[uuid.UUID][]uuid.UUID{}
	for _, p := range pairs {
		m[p[0]] = append(m[p[0]], p[1])
	}
	return m
}

// T2.15.1 — Linear graph accepted (A → B → C).
func TestT2151LinearAccepted(t *testing.T) {
	a, b, c := id(), id(), id()
	if err := CheckDependencyGraph(depsFrom([2]uuid.UUID{a, b}, [2]uuid.UUID{b, c})); err != nil {
		t.Fatalf("linear graph rejected: %v", err)
	}
}

// T2.15.2 — Branching graph accepted (C depends on A and B).
func TestT2152BranchingAccepted(t *testing.T) {
	a, b, c := id(), id(), id()
	if err := CheckDependencyGraph(depsFrom([2]uuid.UUID{c, a}, [2]uuid.UUID{c, b})); err != nil {
		t.Fatalf("branching graph rejected: %v", err)
	}
}

// T2.15.3 — Direct cycle rejected (A ↔ B).
func TestT2153DirectCycleRejected(t *testing.T) {
	a, b := id(), id()
	err := CheckDependencyGraph(depsFrom([2]uuid.UUID{a, b}, [2]uuid.UUID{b, a}))
	if !isCycle(err) {
		t.Fatalf("direct cycle: got %v, want ErrDependencyCycle", err)
	}
}

// T2.15.4 — Indirect cycle rejected (A → B → C → A).
func TestT2154IndirectCycleRejected(t *testing.T) {
	a, b, c := id(), id(), id()
	err := CheckDependencyGraph(depsFrom([2]uuid.UUID{a, b}, [2]uuid.UUID{b, c}, [2]uuid.UUID{c, a}))
	if !isCycle(err) {
		t.Fatalf("indirect cycle: got %v, want ErrDependencyCycle", err)
	}
}

// T2.15.5 — Self cycle rejected (A → A).
func TestT2155SelfCycleRejected(t *testing.T) {
	a := id()
	err := CheckDependencyGraph(depsFrom([2]uuid.UUID{a, a}))
	if !isCycle(err) {
		t.Fatalf("self cycle: got %v, want ErrDependencyCycle", err)
	}
}

func isCycle(err error) bool {
	var cycle *ErrDependencyCycle
	return errors.As(err, &cycle)
}

// Determinism guard: identical input must yield identical cycle paths,
// since map iteration order is nondeterministic.
func TestCyclePathDeterministic(t *testing.T) {
	ids := make([]uuid.UUID, 0, 6)
	for i := 0; i < 6; i++ {
		ids = append(ids, id())
	}
	pairs := [][2]uuid.UUID{}
	for i := 1; i < len(ids); i++ {
		pairs = append(pairs, [2]uuid.UUID{ids[i], ids[i-1]})
	}
	pairs = append(pairs, [2]uuid.UUID{ids[0], ids[len(ids)-1]})
	var first []uuid.UUID
	for i := 0; i < 25; i++ {
		err := CheckDependencyGraph(depsFrom(pairs...))
		var cycle *ErrDependencyCycle
		if !errors.As(err, &cycle) {
			t.Fatalf("iteration %d: got %v, want ErrDependencyCycle", i, err)
		}
		if i == 0 {
			first = cycle.Path
		}
		if len(cycle.Path) != len(first) {
			t.Fatalf("iteration %d: path length %d, want %d", i, len(cycle.Path), len(first))
		}
		for j := range first {
			if cycle.Path[j] != first[j] {
				t.Fatalf("iteration %d: nondeterministic path", i)
			}
		}
	}
}
