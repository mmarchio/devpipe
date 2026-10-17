package integration

import (
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// initSvc wires a fully-populated RoleService (tx pool included) for the
// project-initialization tests (WBS 5.3).
func initSvc(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(h.App.DB().Raw()),
		dbrepo.NewSystemPromptRepository(h.App.DB().Raw()),
	).WithTxPool(h.App.DB().Raw())
}

// T5.3.1 — Project receives default roles.
func TestP513ProjectReceivesDefaults(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	globals, _ := newRoleRepo(t, h).ListByScope(ctx0(), nil)
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	got, _ := newRoleRepo(t, h).ListByScope(ctx0(), &projID)
	if len(got) != len(globals) {
		t.Fatalf("project roles = %d, want %d (all globals copied)", len(got), len(globals))
	}
}

// T5.3.2 — Role content copied correctly (name + current prompt content).
func TestP513ContentCopiedCorrectly(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	promptRepo := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	projID := parseUUID(t, createPhaseProject(t, h))
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	if len(globals) == 0 {
		t.Fatal("no global roles seeded")
	}
	g := globals[0]
	const v2 = "updated global prompt content"
	if _, err := promptRepo.AppendVersion(ctx0(), g.ID.String(), v2); err != nil {
		t.Fatal(err)
	}
	created, err := svc.InitializeProjectRoles(ctx0(), projID.String())
	if err != nil {
		t.Fatal(err)
	}
	var copyRec *struct{ id, key, name string }
	for _, c := range created {
		if c.Key == g.Key {
			copyRec = &struct{ id, key, name string }{c.ID.String(), c.Key, c.Name}
		}
	}
	if copyRec == nil {
		t.Fatalf("no project role created for key %q", g.Key)
	}
	if copyRec.name != g.Name {
		t.Fatalf("project role name = %q, want %q", copyRec.name, g.Name)
	}
	pv, err := promptRepo.GetVersion(ctx0(), copyRec.id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Content != v2 {
		t.Fatalf("copied v1 prompt content = %q, want the global's current content %q", pv.Content, v2)
	}
}

// T5.3.3 — Project role has independent ID.
func TestP513IndependentID(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	for _, c := range created {
		for _, g := range globals {
			if c.ID == g.ID {
				t.Fatalf("project role ID %s equals a global role ID — not independent", c.ID)
			}
		}
	}
}

// T5.3.4 — Later global edit doesn't mutate project role.
func TestP513GlobalEditDoesNotMutateProject(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	promptRepo := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	projID := parseUUID(t, createPhaseProject(t, h))
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projCopyID string
	for _, c := range created {
		if c.Key == globals[0].Key {
			projCopyID = c.ID.String()
		}
	}
	if projCopyID == "" {
		t.Fatal("project copy not found")
	}
	before, _ := promptRepo.GetVersion(ctx0(), projCopyID, 1)
	const globalEdit = "changed global prompt after project init"
	if _, err := promptRepo.AppendVersion(ctx0(), globals[0].ID.String(), globalEdit); err != nil {
		t.Fatal(err)
	}
	after, _ := promptRepo.GetVersion(ctx0(), projCopyID, 1)
	if after.Content != before.Content {
		t.Fatalf("project role prompt mutated by global edit: %q -> %q", before.Content, after.Content)
	}
}

// T5.3.5 — Initialization is transactional (one commit covers all globals).
func TestP513Transactional(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if before, _ := roleRepo.ListByScope(ctx0(), &projID); len(before) != 0 {
		t.Fatalf("pre-existing project roles = %d, want 0", len(before))
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	after, _ := roleRepo.ListByScope(ctx0(), &projID)
	if len(after) != len(globals) {
		t.Fatalf("after init project roles = %d, want %d (all committed in one tx)", len(after), len(globals))
	}
}

// T5.3.6 — Duplicate initialization does not duplicate project roles.
func TestP513DuplicateInitNoDuplicates(t *testing.T) {
	h := NewHarness(t)
	svc := initSvc(t, h)
	roleRepo := newRoleRepo(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	for i := 0; i < 3; i++ {
		if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
			t.Fatal(err)
		}
	}
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	got, _ := roleRepo.ListByScope(ctx0(), &projID)
	if len(got) != len(globals) {
		t.Fatalf("after 3 inits project roles = %d, want %d (no duplicates)", len(got), len(globals))
	}
}
