package integration

import (
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/role"
	"overseer/internal/service"
)

// snapshotSvc wires a fully-populated RoleService for the RoleSnapshot read
// model (WBS 5.14/5.29), including the tx pool so the one-transaction read is
// exercised.
func snapshotSvc(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(h.App.DB().Raw()),
		dbrepo.NewSystemPromptRepository(h.App.DB().Raw()),
	).WithInstructions(dbrepo.NewInstructionRepository(h.App.DB().Raw())).
		WithAssignments(dbrepo.NewRoleInstructionAssignmentRepository(h.App.DB().Raw())).
		WithInstructionVersions(dbrepo.NewInstructionVersionRepository(h.App.DB().Raw())).
		WithStateFiles(dbrepo.NewRoleStateFileRepository(h.App.DB().Raw())).
		WithAllowedStates(dbrepo.NewRoleAllowedStateRepository(h.App.DB().Raw())).
		WithTxPool(h.App.DB().Raw())
}

// seedSnapshotRole builds a role with a v1 prompt, two assigned instructions,
// two allowed states, and two state files — the full snapshot surface.
func seedSnapshotRole(t *testing.T, h *Harness, svc *service.RoleService) string {
	t.Helper()
	roleID := seedRole(t, h, nil, "engineer-x", "E")
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), roleID, "system prompt body"); err != nil {
		t.Fatal(err)
	}
	// Two instructions assigned in order.
	i1 := seedInstruction(t, h, "code", "Code", "write code")
	i2 := seedInstruction(t, h, "test", "Test", "write tests")
	if err := svc.AssignInstruction(ctx0(), roleID, i1); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignInstruction(ctx0(), roleID, i2); err != nil {
		t.Fatal(err)
	}
	// Allowed states + state files.
	stRepo := dbrepo.NewRoleAllowedStateRepository(h.App.DB().Raw())
	sfRepo := dbrepo.NewRoleStateFileRepository(h.App.DB().Raw())
	rid := parseUUID(t, roleID)
	for _, k := range []string{"implementation", "review"} {
		if _, err := stRepo.AssignState(ctx0(), rid, k); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"ACTIVE_TASK", "PROJECT_STATE"} {
		if _, err := sfRepo.Require(ctx0(), rid, k); err != nil {
			t.Fatal(err)
		}
	}
	return roleID
}

// T5.14.1 — Snapshot contains role.
func TestP514SnapshotContainsRole(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RoleKey != "ENGINEER-X" || snap.RoleVersion != 1 {
		t.Fatalf("snapshot role key = %q version = %d, want ENGINEER-X / 1", snap.RoleKey, snap.RoleVersion)
	}
}

// T5.14.2 — Snapshot contains exact system-prompt version.
func TestP514SnapshotPromptVersion(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SystemPrompt.Version != 1 || snap.SystemPrompt.Content != "system prompt body" {
		t.Fatalf("system prompt snapshot = %+v, want version 1 with seeded content", snap.SystemPrompt)
	}
}

// T5.14.3 — Snapshot contains ordered instructions.
func TestP514SnapshotOrderedInstructions(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Instructions) != 2 {
		t.Fatalf("instructions = %d, want 2", len(snap.Instructions))
	}
	if snap.Instructions[0].Key != "CODE" || snap.Instructions[1].Key != "TEST" {
		t.Fatalf("instruction order = [%q,%q], want [code,test]", snap.Instructions[0].Key, snap.Instructions[1].Key)
	}
}

// T5.14.4 — Snapshot contains hashes.
func TestP514SnapshotContainsHashes(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SystemPrompt.ContentHash != role.HashSystemPromptContent(snap.SystemPrompt.Content) {
		t.Fatalf("system prompt hash = %q, want computed hash", snap.SystemPrompt.ContentHash)
	}
	for _, ins := range snap.Instructions {
		if ins.ContentHash != role.HashSystemPromptContent(ins.Content) {
			t.Fatalf("instruction %q hash = %q, want computed hash", ins.Key, ins.ContentHash)
		}
	}
}

// T5.14.5 — Snapshot deterministic for unchanged configuration.
func TestP514SnapshotDeterministic(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	a, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.SystemPrompt.Content != b.SystemPrompt.Content ||
		a.SystemPrompt.ContentHash != b.SystemPrompt.ContentHash ||
		len(a.Instructions) != len(b.Instructions) {
		t.Fatal("two snapshots of unchanged config differ")
	}
	for i := range a.Instructions {
		if a.Instructions[i] != b.Instructions[i] {
			t.Fatalf("instruction %d differs between snapshots: %+v vs %+v", i, a.Instructions[i], b.Instructions[i])
		}
	}
}

// T5.29.1 — System prompt classified SYSTEM.
func TestP529SystemPromptClassified(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SystemPrompt.Classification != role.ClassificationSystem {
		t.Fatalf("system prompt classification = %q, want %q", snap.SystemPrompt.Classification, role.ClassificationSystem)
	}
}

// T5.29.2 — Role instructions classified ROLE_INSTRUCTION.
func TestP529InstructionsClassified(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, ins := range snap.Instructions {
		if ins.Classification != role.ClassificationRoleInstruction {
			t.Fatalf("instruction %q classification = %q, want %q", ins.Key, ins.Classification, role.ClassificationRoleInstruction)
		}
	}
}

// T5.29.3 — Source metadata preserved in snapshots (classifications + hashes
// retained, not just content).
func TestP529SourceMetadataPreserved(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SystemPrompt.Classification == "" || snap.SystemPrompt.ContentHash == "" {
		t.Fatal("system prompt source metadata (classification/hash) not preserved")
	}
	if len(snap.Instructions) == 0 || snap.Instructions[0].Classification == "" {
		t.Fatal("instruction source metadata not preserved")
	}
}

// T5.29.4 — Classification survives persistence/retrieval (fresh snapshot
// from the same store re-reads the same classifications).
func TestP529ClassificationSurvivesRetrieval(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	first, _ := svc.SnapshotRole(ctx0(), id)
	// Re-read through a freshly-constructed service (new repository handles,
	// same store) to confirm classifications come from stored data.
	svc2 := snapshotSvc(t, h)
	second, err := svc2.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if second.SystemPrompt.Classification != first.SystemPrompt.Classification {
		t.Fatal("system prompt classification did not survive retrieval")
	}
	if len(second.Instructions) != len(first.Instructions) {
		t.Fatalf("instruction count changed on retrieval: %d -> %d", len(first.Instructions), len(second.Instructions))
	}
	for i := range second.Instructions {
		if second.Instructions[i].Classification != first.Instructions[i].Classification {
			t.Fatal("instruction classification did not survive retrieval")
		}
	}
}
