package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// docRig bundles a harness, the document repository, and a known phase ID
// (plus its project ID).
type docRig struct {
	h       *Harness
	repo    *dbrepo.DocumentRepository
	phaseID string
	projID  string
}

func newDocRig(t *testing.T) *docRig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	projID := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(projID, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + projID.String() + "', 1, 'Phase', 'PENDING')"
	if _, err := h.DBExec(ctx, sql); err != nil {
		t.Fatalf("phase: %v", err)
	}
	return &docRig{h: h, repo: dbrepo.NewDocumentRepository(h.App.DB().Raw()), phaseID: phaseID.String(), projID: projID.String()}
}

// T2.10.1 — Document persisted.
func TestT2101DocumentPersisted(t *testing.T) {
	r := newDocRig(t)
	ctx := context.Background()
	doc, err := r.repo.Create(ctx, r.phaseID, "WBS", "# Scope\n\ncontent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.ID == "" {
		t.Fatal("ID not assigned")
	}
	if doc.Version != 1 {
		t.Fatalf("version %d, want 1", doc.Version)
	}
	got, err := r.repo.GetByPhase(ctx, r.phaseID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "# Scope\n\ncontent" || got.Version != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

// T2.10.2 — Document requires valid phase (FK violation).
func TestT2102DocumentRequiresValidPhase(t *testing.T) {
	r := newDocRig(t)
	_, err := r.repo.Create(context.Background(), uuid.New().String(), "WBS", "content")
	if !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("got %v, want ErrInvalidReference", err)
	}
}

// T2.10.3 — WBS retrieved by phase (other phases do not see it).
func TestT2103RetrievedByPhase(t *testing.T) {
	r := newDocRig(t)
	ctx := context.Background()
	if _, err := r.repo.Create(ctx, r.phaseID, "WBS", "v1 content"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second phase in the same project must not return this document.
	otherPhase := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		otherPhase.String() + "', '" + r.projID + "', 2, 'Phase2', 'PENDING')"
	if _, err := r.h.DBExec(ctx, sql); err != nil {
		t.Fatalf("phase2: %v", err)
	}
	if _, err := r.repo.GetByPhase(ctx, otherPhase.String()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound for phase without document, got %v", err)
	}
}

// T2.10.4 — Updating content persists (version increments).
func TestT2104UpdatePersists(t *testing.T) {
	r := newDocRig(t)
	ctx := context.Background()
	if _, err := r.repo.Create(ctx, r.phaseID, "WBS", "v1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.repo.UpdateContent(ctx, r.phaseID, "v2 content"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := r.repo.GetByPhase(ctx, r.phaseID)
	if got.Content != "v2 content" {
		t.Fatalf("content not persisted: %q", got.Content)
	}
	if got.Version != 2 {
		t.Fatalf("version %d, want 2", got.Version)
	}
}

// T2.10.5 — Deleting phase cascades documents.
func TestT2105PhaseDeleteCascadesDocuments(t *testing.T) {
	r := newDocRig(t)
	ctx := context.Background()
	if _, err := r.repo.Create(ctx, r.phaseID, "WBS", "content"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.h.DBExec(ctx, "DELETE FROM phases WHERE id = '"+r.phaseID+"'"); err != nil {
		t.Fatalf("delete phase: %v", err)
	}
	var count int
	if err := r.h.DBScan(ctx, "SELECT count(*) FROM phase_documents WHERE phase_id = '"+r.phaseID+"'", &count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("orphan documents after phase delete: %d", count)
	}
}
