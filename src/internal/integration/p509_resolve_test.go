package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/role"
	"overseer/internal/service"
)

// newRoleResolveService wires a fully-wired RoleService (P5-09).
func newRoleResolveService(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
	).WithInstructions(dbrepo.NewInstructionRepository(pool)).
		WithAssignments(dbrepo.NewRoleInstructionAssignmentRepository(pool)).
		WithInstructionVersions(dbrepo.NewInstructionVersionRepository(pool))
}

// T5.10.1 — Correct instructions returned.
func TestP509CorrectInstructionsReturned(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleResolveService(t, h)
	ctx := context.Background()

	roleID := seedRole(t, h, nil, "engineer-x", "E")
	instA := seedInstruction(t, h, "tdd", "TDD", "TDD content A")
	instB := seedInstruction(t, h, "cod", "CODING", "CODING content B")

	repo := newAssignmentRepo(t, h)
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, instA)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, instB)); err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.ResolveRoleInstructions(ctx, roleID)
	if err != nil {
		t.Fatalf("ResolveRoleInstructions: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved %d instructions, want 2", len(resolved))
	}
	keys := map[string]bool{}
	for _, r := range resolved {
		keys[r.Key] = true
	}
	if !keys["TDD"] || !keys["COD"] {
		t.Fatalf("resolved keys = %v, want TDD + COD (normalized)", keys)
	}
}

// T5.10.2 — Correct order returned (assignment position).
func TestP509OrderReturned(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleResolveService(t, h)
	ctx := context.Background()

	roleID := seedRole(t, h, nil, "engineer-x", "E")
	a := seedInstruction(t, h, "tdd", "TDD", "A")
	b := seedInstruction(t, h, "cod", "CODING", "B")
	c := seedInstruction(t, h, "sec", "SECURITY", "C")
	repo := newAssignmentRepo(t, h)
	for _, id := range []string{a, b, c} {
		if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := svc.ResolveRoleInstructions(ctx, roleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 3 {
		t.Fatalf("resolved %d, want 3", len(resolved))
	}
	want := []string{"TDD", "COD", "SEC"}
	for i, r := range resolved {
		if r.Key != want[i] {
			t.Fatalf("resolved[%d].Key = %q, want %q (assignment order)", i, r.Key, want[i])
		}
	}
}

// T5.10.3 — Disabled assignment excluded: unassigning removes it from the
// resolved set.
func TestP509DisabledAssignmentExcluded(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleResolveService(t, h)
	ctx := context.Background()

	roleID := seedRole(t, h, nil, "engineer-x", "E")
	a := seedInstruction(t, h, "tdd", "TDD", "A")
	b := seedInstruction(t, h, "cod", "CODING", "B")
	repo := newAssignmentRepo(t, h)
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, b)); err != nil {
		t.Fatal(err)
	}
	// "Disable" the assignment by unassigning it (WBS 5.9 removal semantics).
	if err := repo.Unassign(ctx, parseUUID(t, roleID), parseUUID(t, a)); err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.ResolveRoleInstructions(ctx, roleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Key != "COD" {
		t.Fatalf("resolved = %+v, want only COD", resolved)
	}
}

// T5.10.4 — Disabled instruction excluded: a disabled instruction in the
// assignment set is omitted from the resolved output.
func TestP509DisabledInstructionExcluded(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleResolveService(t, h)
	ctx := context.Background()

	roleID := seedRole(t, h, nil, "engineer-x", "E")
	a := seedInstruction(t, h, "tdd", "TDD", "A")
	b := seedInstruction(t, h, "cod", "CODING", "B")
	repo := newAssignmentRepo(t, h)
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, b)); err != nil {
		t.Fatal(err)
	}
	// Disable instruction A directly in storage.
	if _, err := h.DBExec(ctx, "UPDATE instructions SET enabled = false WHERE id = '"+a+"'"); err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.ResolveRoleInstructions(ctx, roleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Key != "COD" {
		t.Fatalf("resolved = %+v, want only COD (A disabled)", resolved)
	}
}

// T5.10.5 — Version/hash metadata included: the resolved entry carries the
// instruction's current version, its content, and a hash matching the content.
func TestP509VersionHashMetadataIncluded(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleResolveService(t, h)
	ctx := context.Background()

	roleID := seedRole(t, h, nil, "engineer-x", "E")
	const content = "# CODING\n- go\n- style"
	b := seedInstruction(t, h, "cod", "CODING", content)
	// Append a v2 version so the current version is 2 (resolution must track
	// the current version, not the stale instructions row).
	verRepo := newInstructionVersionRepo(t, h)
	if _, err := verRepo.AppendVersion(ctx, b, "v1"); err != nil {
		t.Fatal(err)
	}
	v2, err := verRepo.AppendVersion(ctx, b, content)
	if err != nil {
		t.Fatal(err)
	}
	repo := newAssignmentRepo(t, h)
	if _, err := repo.Assign(ctx, parseUUID(t, roleID), parseUUID(t, b)); err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.ResolveRoleInstructions(ctx, roleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved %d, want 1", len(resolved))
	}
	r := resolved[0]
	if r.Version != v2.Version {
		t.Fatalf("resolved version = %d, want %d (current)", r.Version, v2.Version)
	}
	if r.Content != content {
		t.Fatalf("resolved content = %q, want the current content", r.Content)
	}
	if r.ContentHash == "" {
		t.Fatalf("resolved has no content hash")
	}
	if r.ContentHash != hashOf(content) {
		t.Fatalf("resolved hash does not match hash(content)")
	}
}

// hashOf mirrors role.HashSystemPromptContent for test assertions.
func hashOf(content string) string {
	return role.HashSystemPromptContent(content)
}
