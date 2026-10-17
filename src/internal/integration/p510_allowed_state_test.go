package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
)

func newAllowedStateRepo(t *testing.T, h *Harness) *dbrepo.RoleAllowedStateRepository {
	t.Helper()
	return dbrepo.NewRoleAllowedStateRepository(h.App.DB().Raw())
}

// T5.11.1 — Allowed state assigned.
func TestP510AllowedStateAssigned(t *testing.T) {
	h := NewHarness(t)
	repo := newAllowedStateRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	rec, err := repo.AssignState(context.Background(), parseUUID(t, roleID), "implementation")
	if err != nil {
		t.Fatalf("AssignState: %v", err)
	}
	if rec.StateKey != "implementation" {
		t.Fatalf("state key = %q, want implementation", rec.StateKey)
	}
}

// T5.11.2 — Multiple states supported.
func TestP510MultipleStates(t *testing.T) {
	h := NewHarness(t)
	repo := newAllowedStateRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	for _, k := range []string{"implementation", "continuation"} {
		if _, err := repo.AssignState(context.Background(), parseUUID(t, roleID), k); err != nil {
			t.Fatalf("assign %q: %v", k, err)
		}
	}
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("allowed states = %d, want 2", len(list))
	}
}

// T5.11.3 — Duplicate state assignment rejected.
func TestP510DuplicateStateRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newAllowedStateRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	if _, err := repo.AssignState(context.Background(), parseUUID(t, roleID), "implementation"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AssignState(context.Background(), parseUUID(t, roleID), "implementation"); !isConflict(err) {
		t.Fatalf("duplicate state: got %v, want ErrConflict", err)
	}
}

// T5.11.4 — State key retained exactly (case, spacing).
func TestP510StateKeyRetainedExactly(t *testing.T) {
	h := NewHarness(t)
	repo := newAllowedStateRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	const key = "implementation-detail v2"
	if _, err := repo.AssignState(context.Background(), parseUUID(t, roleID), key); err != nil {
		t.Fatal(err)
	}
	eligible, err := repo.IsEligible(context.Background(), parseUUID(t, roleID), key)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatalf("eligibility for exact key = false, want true")
	}
	// A differing key (case) is distinct.
	if el, _ := repo.IsEligible(context.Background(), parseUUID(t, roleID), "IMPLEMENTATION-DETAIL v2"); el {
		t.Fatalf("eligibility for a different key = true, want false")
	}
}

// T5.11.5 — Role eligibility query works.
func TestP510EligibilityQuery(t *testing.T) {
	h := NewHarness(t)
	repo := newAllowedStateRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	if _, err := repo.AssignState(context.Background(), parseUUID(t, roleID), "testing"); err != nil {
		t.Fatal(err)
	}
	if el, _ := repo.IsEligible(context.Background(), parseUUID(t, roleID), "testing"); !el {
		t.Fatalf("eligible(testing) = false, want true")
	}
	if el, _ := repo.IsEligible(context.Background(), parseUUID(t, roleID), "review"); el {
		t.Fatalf("eligible(review) = true, want false")
	}
}
