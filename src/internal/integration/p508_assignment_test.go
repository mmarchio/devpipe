package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

func newAssignmentRepo(t *testing.T, h *Harness) *dbrepo.RoleInstructionAssignmentRepository {
	t.Helper()
	return dbrepo.NewRoleInstructionAssignmentRepository(h.App.DB().Raw())
}

// newRoleAssignService wires a RoleService with role + instruction +
// assignment repositories.
func newRoleAssignService(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
	).WithInstructions(dbrepo.NewInstructionRepository(pool)).
		WithAssignments(dbrepo.NewRoleInstructionAssignmentRepository(pool))
}

// T5.9.1 — Instruction assigned to role.
func TestP508InstructionAssigned(t *testing.T) {
	h := NewHarness(t)
	repo := newAssignmentRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	instID := seedInstruction(t, h, "tdd", "TDD", "c")

	rec, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, instID))
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if rec.Position != 1 {
		t.Fatalf("first assignment position = %d, want 1", rec.Position)
	}
}

// T5.9.2 — Multiple instructions supported.
func TestP508MultipleInstructions(t *testing.T) {
	h := NewHarness(t)
	repo := newAssignmentRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	a := seedInstruction(t, h, "tdd", "TDD", "c")
	b := seedInstruction(t, h, "cod", "CODING", "c")
	c := seedInstruction(t, h, "sec", "SECURITY", "c")

	for _, id := range []string{a, b, c} {
		if _, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, id)); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("assignments = %d, want 3", len(list))
	}
}

// T5.9.3 — Assignment order preserved (by position).
func TestP508OrderPreserved(t *testing.T) {
	h := NewHarness(t)
	repo := newAssignmentRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	a := seedInstruction(t, h, "tdd", "TDD", "c")
	b := seedInstruction(t, h, "cod", "CODING", "c")
	c := seedInstruction(t, h, "sec", "SECURITY", "c")

	for _, id := range []string{a, b, c} {
		if _, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	for i, rec := range list {
		if rec.Position != i+1 {
			t.Fatalf("list not in position order: position %d at index %d", rec.Position, i)
		}
	}
	// Positions are the exact insertion sequence 1,2,3.
	wantIDs := map[string]bool{a: false, b: false, c: false}
	for _, rec := range list {
		wantIDs[rec.InstructionID.String()] = true
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Fatalf("instruction %s missing from ordered list", id)
		}
	}
}

// T5.9.4 — Duplicate assignment rejected.
func TestP508DuplicateRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newAssignmentRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	instID := seedInstruction(t, h, "tdd", "TDD", "c")
	if _, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, instID)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, instID)); !isConflict(err) {
		t.Fatalf("duplicate assignment: got %v, want ErrConflict", err)
	}
}

// T5.9.5 — Removing assignment doesn't delete instruction.
func TestP508RemovalKeepsInstruction(t *testing.T) {
	h := NewHarness(t)
	repo := newAssignmentRepo(t, h)
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	instID := seedInstruction(t, h, "tdd", "TDD", "c")
	if _, err := repo.Assign(context.Background(), parseUUID(t, roleID), parseUUID(t, instID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Unassign(context.Background(), parseUUID(t, roleID), parseUUID(t, instID)); err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	// The assignment row is gone.
	list, err := repo.ListByRole(context.Background(), parseUUID(t, roleID))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("assignments after unassign = %d, want 0", len(list))
	}
	// The instruction row is retained.
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM instructions WHERE id = '"+instID+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("instruction rows after unassign = %d, want 1 (retained)", n)
	}
}

// T5.9.6 — Instruction from unrelated project rejected (service level).
func TestP508CrossProjectRejected(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleAssignService(t, h)
	ctx := context.Background()
	// A global role and a project-scoped instruction (different scope).
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	pid := roleProjectID(t, createPhaseProject(t, h))
	instID := seedProjectInstruction(t, h, &pid, "tdd", "TDD", "c")

	if err := svc.AssignInstruction(ctx, roleID, instID); err == nil {
		t.Fatalf("cross-project assignment accepted, want rejection")
	}
	if err := svc.AssignInstruction(ctx, roleID, instID); err != service.ErrInvalidInput {
		t.Fatalf("cross-project assignment: got %v, want ErrInvalidInput", err)
	}
}

// parseUUID parses a uuid string into uuid.UUID.
func parseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// seedProjectInstruction creates a project-scoped instruction.
func seedProjectInstruction(t *testing.T, h *Harness, pid *uuid.UUID, key, name, content string) string {
	t.Helper()
	repo := newInstructionRepo(t, h)
	rec := newInstructionRecord(t, pid, key, name, content)
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed project instruction: %v", err)
	}
	return rec.ID.String()
}

var _ = persistence.ErrInvalidReference
