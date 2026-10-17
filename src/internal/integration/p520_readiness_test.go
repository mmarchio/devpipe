package integration

import (
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// seedReadyProject initializes a project from the global defaults and adds an
// allowed state to every project role so all five required roles are valid
// (prompt comes from the seeded global defaults).
func seedReadyProject(t *testing.T, h *Harness, svc *service.RoleService) string {
	t.Helper()
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	stRepo := dbrepo.NewRoleAllowedStateRepository(h.App.DB().Raw())
	roleRepo := dbrepo.NewRoleRepository(h.App.DB().Raw())
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	for _, r := range created {
		if _, err := stRepo.AssignState(ctx0(), r.ID, "implementation"); err != nil {
			t.Fatal(err)
		}
	}
	return projID.String()
}

func readySvc(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	return snapshotSvc(t, h)
}

// T5.20.1 — Project with valid required roles passes.
func TestP520ValidRequiredRolesPass(t *testing.T) {
	h := NewHarness(t)
	svc := readySvc(t, h)
	projID := seedReadyProject(t, h, svc)
	rep, err := svc.CheckRoleReadiness(ctx0(), projID, service.DefaultRequiredRoles)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ready {
		t.Fatalf("readiness = %+v, want ready", rep)
	}
}

// T5.20.2 — Missing required role fails readiness.
func TestP520MissingRequiredRoleFails(t *testing.T) {
	h := NewHarness(t)
	svc := readySvc(t, h)
	projID := seedReadyProject(t, h, svc)
	// Require a key that was never initialized (not among the five defaults).
	rep, err := svc.CheckRoleReadiness(ctx0(), projID, append(append([]string{}, service.DefaultRequiredRoles...), "NONEXISTENT"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Fatal("readiness = ready, want fail for a missing required role")
	}
	var found bool
	for _, f := range rep.Failures {
		if f.RoleKey == "NONEXISTENT" && f.Reason == "missing required role" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-role failure not reported: %+v", rep.Failures)
	}
}

// T5.20.3 — Disabled required role fails readiness.
func TestP520DisabledRequiredRoleFails(t *testing.T) {
	h := NewHarness(t)
	svc := readySvc(t, h)
	projID := seedReadyProject(t, h, svc)
	roleRepo := dbrepo.NewRoleRepository(h.App.DB().Raw())
	pid := parseUUID(t, projID)
	created, _ := roleRepo.ListByScope(ctx0(), &pid)
	var target string
	for _, r := range created {
		if r.Key == "ENGINEER" {
			target = r.ID.String()
		}
	}
	if target == "" {
		t.Fatal("no ENGINEER project role")
	}
	if err := svc.Disable(ctx0(), target); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.CheckRoleReadiness(ctx0(), projID, service.DefaultRequiredRoles)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Fatal("readiness = ready, want fail for a disabled required role")
	}
}

// T5.20.4 — Invalid required role fails readiness.
func TestP520InvalidRequiredRoleFails(t *testing.T) {
	h := NewHarness(t)
	svc := readySvc(t, h)
	projID := seedReadyProject(t, h, svc)
	// Remove all allowed states from one required role → it becomes invalid.
	roleRepo := dbrepo.NewRoleRepository(h.App.DB().Raw())
	raw := h.App.DB().Raw()
	pid := parseUUID(t, projID)
	created, _ := roleRepo.ListByScope(ctx0(), &pid)
	for _, r := range created {
		if r.Key == "ENGINEER" {
			raw.Exec(ctx0(), `DELETE FROM role_allowed_states WHERE role_id = $1`, r.ID)
		}
	}
	rep, err := svc.CheckRoleReadiness(ctx0(), projID, service.DefaultRequiredRoles)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Fatal("readiness = ready, want fail for an invalid required role")
	}
}

// T5.20.5 — Optional disabled role doesn't fail readiness.
func TestP520OptionalDisabledDoesNotFail(t *testing.T) {
	h := NewHarness(t)
	svc := readySvc(t, h)
	projID := seedReadyProject(t, h, svc)
	pid := parseUUID(t, projID)
	// Create an extra (optional) role in the project and disable it.
	extra := seedRole(t, h, &pid, "optional-role", "Opt")
	if err := svc.Disable(ctx0(), extra); err != nil {
		t.Fatal(err)
	}
	// Required set excludes the optional role.
	rep, err := svc.CheckRoleReadiness(ctx0(), projID, service.DefaultRequiredRoles)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ready {
		t.Fatalf("readiness = %+v, want ready (disabled optional role should not fail)", rep)
	}
}
