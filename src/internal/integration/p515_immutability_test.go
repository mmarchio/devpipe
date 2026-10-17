package integration

import (
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/role"
)

// T5.15.1 — Existing snapshot retains the old version after an edit.
func TestP515ExistingSnapshotRetainsVersion(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	before, _ := svc.SnapshotRole(ctx0(), id)
	const v2 = "edited prompt content"
	if _, err := prompts.AppendVersion(ctx0(), id, v2); err != nil {
		t.Fatal(err)
	}
	// The already-captured snapshot still references the original version.
	if before.SystemPrompt.Version != 1 || before.SystemPrompt.Content != "system prompt body" {
		t.Fatalf("existing snapshot mutated after edit: %+v", before.SystemPrompt)
	}
}

// T5.15.2 — New snapshot resolves the latest version.
func TestP515NewSnapshotResolvesLatest(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	const v2 = "edited prompt content"
	if _, err := prompts.AppendVersion(ctx0(), id, v2); err != nil {
		t.Fatal(err)
	}
	after, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.SystemPrompt.Version != 2 || after.SystemPrompt.Content != v2 {
		t.Fatalf("new snapshot = %+v, want version 2 with edited content", after.SystemPrompt)
	}
}

// T5.15.3 — Historical version remains retrievable.
func TestP515HistoricalVersionRetrievable(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), id, "v2 content"); err != nil {
		t.Fatal(err)
	}
	v1, err := prompts.GetVersion(ctx0(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Content != "system prompt body" {
		t.Fatalf("historical v1 content = %q, want original", v1.Content)
	}
}

// T5.15.4 — Editing instructions doesn't mutate old snapshot references.
func TestP515InstructionEditDoesNotMutateSnapshot(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	before, _ := svc.SnapshotRole(ctx0(), id)
	if len(before.Instructions) == 0 {
		t.Fatal("no instructions in baseline snapshot")
	}
	// Bump the version of an assigned instruction (via the version store).
	iv := dbrepo.NewInstructionVersionRepository(h.App.DB().Raw())
	instrRepo := dbrepo.NewInstructionRepository(h.App.DB().Raw())
	instrs, _ := instrRepo.ListByScope(ctx0(), nil)
	var bumped bool
	for _, ins := range instrs {
		if ins.Key == before.Instructions[0].Key {
			if _, err := iv.AppendVersion(ctx0(), ins.ID.String(), "edited instruction"); err != nil {
				t.Fatal(err)
			}
			bumped = true
		}
	}
	if !bumped {
		t.Fatal("could not locate assigned instruction to bump")
	}
	// The old snapshot's instruction content is unchanged.
	firstBefore := before.Instructions[0]
	if firstBefore.Content == "edited instruction" {
		t.Fatal("old snapshot instruction mutated by edit")
	}
}

// T5.28.1 — Active snapshot remains unchanged during a "running" edit.
func TestP528ActiveSnapshotUnchanged(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	active, _ := svc.SnapshotRole(ctx0(), id)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), id, "running edit"); err != nil {
		t.Fatal(err)
	}
	if active.SystemPrompt.Version != 1 {
		t.Fatalf("active snapshot mutated: version = %d, want 1", active.SystemPrompt.Version)
	}
}

// T5.28.2 — New snapshot uses the latest version.
func TestP528NewSnapshotUsesLatest(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), id, "running edit"); err != nil {
		t.Fatal(err)
	}
	fresh, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SystemPrompt.Version != 2 {
		t.Fatalf("new snapshot version = %d, want 2", fresh.SystemPrompt.Version)
	}
}

// T5.28.3 — Historical version remains available.
func TestP528HistoricalVersionAvailable(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), id, "running edit"); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.GetVersion(ctx0(), id, 1); err != nil {
		t.Fatalf("historical v1 not retrievable: %v", err)
	}
}

// T5.28.4 — Editing role does not corrupt active execution metadata.
func TestP528EditDoesNotCorruptActiveMetadata(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	active, _ := svc.SnapshotRole(ctx0(), id)
	// Capture the active snapshot's full shape (classification + hashes).
	if active.SystemPrompt.Classification != role.ClassificationSystem {
		t.Fatal("active snapshot classification corrupted")
	}
	if active.SystemPrompt.ContentHash == "" {
		t.Fatal("active snapshot hash corrupted")
	}
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(ctx0(), id, "running edit"); err != nil {
		t.Fatal(err)
	}
	// Active snapshot metadata still intact and internally consistent.
	if role.HashSystemPromptContent(active.SystemPrompt.Content) != active.SystemPrompt.ContentHash {
		t.Fatal("active snapshot hash no longer matches its (unchanged) content")
	}
}
