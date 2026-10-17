package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-09 (WBS 7.9/7.10/7.14/7.15): per-type state-file content ops — project
// state, phase state, architecture, test state — driven over the harness
// pool through the P7-08 CRUD service (CreateFile / UpdateContent /
// GetCurrentVersion / GetVersion). Service-only surface (the REST API is
// P7-21).

// p709DefaultTypeID resolves one of the six GLOBAL default type rows seeded
// at every harness boot (P7-07) by its key.
func p709DefaultTypeID(t *testing.T, h *Harness, key string) uuid.UUID {
	t.Helper()
	recs, err := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw()).ListByScope(context.Background(), nil)
	if err != nil {
		t.Fatalf("list global types: %v", err)
	}
	for _, r := range recs {
		if r.Key == key {
			return r.ID
		}
	}
	t.Fatalf("global default type %s not seeded", key)
	return uuid.Nil
}

// T7.9.1-T7.9.4 — project state: created with content, project scope
// enforced (row + unique project-scoped lookup), historical versions
// retained, no phase/work-unit association required (T7.9.4 — the file is
// created with nil associations, which PROJECT scope permits).
func TestP709ProjectStateLifecycle(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	typeID := p709DefaultTypeID(t, h, "PROJECT_STATE")

	// T7.9.4: creation without any association is valid for PROJECT scope.
	fv, err := svc.CreateFile(ctx, service.CreateFileInput{
		ProjectID: projectID.String(),
		TypeID:    typeID.String(),
		Scope:     "PROJECT",
	})
	if err != nil {
		t.Fatalf("create project state: %v", err)
	}
	if fv.PhaseID != nil || fv.WorkUnitID != nil {
		t.Fatalf("project state file carries association %+v, want none", fv)
	}
	fileID := uuid.MustParse(fv.ID)

	// T7.9.1: the first content update creates version 1 and persists it.
	v1, err := svc.UpdateContent(ctx, fileID.String(), "# Project State\nPhase: 7", 0)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("v1 version = %d, want 1", v1.Version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 1 {
		t.Fatalf("persisted versions = %d, want 1", n)
	}

	// T7.9.2: project scope enforced — the row belongs to the project and the
	// (type, PROJECT scope) lookup resolves to this file.
	var storedProject string
	if err := h.DBScan(ctx, "SELECT project_id::text FROM state_files WHERE id = '"+fileID.String()+"'", &storedProject); err != nil {
		t.Fatalf("scan project: %v", err)
	}
	if storedProject != projectID.String() {
		t.Fatalf("stored project = %s, want %s", storedProject, projectID)
	}
	rec, err := dbrepo.NewStateFileRepository(h.App.DB().Raw()).GetByTypeAndScope(ctx, projectID, typeID, "PROJECT", nil, nil)
	if err != nil {
		t.Fatalf("scope lookup: %v", err)
	}
	if rec.ID != fileID {
		t.Fatalf("scope lookup resolved %s, want %s", rec.ID, fileID)
	}

	// T7.9.3: a second update appends v2 and the historical v1 stays intact.
	if _, err := svc.UpdateContent(ctx, fileID.String(), "updated state", 1); err != nil {
		t.Fatalf("v2: %v", err)
	}
	hist, err := svc.GetVersion(ctx, fileID.String(), 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if hist.Content != "# Project State\nPhase: 7" {
		t.Fatalf("historical v1 = %q", hist.Content)
	}
	if got := p706Current(t, h, fileID); got != 2 {
		t.Fatalf("current_version = %d, want 2", got)
	}
}

// T7.10.1-T7.10.4 — phase state: bound to a phase, cross-phase lookup
// isolated, history retained, phase completion (driven by direct SQL)
// leaves the state history intact.
func TestP709PhaseStateLifecycle(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, phaseA, _ := p705Rig(t, h)
	phaseB := seedPhaseStatus(t, h, projectID, 2, "Foundation B", "PENDING")
	typeID := p709DefaultTypeID(t, h, "PHASE_STATE")

	fv, err := svc.CreateFile(ctx, service.CreateFileInput{
		ProjectID: projectID.String(),
		TypeID:    typeID.String(),
		Scope:     "PHASE",
		PhaseID:   ptrStr(phaseA.String()),
	})
	if err != nil {
		t.Fatalf("create phase state: %v", err)
	}
	fileID := uuid.MustParse(fv.ID)

	// T7.10.1: the file row is bound to the phase.
	var storedPhase string
	if err := h.DBScan(ctx, "SELECT phase_id::text FROM state_files WHERE id = '"+fileID.String()+"'", &storedPhase); err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	if storedPhase != phaseA.String() {
		t.Fatalf("stored phase = %s, want %s", storedPhase, phaseA)
	}

	// T7.10.2: the file is invisible from the other phase.
	other, err := dbrepo.NewStateFileRepository(h.App.DB().Raw()).ListByPhase(ctx, phaseB)
	if err != nil {
		t.Fatalf("list by phase: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("phase B sees %d files, want 0", len(other))
	}

	// T7.10.3: history retained across two updates.
	if _, err := svc.UpdateContent(ctx, fileID.String(), "phase objective v1", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := svc.UpdateContent(ctx, fileID.String(), "phase objective v2", 1); err != nil {
		t.Fatalf("v2: %v", err)
	}
	hist, err := svc.GetVersion(ctx, fileID.String(), 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if hist.Content != "phase objective v1" {
		t.Fatalf("historical v1 = %q", hist.Content)
	}

	// T7.10.4: completing the phase (status -> DONE via direct SQL, as the
	// WBS defers phase-status transitions to Phase 2) deletes nothing: the
	// version rows and the file row are unchanged and still readable.
	if _, err := h.DBExec(ctx, "UPDATE phases SET status='DONE', completed_at=now() WHERE id='"+phaseA.String()+"'"); err != nil {
		t.Fatalf("complete phase: %v", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("versions after phase DONE = %d, want 2", count)
	}
	if got := p706Current(t, h, fileID); got != 2 {
		t.Fatalf("current_version after phase DONE = %d, want 2", got)
	}
	cur, err := svc.GetCurrentVersion(ctx, fileID.String())
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.Content != "phase objective v2" {
		t.Fatalf("current content after phase DONE = %q", cur.Content)
	}
}

// T7.14.1-T7.14.5 — architecture: created, controlled update appends a
// version (the P7-04 policy path for the default-seeded ARCHITECTURE type,
// whose source class admits controlled updates), history retained, the
// content hash changes on edit, project isolation enforced.
func TestP709ArchitectureLifecycle(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectA, _, _ := p705Rig(t, h)
	projectB := uuid.MustParse(createPhaseProject(t, h))
	typeID := p709DefaultTypeID(t, h, "ARCHITECTURE")

	// T7.14.1: created for project A.
	fv, err := svc.CreateFile(ctx, service.CreateFileInput{
		ProjectID: projectA.String(),
		TypeID:    typeID.String(),
		Scope:     "PROJECT",
	})
	if err != nil {
		t.Fatalf("create architecture: %v", err)
	}
	fileA := uuid.MustParse(fv.ID)

	// T7.14.2 + T7.14.4: two controlled updates append versions and the
	// content hash changes after the edit.
	v1, err := svc.UpdateContent(ctx, fileA.String(), "decisions v1", 0)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := svc.UpdateContent(ctx, fileA.String(), "decisions v2", 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 version = %d, want 2", v2.Version)
	}
	if v1.ContentHash == v2.ContentHash {
		t.Fatal("content hash unchanged after edit")
	}

	// T7.14.3: historical v1 retained.
	hist, err := svc.GetVersion(ctx, fileA.String(), 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if hist.Content != "decisions v1" {
		t.Fatalf("historical v1 = %q", hist.Content)
	}

	// T7.14.5: project isolation — project B creates its own file of the
	// same type; the two files are distinct rows and A's content is
	// untouched by B's update.
	fvB, err := svc.CreateFile(ctx, service.CreateFileInput{
		ProjectID: projectB.String(),
		TypeID:    typeID.String(),
		Scope:     "PROJECT",
	})
	if err != nil {
		t.Fatalf("create B architecture: %v", err)
	}
	fileB := uuid.MustParse(fvB.ID)
	if fileB == fileA {
		t.Fatal("projects A and B share one architecture file")
	}
	if _, err := svc.UpdateContent(ctx, fileB.String(), "project B decisions", 0); err != nil {
		t.Fatalf("B v1: %v", err)
	}
	listA, err := dbrepo.NewStateFileRepository(h.App.DB().Raw()).ListByProject(ctx, projectA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	for _, r := range listA {
		if r.TypeID == typeID && r.ID != fileA {
			t.Fatalf("project A lists foreign architecture file %s", r.ID)
		}
	}
	again, err := svc.GetVersion(ctx, fileA.String(), 1)
	if err != nil {
		t.Fatalf("re-read A v1: %v", err)
	}
	if again.Content != "decisions v1" {
		t.Fatalf("A v1 after B update = %q", again.Content)
	}
}

// T7.15.1-T7.15.4 — test state: associated correctly (the default
// TEST_STATE type is PHASE-scoped, so the file binds to a phase), failure
// information retained, updates create versions, previous results
// reconstructable from history.
func TestP709TestStateLifecycle(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, phaseID, _ := p705Rig(t, h)
	typeID := p709DefaultTypeID(t, h, "TEST_STATE")

	fv, err := svc.CreateFile(ctx, service.CreateFileInput{
		ProjectID: projectID.String(),
		TypeID:    typeID.String(),
		Scope:     "PHASE",
		PhaseID:   ptrStr(phaseID.String()),
	})
	if err != nil {
		t.Fatalf("create test state: %v", err)
	}
	fileID := uuid.MustParse(fv.ID)

	// T7.15.1: associated to the phase (row + list membership).
	var storedPhase string
	if err := h.DBScan(ctx, "SELECT phase_id::text FROM state_files WHERE id = '"+fileID.String()+"'", &storedPhase); err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	if storedPhase != phaseID.String() {
		t.Fatalf("stored phase = %s, want %s", storedPhase, phaseID)
	}
	recs, err := dbrepo.NewStateFileRepository(h.App.DB().Raw()).ListByPhase(ctx, phaseID)
	if err != nil {
		t.Fatalf("list by phase: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.ID == fileID {
			found = true
		}
	}
	if !found {
		t.Fatal("test state file missing from phase listing")
	}

	// T7.15.2: failure information is retained in content — v1 carries a
	// failure summary and stays verbatim in history after later updates.
	failure := "failing tests: TestP709; summary: assertion failed"
	if _, err := svc.UpdateContent(ctx, fileID.String(), "passing: 3\nfailing: 1\n"+failure, 0); err != nil {
		t.Fatalf("v1: %v", err)
	}

	// T7.15.3: the update creates version 2.
	v2, err := svc.UpdateContent(ctx, fileID.String(), "passing: 4\nfailing: 0", 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 version = %d, want 2", v2.Version)
	}

	// T7.15.4: previous results reconstructable from history.
	prev, err := svc.GetVersion(ctx, fileID.String(), 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if prev.Content != "passing: 3\nfailing: 1\n"+failure {
		t.Fatalf("reconstructed v1 = %q", prev.Content)
	}
	if !strings.Contains(prev.Content, "summary: assertion failed") {
		t.Fatal("failure information lost in historical version")
	}
	if got := p706Current(t, h, fileID); got != 2 {
		t.Fatalf("current_version = %d, want 2", got)
	}
}
