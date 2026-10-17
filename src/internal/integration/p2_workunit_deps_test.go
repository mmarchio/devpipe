package integration

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

func (r *workUnitRig) twoUnits(t *testing.T, planID uuid.UUID, a, b string) (*wbs.WorkUnit, *wbs.WorkUnit) {
	t.Helper()
	ua := r.unit(t, planID, 1, a)
	ub := r.unit(t, planID, 2, b)
	return ua, ub
}

// T2.14.1 — Dependency persisted.
func TestT2141DependencyPersisted(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a, b := r.twoUnits(t, planID, "a", "b")
	if err := r.repo.AddDependency(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("add dependency: %v", err)
	}
	deps, err := r.repo.GetDependencies(ctx, a.ID)
	if err != nil {
		t.Fatalf("get dependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != b.ID {
		t.Fatalf("dependencies = %d units, want exactly b", len(deps))
	}
}

// T2.14.2 — Multiple dependencies supported.
func TestT2142MultipleDependencies(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a := r.unit(t, planID, 1, "a")
	b := r.unit(t, planID, 2, "b")
	c := r.unit(t, planID, 3, "c")
	d := r.unit(t, planID, 4, "d")
	for _, dep := range []uuid.UUID{b.ID, c.ID, d.ID} {
		if err := r.repo.AddDependency(ctx, a.ID, dep); err != nil {
			t.Fatalf("add dependency: %v", err)
		}
	}
	deps, err := r.repo.GetDependencies(ctx, a.ID)
	if err != nil {
		t.Fatalf("get dependencies: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("dependencies = %d, want 3", len(deps))
	}
	got := map[uuid.UUID]bool{}
	for _, u := range deps {
		got[u.ID] = true
	}
	for _, want := range []uuid.UUID{b.ID, c.ID, d.ID} {
		if !got[want] {
			t.Fatalf("missing dependency %v", want)
		}
	}
}

// T2.14.3 — Duplicate dependency rejected.
func TestT2143DuplicateDependencyRejected(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a, b := r.twoUnits(t, planID, "a", "b")
	if err := r.repo.AddDependency(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := r.repo.AddDependency(ctx, a.ID, b.ID); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate: got %v, want ErrConflict", err)
	}
}

// T2.14.4 — Self dependency rejected.
func TestT2144SelfDependencyRejected(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	a, _ := r.twoUnits(t, planID, "a", "b")
	if err := r.repo.AddDependency(ctx, a.ID, a.ID); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("self: got %v, want ErrConflict", err)
	}
}

// T2.14.5 — Cross-plan dependency rejected.
func TestT2145CrossPlanDependencyRejected(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planA := r.plan(t)
	planB := r.plan(t)
	a, _ := r.twoUnits(t, planA, "a", "b")
	_, c := r.twoUnits(t, planB, "c", "d")
	if err := r.repo.AddDependency(ctx, a.ID, c.ID); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("cross-plan: got %v, want ErrInvalidReference", err)
	}
}

// T2.14.6 / T2.16.6 / T2.16.7 — Dependency retrieval returns the correct graph.
func TestT2146DependencyRetrieval(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	// b, c -> a -> d ; unit e is independent.
	a := r.unit(t, planID, 1, "a")
	b := r.unit(t, planID, 2, "b")
	c := r.unit(t, planID, 3, "c")
	d := r.unit(t, planID, 4, "d")
	e := r.unit(t, planID, 5, "e")
	for _, dep := range []uuid.UUID{b.ID, c.ID} {
		if err := r.repo.AddDependency(ctx, a.ID, dep); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := r.repo.AddDependency(ctx, d.ID, a.ID); err != nil {
		t.Fatalf("add: %v", err)
	}
	// T2.16.6 — GetDependencies.
	deps, err := r.repo.GetDependencies(ctx, d.ID)
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != a.ID {
		t.Fatalf("d deps = %v, want [a]", ids(deps))
	}
	deps, err = r.repo.GetDependencies(ctx, a.ID)
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("a deps = %d, want 2", len(deps))
	}
	// T2.16.7 — GetDependents.
	dependents, err := r.repo.GetDependents(ctx, a.ID)
	if err != nil {
		t.Fatalf("dependents: %v", err)
	}
	if len(dependents) != 1 || dependents[0].ID != d.ID {
		t.Fatalf("a dependents = %v, want [d]", ids(dependents))
	}
	dependents, err = r.repo.GetDependents(ctx, b.ID)
	if err != nil {
		t.Fatalf("dependents: %v", err)
	}
	if len(dependents) != 1 || dependents[0].ID != a.ID {
		t.Fatalf("b dependents = %v, want [a]", ids(dependents))
	}
	if dependents, err = r.repo.GetDependents(ctx, e.ID); err != nil {
		t.Fatalf("dependents: %v", err)
	}
	if len(dependents) != 0 {
		t.Fatalf("e dependents = %d, want 0", len(dependents))
	}
}

func ids(units []*wbs.WorkUnit) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(units))
	for _, u := range units {
		out = append(out, u.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
