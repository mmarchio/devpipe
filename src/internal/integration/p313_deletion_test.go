package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p313Rig struct {
	h    *Harness
	proj *service.ProjectService
	p    *project.Project
}

func newP313Rig(t *testing.T) *p313Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "deletion project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p313Rig{h: h, proj: projSvc, p: p}
}

func (r *p313Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func (r *p313Rig) exists(t *testing.T) bool {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT COUNT(*) FROM projects WHERE id = '"+r.p.ID.String()+"'", &n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n == 1
}

func (r *p313Rig) seedExecutedHistory(t *testing.T) {
	t.Helper()
	r.addPhaseWithStatusReuse(t, "DONE")
}

func (r *p313Rig) addPhaseWithStatusReuse(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	phases := dbrepo.NewPhaseRepository(r.h.App.DB().Raw())
	ph, err := phases.Append(ctx, r.p.ID, "executed phase", "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if status == "DONE" {
		if err := phases.MarkCompleted(ctx, ph.ID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
	docs := dbrepo.NewDocumentRepository(r.h.App.DB().Raw())
	if _, err := docs.Create(ctx, ph.ID.String(), "WBS", "executed wbs"); err != nil {
		t.Fatalf("doc: %v", err)
	}
}

func (r *p313Rig) hierarchyCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT (SELECT COUNT(*) FROM phases WHERE project_id = '"+r.p.ID.String()+"') + (SELECT COUNT(*) FROM phase_documents WHERE phase_id IN (SELECT id FROM phases WHERE project_id = '"+r.p.ID.String()+"'))", &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// T3.13.1 — DRAFT project deletable.
func TestT3131DraftDeletable(t *testing.T) {
	r := newP313Rig(t)
	if _, err := r.proj.DeleteProject(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if r.exists(t) {
		t.Fatal("project still exists after delete")
	}
}

// T3.13.2 — READY project deletable.
func TestT3132ReadyDeletable(t *testing.T) {
	r := newP313Rig(t)
	r.setStatus(t, "READY")
	if _, err := r.proj.DeleteProject(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if r.exists(t) {
		t.Fatal("project still exists after delete")
	}
}

// CANCELLED projects are deletable (WBS 3.13 "optionally deletable").
func TestT313xCancelledDeletable(t *testing.T) {
	r := newP313Rig(t)
	r.setStatus(t, "CANCELLED")
	if _, err := r.proj.DeleteProject(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if r.exists(t) {
		t.Fatal("project still exists after delete")
	}
}

// T3.13.3 — RUNNING deletion rejected.
func TestT3133RunningRejected(t *testing.T) {
	r := newP313Rig(t)
	r.setStatus(t, "RUNNING")
	_, err := r.proj.DeleteProject(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if !r.exists(t) {
		t.Fatal("running project was deleted")
	}
}

// T3.13.4 — PAUSED deletion rejected.
func TestT3134PausedRejected(t *testing.T) {
	r := newP313Rig(t)
	r.setStatus(t, "PAUSED")
	_, err := r.proj.DeleteProject(context.Background(), r.p.ID.String())
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if !r.exists(t) {
		t.Fatal("paused project was deleted")
	}
}

// T3.13.5 — Terminal projects (COMPLETED, FAILED) are deletable and the
// delete cascades to the whole project hierarchy.
func TestT3135TerminalDeletableCascades(t *testing.T) {
	for _, s := range []string{"COMPLETED", "FAILED"} {
		t.Run(s, func(t *testing.T) {
			r := newP313Rig(t)
			r.seedExecutedHistory(t)
			if before := r.hierarchyCount(t); before < 2 {
				t.Fatalf("seed hierarchy = %d, want >= 2 (phase + wbs)", before)
			}
			r.setStatus(t, s)
			if _, err := r.proj.DeleteProject(context.Background(), r.p.ID.String()); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if r.exists(t) {
				t.Fatal("project still exists after delete")
			}
			if after := r.hierarchyCount(t); after != 0 {
				t.Fatalf("hierarchy left behind: %d rows", after)
			}
		})
	}
}
