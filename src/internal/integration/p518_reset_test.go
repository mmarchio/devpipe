package integration

import (
	"testing"

	"overseer/internal/dbrepo"
)

// T5.18.1 — Reset creates a new project-role version.
func TestP518ResetCreatesNewVersion(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projRoleID string
	beforeVer := 0
	for _, c := range created {
		if c.Key == globals[0].Key {
			projRoleID = c.ID.String()
			beforeVer = c.Version
		}
	}
	if _, err := svc.ResetRole(ctx0(), projRoleID); err != nil {
		t.Fatal(err)
	}
	rec, _ := roleRepo.Get(ctx0(), projRoleID)
	if rec.Version != beforeVer+1 {
		t.Fatalf("version after reset = %d, want %d", rec.Version, beforeVer+1)
	}
}

// T5.18.2 — Current default copied (global's current prompt becomes the project role's latest).
func TestP518CurrentDefaultCopied(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projRoleID, globalRoleID string
	for _, c := range created {
		if c.Key == globals[0].Key {
			projRoleID = c.ID.String()
		}
	}
	globalRoleID = globals[0].ID.String()
	// Change the global's current prompt so the reset must copy the NEW content.
	const globalV2 = "global prompt after change"
	if _, err := prompts.AppendVersion(ctx0(), globalRoleID, globalV2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResetRole(ctx0(), projRoleID); err != nil {
		t.Fatal(err)
	}
	// The project role's latest prompt version should now carry the global's current content.
	cur, err := prompts.GetCurrentVersion(ctx0(), projRoleID)
	if err != nil {
		t.Fatal(err)
	}
	if cur < 1 {
		t.Fatal("no prompt version after reset")
	}
	latest, err := prompts.GetVersion(ctx0(), projRoleID, cur)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Content != globalV2 {
		t.Fatalf("reset copied content = %q, want global's current %q", latest.Content, globalV2)
	}
}

// T5.18.3 — Historical project role retained.
func TestP518HistoricalRetained(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projRoleID string
	for _, c := range created {
		if c.Key == globals[0].Key {
			projRoleID = c.ID.String()
		}
	}
	// Capture the pre-reset prompt version.
	before, _ := prompts.ListVersions(ctx0(), projRoleID)
	if _, err := svc.ResetRole(ctx0(), projRoleID); err != nil {
		t.Fatal(err)
	}
	after, _ := prompts.ListVersions(ctx0(), projRoleID)
	if len(after) < len(before)+1 {
		t.Fatalf("versions after reset = %d, want >= %d (history retained + new)", len(after), len(before)+1)
	}
	// The original v1 content is still retrievable.
	if len(before) > 0 {
		if _, err := prompts.GetVersion(ctx0(), projRoleID, before[0].Version); err != nil {
			t.Fatalf("historical prompt version %d lost after reset: %v", before[0].Version, err)
		}
	}
}

// T5.18.4 — Other roles unaffected.
func TestP518OtherRolesUnaffected(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var targetID string
	otherBefore := make(map[string]int)
	for _, c := range created {
		if c.Key == globals[0].Key {
			targetID = c.ID.String()
		} else {
			otherBefore[c.Key] = c.Version
		}
	}
	if _, err := svc.ResetRole(ctx0(), targetID); err != nil {
		t.Fatal(err)
	}
	createdAfter, _ := roleRepo.ListByScope(ctx0(), &projID)
	for _, c := range createdAfter {
		if c.Key == globals[0].Key {
			continue
		}
		if before, ok := otherBefore[c.Key]; ok && c.Version != before {
			t.Fatalf("role %s version changed %d -> %d by resetting %s", c.Key, before, c.Version, globals[0].Key)
		}
	}
}
