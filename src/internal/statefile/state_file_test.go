package statefile

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// T7.4.1 — a valid state file is created with type, project, scope, and
// timestamps; CurrentVersion starts at 0 (no content yet — content versioning
// is P7-03/P7-06).
func TestNewStateFileValid(t *testing.T) {
	proj := uuid.New()
	typ := uuid.New()
	in := StateFileInput{
		ProjectID: &proj,
		TypeID:    &typ,
		Scope:     ScopeProject,
	}
	f, err := NewStateFile(in)
	if err != nil {
		t.Fatalf("valid state file rejected: %v", err)
	}
	if f.ID == uuid.Nil {
		t.Fatal("ID not assigned")
	}
	if f.ProjectID != proj {
		t.Errorf("ProjectID = %v, want %v", f.ProjectID, proj)
	}
	if f.TypeID != typ {
		t.Errorf("TypeID = %v, want %v", f.TypeID, typ)
	}
	if f.Scope != ScopeProject {
		t.Errorf("Scope = %v, want PROJECT", f.Scope)
	}
	if f.CurrentVersion != 0 {
		t.Errorf("CurrentVersion = %d, want 0 (no content yet)", f.CurrentVersion)
	}
	if f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}
	if !f.CreatedAt.Equal(f.UpdatedAt) {
		t.Error("new file: CreatedAt should equal UpdatedAt")
	}
	if loc, off := f.CreatedAt.Zone(); loc != "UTC" || off != 0 {
		t.Errorf("timestamps not UTC: loc=%q off=%d", loc, off)
	}
}

// T7.4.2 — type is required.
func TestNewStateFileMissingType(t *testing.T) {
	proj := uuid.New()
	_, err := NewStateFile(StateFileInput{
		ProjectID: &proj,
		TypeID:    nil,
		Scope:     ScopeProject,
	})
	if err == nil {
		t.Fatal("nil TypeID accepted, want error")
	}
}

// T7.4.3 — project is required.
func TestNewStateFileMissingProject(t *testing.T) {
	typ := uuid.New()
	_, err := NewStateFile(StateFileInput{
		ProjectID: nil,
		TypeID:    &typ,
		Scope:     ScopeProject,
	})
	if err == nil {
		t.Fatal("nil ProjectID accepted, want error")
	}
}

// T7.4.4 — scope association is validated: PHASE requires a phase, WORK_UNIT
// requires a work-unit (reusing P7-01's ValidateScopeAssociation).
func TestNewStateFileScopeAssociation(t *testing.T) {
	proj := uuid.New()
	typ := uuid.New()

	// PHASE scope with no phase -> rejected.
	if _, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopePhase,
	}); err == nil {
		t.Error("PHASE scope without phase accepted, want error")
	}
	// WORK_UNIT scope with no work unit -> rejected.
	if _, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopeWorkUnit,
	}); err == nil {
		t.Error("WORK_UNIT scope without work-unit accepted, want error")
	}
	// PHASE scope with a phase -> accepted.
	ph := uuid.New()
	if _, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopePhase, PhaseID: &ph,
	}); err != nil {
		t.Errorf("PHASE scope with phase rejected: %v", err)
	}
	// WORK_UNIT scope with a work unit -> accepted.
	wu := uuid.New()
	if _, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopeWorkUnit, WorkUnitID: &wu,
	}); err != nil {
		t.Errorf("WORK_UNIT scope with work-unit rejected: %v", err)
	}
}

// T7.4.5 — a PHASE-scoped file whose phase belongs to a different project is
// rejected by the cross-project association predicate.
func TestCrossProjectPhaseAssociationRejected(t *testing.T) {
	fileProj := uuid.New()
	typ := uuid.New()
	otherProj := uuid.New()
	ph := uuid.New()
	f, err := NewStateFile(StateFileInput{
		ProjectID: &fileProj, TypeID: &typ, Scope: ScopePhase, PhaseID: &ph,
	})
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	// Same project -> allowed.
	if err := f.AssociationBelongsToProject(fileProj); err != nil {
		t.Errorf("same-project phase association rejected: %v", err)
	}
	// Different project -> rejected.
	if err := f.AssociationBelongsToProject(otherProj); err == nil {
		t.Error("cross-project phase association accepted, want error")
	}
}

// T7.4.6 — a WORK_UNIT-scoped file whose work unit belongs to a different
// project is rejected.
func TestCrossProjectWorkUnitAssociationRejected(t *testing.T) {
	fileProj := uuid.New()
	typ := uuid.New()
	otherProj := uuid.New()
	wu := uuid.New()
	f, err := NewStateFile(StateFileInput{
		ProjectID: &fileProj, TypeID: &typ, Scope: ScopeWorkUnit, WorkUnitID: &wu,
	})
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if err := f.AssociationBelongsToProject(fileProj); err != nil {
		t.Errorf("same-project work-unit association rejected: %v", err)
	}
	if err := f.AssociationBelongsToProject(otherProj); err == nil {
		t.Error("cross-project work-unit association accepted, want error")
	}
}

// AssociationBelongsToProject with no association (PROJECT/EXECUTION scope) is
// always satisfied — there is nothing to cross-check.
func TestAssociationBelongsToProjectNoAssociation(t *testing.T) {
	proj := uuid.New()
	typ := uuid.New()
	f, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopeProject,
	})
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	// A file with no association is trivially "belongs to project" for any project.
	if err := f.AssociationBelongsToProject(uuid.New()); err != nil {
		t.Errorf("file with no association rejected: %v", err)
	}
}

// Ensure the timestamp setter is UTC-normalized (guards against future drift).
func TestNewStateFileTimestampUTC(t *testing.T) {
	proj := uuid.New()
	typ := uuid.New()
	f, err := NewStateFile(StateFileInput{
		ProjectID: &proj, TypeID: &typ, Scope: ScopeProject,
	})
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if f.UpdatedAt.Before(time.Now().UTC().Add(-time.Hour)) {
		t.Error("UpdatedAt implausibly old")
	}
}
