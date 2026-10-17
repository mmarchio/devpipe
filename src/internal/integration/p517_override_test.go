package integration

import (
	"testing"

	"github.com/google/uuid"
)

// T5.17.1 — Project role editable.
func TestP517ProjectRoleEditable(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	roleRepo := newRoleRepo(t, h)
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projRoleID string
	for _, c := range created {
		if c.Key == globals[0].Key {
			projRoleID = c.ID.String()
		}
	}
	if projRoleID == "" {
		t.Fatal("no project role found")
	}
	if _, err := svc.UpdateRole(ctx0(), projRoleID, "Edited Name", "Edited description"); err != nil {
		t.Fatal(err)
	}
	rec, err := roleRepo.Get(ctx0(), projRoleID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "Edited Name" || rec.Description != "Edited description" {
		t.Fatalf("project role after edit = %+v, want edited name/description", rec)
	}
}

// T5.17.2 — Global role unchanged.
func TestP517GlobalUnchanged(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	roleRepo := newRoleRepo(t, h)
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	created, _ := roleRepo.ListByScope(ctx0(), &projID)
	var projRoleID, globalRoleID string
	for _, c := range created {
		if c.Key == globals[0].Key {
			projRoleID = c.ID.String()
		}
	}
	globalRoleID = globals[0].ID.String()
	globalBefore, _ := roleRepo.Get(ctx0(), globalRoleID)
	if _, err := svc.UpdateRole(ctx0(), projRoleID, "Edited Name", "Edited description"); err != nil {
		t.Fatal(err)
	}
	globalAfter, _ := roleRepo.Get(ctx0(), globalRoleID)
	if globalAfter.Name != globalBefore.Name || globalAfter.Description != globalBefore.Description {
		t.Fatalf("global role mutated by project edit: %+v -> %+v", globalBefore, globalAfter)
	}
}

// T5.17.3 — Other projects unchanged.
func TestP517OtherProjectsUnchanged(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	pA := parseUUID(t, createPhaseProject(t, h))
	pB := parseUUID(t, createPhaseProject(t, h))
	for _, p := range []uuid.UUID{pA, pB} {
		if _, err := svc.InitializeProjectRoles(ctx0(), p.String()); err != nil {
			t.Fatal(err)
		}
	}
	roleRepo := newRoleRepo(t, h)
	globals, _ := roleRepo.ListByScope(ctx0(), nil)
	// Edit project A's copy.
	createdA, _ := roleRepo.ListByScope(ctx0(), &pA)
	var idA string
	for _, c := range createdA {
		if c.Key == globals[0].Key {
			idA = c.ID.String()
		}
	}
	if _, err := svc.UpdateRole(ctx0(), idA, "A Edited", "A desc"); err != nil {
		t.Fatal(err)
	}
	// Project B's copy is unchanged.
	createdB, _ := roleRepo.ListByScope(ctx0(), &pB)
	for _, c := range createdB {
		if c.Key == globals[0].Key && c.Name == "A Edited" {
			t.Fatal("project B role was mutated by a project A edit")
		}
	}
}

// T5.17.4 — Project role version increments.
func TestP517VersionIncrements(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	projID := parseUUID(t, createPhaseProject(t, h))
	if _, err := svc.InitializeProjectRoles(ctx0(), projID.String()); err != nil {
		t.Fatal(err)
	}
	roleRepo := newRoleRepo(t, h)
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
	if _, err := svc.UpdateRole(ctx0(), projRoleID, "Edited", "desc"); err != nil {
		t.Fatal(err)
	}
	rec, _ := roleRepo.Get(ctx0(), projRoleID)
	if rec.Version != beforeVer+1 {
		t.Fatalf("version after edit = %d, want %d (incremented)", rec.Version, beforeVer+1)
	}
}
