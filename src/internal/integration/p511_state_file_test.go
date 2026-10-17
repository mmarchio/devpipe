package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
)

func newStateFileRepo(t *testing.T, h *Harness) *dbrepo.RoleStateFileRepository {
	t.Helper()
	return dbrepo.NewRoleStateFileRepository(h.App.DB().Raw())
}

// T5.12.1 — State-file requirement assigned.
func TestP511StateFileAssigned(t *testing.T) {
	h := NewHarness(t)
	repo := newStateFileRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	rec, err := repo.Require(context.Background(), parseUUID(t, roleID), "ACTIVE_TASK")
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if rec.StateFileKey != "ACTIVE_TASK" {
		t.Fatalf("state file key = %q, want ACTIVE_TASK", rec.StateFileKey)
	}
}

// T5.12.2 — Multiple requirements supported.
func TestP511MultipleStateFiles(t *testing.T) {
	h := NewHarness(t)
	repo := newStateFileRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	for _, k := range []string{"ACTIVE_TASK", "ARCHITECTURE", "PROJECT_STATE"} {
		if _, err := repo.Require(context.Background(), parseUUID(t, roleID), k); err != nil {
			t.Fatalf("require %q: %v", k, err)
		}
	}
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("state files = %d, want 3", len(list))
	}
}

// T5.12.3 — Duplicate requirement rejected.
func TestP511DuplicateStateFileRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newStateFileRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	if _, err := repo.Require(context.Background(), parseUUID(t, roleID), "ACTIVE_TASK"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Require(context.Background(), parseUUID(t, roleID), "ACTIVE_TASK"); !isConflict(err) {
		t.Fatalf("duplicate state-file: got %v, want ErrConflict", err)
	}
}

// T5.12.4 — Requirement order deterministic.
func TestP511StateFileOrderDeterministic(t *testing.T) {
	h := NewHarness(t)
	repo := newStateFileRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	want := []string{"ACTIVE_TASK", "ARCHITECTURE", "PROJECT_STATE"}
	for _, k := range want {
		if _, err := repo.Require(context.Background(), parseUUID(t, roleID), k); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range want {
		if list[i].StateFileKey != k {
			t.Fatalf("position %d = %q, want %q", i, list[i].StateFileKey, k)
		}
	}
}

// T5.12.5 — Removing a requirement doesn't affect unrelated roles.
func TestP511RemoveIsolated(t *testing.T) {
	h := NewHarness(t)
	repo := newStateFileRepo(t, h)
	a := parseUUID(t, seedRole(t, h, nil, "engineer-x", "E"))
	b := parseUUID(t, seedRole(t, h, nil, "tester-x", "T"))
	for _, rid := range []uuid.UUID{a, b} {
		if _, err := repo.Require(context.Background(), rid, "ACTIVE_TASK"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Remove(context.Background(), a, "ACTIVE_TASK"); err != nil {
		t.Fatal(err)
	}
	la, _ := repo.ListByRole(context.Background(), a)
	lb, _ := repo.ListByRole(context.Background(), b)
	if len(la) != 0 {
		t.Fatalf("role A state files after remove = %d, want 0", len(la))
	}
	if len(lb) != 1 || lb[0].StateFileKey != "ACTIVE_TASK" {
		t.Fatalf("role B state files after A removed = %+v, want ACTIVE_TASK intact", lb)
	}
	if err := repo.Remove(context.Background(), b, "NOPE"); err == nil {
		t.Fatal("remove of an absent requirement should be an error")
	}
}
