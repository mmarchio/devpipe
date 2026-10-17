package integration

import (
	"fmt"
	"testing"

	"overseer/internal/dbrepo"
)

// P5-30 role isolation between projects (WBS 5.32). The fixture pins the
// WBS 5.32 state: project A ENGINEER v5, project B ENGINEER v2, global
// ENGINEER v4 (system-prompt versions). The global ENGINEER is the
// boot-seeded default role (SeedDefaultRoles writes its v1 prompt); a
// CreateRole with key "engineer" would collide on the normalized scoped key.
// InitializeProjectRoles copies the globals to project scope, so the project
// roles start at prompt v1.

type p530Scope struct {
	RoleID      string
	PromptVer   int
	Content     string
	ContentHash string
	RoleVersion int
	Hash        string
	Serial      string
}

func p530Capture(t *testing.T, h *Harness, id string) *p530Scope {
	t.Helper()
	svc := snapshotSvc(t, h)
	cur, err := svc.GetCurrentSystemPrompt(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	return &p530Scope{
		RoleID:      id,
		PromptVer:   cur.Version,
		Content:     cur.Content,
		ContentHash: cur.ContentHash,
		RoleVersion: snap.RoleVersion,
		Hash:        snap.ConfigurationHash(),
		Serial:      snap.Serialize(),
	}
}

func p530FindEngineer(t *testing.T, h *Harness, projectID *string) string {
	t.Helper()
	roles, err := snapshotSvc(t, h).ListRoles(ctx0(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if r.Key == "ENGINEER" {
			return r.ID.String()
		}
	}
	t.Fatal("ENGINEER role not found in scope")
	return ""
}

func TestP530ProjectIsolation(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	pA := parseUUID(t, createPhaseProject(t, h))
	pB := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), pA.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InitializeProjectRoles(ctx0(), pB.String()); err != nil {
		t.Fatal(err)
	}
	sA := pA.String()
	sB := pB.String()
	globalID := p530FindEngineer(t, h, nil)
	idA := p530FindEngineer(t, h, &sA)
	idB := p530FindEngineer(t, h, &sB)

	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	// Pin the WBS 5.32 fixture state: A v5, B v2, global v4 (each role
	// starts at the seeded/copied v1, so append 4/1/3 more versions).
	for i := 1; i <= 4; i++ {
		if _, err := prompts.AppendVersion(ctx0(), idA, fmt.Sprintf("engineer A prompt v%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prompts.AppendVersion(ctx0(), idB, "engineer B prompt v2"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := prompts.AppendVersion(ctx0(), globalID, fmt.Sprintf("engineer global prompt v%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}

	aBefore := p530Capture(t, h, idA)
	bBefore := p530Capture(t, h, idB)
	gBefore := p530Capture(t, h, globalID)
	// Pin the fixture values so a drift from the 5.32 spec fails explicitly.
	if aBefore.PromptVer != 5 || bBefore.PromptVer != 2 || gBefore.PromptVer != 4 {
		t.Fatalf("fixture state = A v%d, B v%d, global v%d, want 5/2/4", aBefore.PromptVer, bBefore.PromptVer, gBefore.PromptVer)
	}

	// T5.32.1 — Edit project A's ENGINEER: a new, independent prompt version.
	if _, err := svc.UpdateSystemPrompt(ctx0(), idA, "engineer A prompt v6 (isolated edit)", aBefore.PromptVer); err != nil {
		t.Fatalf("project A edit: %v", err)
	}
	aAfter := p530Capture(t, h, idA)
	bAfter := p530Capture(t, h, idB)
	gAfter := p530Capture(t, h, globalID)

	if aAfter.PromptVer != 6 {
		t.Fatalf("project A prompt version = %d, want 6", aAfter.PromptVer)
	}
	if aAfter.Content != "engineer A prompt v6 (isolated edit)" {
		t.Fatalf("project A content = %q, want the edited prompt", aAfter.Content)
	}

	// T5.32.2 — Project B's ENGINEER is unchanged (version + content + hash).
	if bAfter.PromptVer != bBefore.PromptVer || bAfter.Content != bBefore.Content || bAfter.ContentHash != bBefore.ContentHash {
		t.Fatalf("project B changed by a project A edit: %+v -> %+v", bBefore, bAfter)
	}
	if bAfter.Hash != bBefore.Hash {
		t.Fatalf("project B configuration hash changed: %s -> %s", bBefore.Hash, bAfter.Hash)
	}

	// T5.32.3 — Global ENGINEER is unchanged (version + content + hash).
	if gAfter.PromptVer != gBefore.PromptVer || gAfter.Content != gBefore.Content || gAfter.ContentHash != gBefore.ContentHash {
		t.Fatalf("global changed by a project A edit: %+v -> %+v", gBefore, gAfter)
	}
	if gAfter.Hash != gBefore.Hash {
		t.Fatalf("global configuration hash changed: %s -> %s", gBefore.Hash, gAfter.Hash)
	}

	// T5.32.4 — Configuration hashes reflect the three independent
	// configurations: the edited scope's hash moves (its configuration
	// differs), the unchanged scopes keep their exact hash, and the edited
	// configuration's hash no longer matches the unchanged ones.
	if aAfter.Hash == aBefore.Hash {
		t.Fatal("project A configuration hash did not change after the edit")
	}
	if bAfter.Hash != bBefore.Hash {
		t.Fatalf("project B configuration hash changed: %s -> %s", bBefore.Hash, bAfter.Hash)
	}
	if gAfter.Hash != gBefore.Hash {
		t.Fatalf("global configuration hash changed: %s -> %s", gBefore.Hash, gAfter.Hash)
	}
	if aAfter.Hash == bAfter.Hash || aAfter.Hash == gAfter.Hash {
		t.Fatalf("edited scope hash matches an unchanged scope: A=%s B=%s G=%s", aAfter.Hash, bAfter.Hash, gAfter.Hash)
	}
}
