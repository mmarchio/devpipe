package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// newRoleSeedService wires the real role service to the harness pool.
func newRoleSeedService(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
	)
}

// globalRoleKeys returns the keys of all global roles.
func globalRoleKeys(t *testing.T, h *Harness) []string {
	t.Helper()
	ctx := context.Background()
	repo := newRoleRepo(t, h)
	roles, err := repo.ListByScope(ctx, nil)
	if err != nil {
		t.Fatalf("ListByScope(global): %v", err)
	}
	var keys []string
	for _, r := range roles {
		keys = append(keys, r.Key)
	}
	return keys
}

// countGlobalPromptVersions returns the number of system-prompt rows for the
// global role with the given key.
func countGlobalPromptVersions(t *testing.T, h *Harness, key string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM role_system_prompts r WHERE r.role_id IN "+
			"(SELECT id FROM roles WHERE project_id IS NULL AND key = '"+key+"')", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T5.2.1–T5.2.5 — the five default roles exist with the expected keys and a
// non-empty v1 system prompt.
func TestP504DefaultRolesExist(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSeedService(t, h)
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatalf("SeedDefaultRoles: %v", err)
	}
	want := map[string]bool{"ARCHITECT": false, "PLANNER": false, "ENGINEER": false, "TESTER": false, "PROJECT_MANAGER": false}
	for _, k := range globalRoleKeys(t, h) {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Fatalf("default role %s not seeded", k)
		}
	}
	// Each default has exactly one v1 system prompt.
	for k := range want {
		if n := countGlobalPromptVersions(t, h, k); n != 1 {
			t.Fatalf("default role %s has %d system-prompt versions, want 1", k, n)
		}
	}
}

// T5.2.6 — Seeding is idempotent: calling again creates no duplicates.
func TestP504SeedingIdempotent(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSeedService(t, h)
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatalf("second SeedDefaultRoles: %v", err)
	}
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatalf("third SeedDefaultRoles: %v", err)
	}
	if got := len(globalRoleKeys(t, h)); got != 5 {
		t.Fatalf("global roles after repeated seeding = %d, want exactly 5", got)
	}
	for _, k := range []string{"ARCHITECT", "PLANNER", "ENGINEER", "TESTER", "PROJECT_MANAGER"} {
		if n := countGlobalPromptVersions(t, h, k); n != 1 {
			t.Fatalf("role %s prompt versions = %d, want 1 (idempotent)", k, n)
		}
	}
}

// T5.2.7 — Re-running startup (a fresh harness instance = fresh app on the
// same seeded DB) doesn't duplicate defaults.
func TestP504RestartNoDuplicate(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSeedService(t, h)
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A second service over the SAME pool (simulating restart re-seed) must
	// not create duplicates.
	if err := svc.SeedDefaultRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(globalRoleKeys(t, h)); got != 5 {
		t.Fatalf("global roles after restart seed = %d, want 5", got)
	}
}

// T5.2.7 (startup wiring) — a fresh harness boot (app.Start) auto-seeds the
// five defaults; no explicit seed call needed.
func TestP504StartupAutoSeeds(t *testing.T) {
	h := NewHarness(t)
	if got := len(globalRoleKeys(t, h)); got != 5 {
		t.Fatalf("global roles after a fresh boot = %d, want 5 (auto-seeded at startup)", got)
	}
	for _, k := range []string{"ARCHITECT", "PLANNER", "ENGINEER", "TESTER", "PROJECT_MANAGER"} {
		if n := countGlobalPromptVersions(t, h, k); n != 1 {
			t.Fatalf("role %s prompt versions after boot = %d, want 1", k, n)
		}
	}
}
