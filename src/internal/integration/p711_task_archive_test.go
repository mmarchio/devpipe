package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-11 (WBS 7.13): task-archive derivation. The TaskArchiveService derives
// the project's TASK_ARCHIVE content from its COMPLETED (status DONE) work
// units and writes it through the P7-08 versioned content path (the
// DERIVED source class's only writer), so the DB work-unit records stay
// authoritative (T7.13.5) and the derived content never feeds back into
// unit status.
//
// Ordering rule (stated here and in the statefile renderer tests):
// ascending completed_at, tie-broken by (sequence, id).
//
// File semantics (stated per the task NOTES): the TASK_ARCHIVE global
// default (P7-07) is PROJECT-scoped — one archive file per project,
// created on first derivation and reused on every rebuild (T7.13.4's
// rebuild appends a version to the SAME file; the content is
// byte-identical).

// p711Service wires the TaskArchiveService against the harness pool: the
// P7-08 CRUD service (the derivation's write path) plus the phase/plan/unit
// repositories the project's completed units are read through.
func p711Service(t *testing.T, h *Harness) *service.TaskArchiveService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewTaskArchiveService(
		p708Service(t, h),
		dbrepo.NewPhaseRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
	)
}

// p711ArchiveFile returns the id of the project's TASK_ARCHIVE file (type,
// PROJECT, no associations) after a derivation has created it.
func p711ArchiveFile(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	typeID := p709DefaultTypeID(t, h, "TASK_ARCHIVE")
	var fileID uuid.UUID
	if err := h.DBScan(context.Background(),
		"SELECT id FROM state_files WHERE project_id = '"+projectID.String()+
			"' AND type_id = '"+typeID.String()+"' AND scope = 'PROJECT'", &fileID); err != nil {
		t.Fatalf("scan archive file: %v", err)
	}
	return fileID
}

// T7.13.1/T7.13.2/T7.13.3 — the derived archive contains every completed
// (DONE) unit with its completion data, excludes the pending (non-DONE)
// units, and orders the entries by ascending completed_at (tie-broken by
// sequence).
func TestP711TaskArchiveDerivation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p711Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)

	// Three completed units with DELIBERATELY distinct completion times:
	// unit c2 completes first, c1 last — the rendered order must follow the
	// completion times, not the sequence (which would put c1 first).
	c1 := p710UnitWithSpec(t, h, planID, 1, "p711-c1", "PENDING", nil, nil, nil)
	c2 := p710UnitWithSpec(t, h, planID, 2, "p711-c2", "PENDING", nil, nil, nil)
	c3 := p710UnitWithSpec(t, h, planID, 3, "p711-c3", "PENDING", nil, nil, nil)
	pending := p710UnitWithSpec(t, h, planID, 4, "p711-pending", "PENDING", nil, nil, nil)
	markTime := func(id uuid.UUID, ts time.Time) {
		if _, err := h.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at='"+ts.Format("2006-01-02 15:04:05")+"' WHERE id='"+id.String()+"'"); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	// Completion times deliberately differ from sequence order: c2
	// completes first, c1 last — the rendered order must follow the
	// completion times, not the sequence.
	markTime(c2, base)
	markTime(c3, base.Add(time.Minute))
	markTime(c1, base.Add(2*time.Minute))

	v, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	fileID := p711ArchiveFile(t, h, projectID)
	if v.StateFileID != fileID.String() {
		t.Fatalf("derived file %s, want the project TASK_ARCHIVE file %s", v.StateFileID, fileID)
	}
	// T7.13.1: every completed unit appears with its completion data.
	for _, u := range []uuid.UUID{c1, c2, c3} {
		if !strings.Contains(v.Content, u.String()) {
			t.Fatalf("archive missing completed unit %s:\n%s", u, v.Content)
		}
	}
	// T7.13.2: the pending unit is excluded.
	if strings.Contains(v.Content, pending.String()) || strings.Contains(v.Content, "p711-pending") {
		t.Fatalf("archive includes a non-DONE unit:\n%s", v.Content)
	}
	// T7.13.3: the entry order follows completion time, not sequence.
	iC1 := strings.Index(v.Content, c1.String())
	iC2 := strings.Index(v.Content, c2.String())
	iC3 := strings.Index(v.Content, c3.String())
	if iC2 < 0 || iC3 < 0 || iC1 < 0 || iC3 < iC2 || iC1 < iC3 {
		t.Fatalf("archive entries not in completed_at order (want c2, c3, c1; got c2=%d c3=%d c1=%d):\n%s", iC2, iC3, iC1, v.Content)
	}
}

// T7.13.4 — rebuilding the archive (re-deriving with no unit changes)
// produces byte-identical content: the same project-scoped file is reused
// and the new version's content equals the previous one.
func TestP711TaskArchiveRebuildStable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p711Service(t, h)
	crud := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	u := p710UnitWithSpec(t, h, planID, 1, "p711-rebuild", "PENDING", nil, nil, nil)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at=now() WHERE id='"+u.String()+"'"); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	v1, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive 1: %v", err)
	}
	v2, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if v1.StateFileID != v2.StateFileID {
		t.Fatalf("rebuild bound a different file (%s -> %s); the TASK_ARCHIVE file is project-scoped and reused", v1.StateFileID, v2.StateFileID)
	}
	if v1.Content != v2.Content {
		t.Fatalf("rebuild not byte-identical:\n%s\n---\n%s", v1.Content, v2.Content)
	}
	if v2.Version <= v1.Version {
		t.Fatalf("rebuild version %d did not advance past %d (history must be retained)", v2.Version, v1.Version)
	}
	prev, err := crud.GetVersion(ctx, v1.StateFileID, v1.Version)
	if err != nil {
		t.Fatalf("read v1 history: %v", err)
	}
	if prev.Content != v1.Content {
		t.Fatalf("historical v1 content changed after rebuild")
	}
}

// T7.13.5 — the historical database records remain authoritative:
// (a) a hand edit of the materialized archive content appends a version but
// changes nothing in the DB; (b) re-deriving restores the unit-derived
// content; (c) a direct-SQL status flip feeds the next derivation (the
// derived content never writes back into the unit row — the work-unit row
// is byte-identical around a derivation, as pinned for the ACTIVE_TASK
// derivation in P7-10 and asserted here on the full row signature).
func TestP711TaskArchiveDBAuthoritative(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p711Service(t, h)
	crud := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	u := p710UnitWithSpec(t, h, planID, 1, "p711-auth", "PENDING", nil, nil, nil)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at=now() WHERE id='"+u.String()+"'"); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	v1, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	// (a) hand edit of the materialized content: nothing in the DB changes.
	if _, err := crud.UpdateContent(ctx, v1.StateFileID, "hand-edited archive content", v1.Version); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	sigAfterEdit := p710UnitSignature(t, h, u)

	// (b) re-derivation restores the unit-derived content.
	v2, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if strings.Contains(v2.Content, "hand-edited") || !strings.Contains(v2.Content, u.String()) {
		t.Fatalf("re-derivation did not restore the unit-derived content:\n%s", v2.Content)
	}

	// (c) the derivation is read-only over the unit row: the full
	// work_units signature is byte-identical around it.
	sigAfterDerive := p710UnitSignature(t, h, u)
	if sigAfterEdit != sigAfterDerive {
		t.Fatalf("derivation altered the work-unit row:\nbefore: %s\nafter:  %s", sigAfterEdit, sigAfterDerive)
	}

	// (d) the DB record is the history source: a direct-SQL status flip is
	// what the next derivation reflects (removal from the archive).
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='PENDING', completed_at=NULL WHERE id='"+u.String()+"'"); err != nil {
		t.Fatalf("un-done: %v", err)
	}
	v3, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive after un-done: %v", err)
	}
	if strings.Contains(v3.Content, u.String()) {
		t.Fatalf("archive reflects the materialized content, not the DB record (unit reappeared after its row was un-done):\n%s", v3.Content)
	}
}

// A project with no completed units (the p705Rig unit is PENDING) derives
// the stable empty-archive document — one project-scoped file, version 1,
// header-only content.
func TestP711TaskArchiveNoCompletedUnits(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p711Service(t, h)
	projectID, _, _ := p705Rig(t, h)

	v, err := svc.DeriveTaskArchive(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if strings.Contains(v.Content, "Status: ") {
		t.Fatalf("archive with no completed units carries entries:\n%s", v.Content)
	}
	fileID := p711ArchiveFile(t, h, projectID)
	if v.StateFileID != fileID.String() {
		t.Fatalf("derived file %s, want the project TASK_ARCHIVE file %s", v.StateFileID, fileID)
	}
}
