package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/service"
)

// newPhaseServiceP406 wires the real PhaseService to the real repositories AND
// the transaction pool (WBS 4.6: deletion must handle associated WBS data
// transactionally, so the service needs the pool seam).
func newPhaseServiceP406(t *testing.T, h *Harness) *service.PhaseService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	)
}

// seedWBSDocument inserts a WBS document row directly (setup only).
func seedWBSDocument(t *testing.T, h *Harness, phaseID string, content string) {
	t.Helper()
	if _, err := h.DBExec(context.Background(),
		"INSERT INTO phase_documents (id, phase_id, doc_type, content, version) VALUES (gen_random_uuid(), '"+
			phaseID+"', 'WBS', '"+content+"', 1)"); err != nil {
		t.Fatalf("seed WBS document: %v", err)
	}
}

// T4.6.1 — Unused pending phase deletable: a PENDING phase that was never
// planned (no execution plan) is deletable; the phase row is gone and, when
// the phase had an unexecuted WBS document, that row is gone with it — no
// orphans.
func TestT461UnusedPendingPhaseDeletable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Throwaway", "never planned")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	seedWBSDocument(t, h, p.ID.String(), "draft content")

	if err := svc.DeletePhase(ctx, projectID, p.ID.String()); err != nil {
		t.Fatalf("DeletePhase on unused PENDING phase: %v", err)
	}
	var n int32
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE id = '"+p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count phase: %v", err)
	}
	if n != 0 {
		t.Fatalf("phase row still present after delete")
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM phase_documents WHERE phase_id = '"+p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned WBS document rows after delete")
	}
}

// T4.6.2 — Executing phase not deletable: an EXECUTING phase is rejected
// with a defined service error and the row is unchanged.
func TestT462ExecutingPhaseNotDeletable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Active", "in flight")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	setPhaseStatus(t, h, p.ID.String(), phase.StatusEXECUTING)
	beforeName, beforeDesc, beforeOrder := phaseRow(t, h, p.ID.String())

	err = svc.DeletePhase(ctx, projectID, p.ID.String())
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeletePhase on EXECUTING = %v, want ErrConflict", err)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != beforeName || description != beforeDesc || order != beforeOrder {
		t.Fatalf("row changed: (%q %q %d) -> (%q %q %d)", beforeName, beforeDesc, beforeOrder, name, description, order)
	}
}

// T4.6.3 — Done phase not deletable: a DONE phase is rejected with a defined
// service error and the row is unchanged.
func TestT463DonePhaseNotDeletable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Shipped", "finished")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	setPhaseStatus(t, h, p.ID.String(), phase.StatusDONE)
	beforeName, beforeDesc, beforeOrder := phaseRow(t, h, p.ID.String())

	err = svc.DeletePhase(ctx, projectID, p.ID.String())
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeletePhase on DONE = %v, want ErrConflict", err)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != beforeName || description != beforeDesc || order != beforeOrder {
		t.Fatalf("row changed: (%q %q %d) -> (%q %q %d)", beforeName, beforeDesc, beforeOrder, name, description, order)
	}
}

// T4.6.4 — Phase with execution plan not deletable: a phase that has an
// execution plan is rejected with a defined service error, and neither the
// phase row nor the plan row is touched.
func TestT464PhaseWithExecutionPlanNotDeletable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Planned", "has a plan")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	planID := uuid.New()
	if _, err := h.DBExec(ctx,
		"INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('"+
			planID.String()+"', '"+p.ID.String()+"', 1, 1, 'READY')"); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	beforeName, beforeDesc, beforeOrder := phaseRow(t, h, p.ID.String())

	err = svc.DeletePhase(ctx, projectID, p.ID.String())
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeletePhase with plan = %v, want ErrConflict", err)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != beforeName || description != beforeDesc || order != beforeOrder {
		t.Fatalf("phase row changed: (%q %q %d) -> (%q %q %d)", beforeName, beforeDesc, beforeOrder, name, description, order)
	}
	var n int32
	if err := h.DBScan(ctx, "SELECT count(*) FROM execution_plans WHERE id = '"+planID.String()+"'", &n); err != nil {
		t.Fatalf("count plan: %v", err)
	}
	if n != 1 {
		t.Fatalf("plan row changed after rejected delete")
	}
}

// T4.6.5 — Deletion doesn't renumber: deleting a middle phase leaves every
// remaining phase's order value untouched (gaps are tolerated, WBS 4.4).
func TestT465DeletionDoesNotRenumber(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	var phases []*phase.Phase
	for _, name := range []string{"one", "two", "three"} {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase: %v", err)
		}
		phases = append(phases, p)
	}

	if err := svc.DeletePhase(ctx, projectID, phases[1].ID.String()); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}
	if _, _, order := phaseRow(t, h, phases[0].ID.String()); order != phases[0].Order {
		t.Fatalf("first phase renumbered: %d", order)
	}
	if _, _, order := phaseRow(t, h, phases[2].ID.String()); order != phases[2].Order {
		t.Fatalf("third phase renumbered: %d", order)
	}
}

// T4.6.6 — Associated unexecuted WBS data handled transactionally: a
// successful deletion removes the phase and its unexecuted WBS document
// together (no orphan rows); a rejected deletion removes nothing.
func TestT466UnexecutedWBSDataTransactional(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP406(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Draft", "unexecuted wbs")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	seedWBSDocument(t, h, p.ID.String(), "unexecuted draft")

	if err := svc.DeletePhase(ctx, projectID, p.ID.String()); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}
	var n int32
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE id = '"+p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count phase: %v", err)
	}
	if n != 0 {
		t.Fatalf("phase row present after delete")
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM phase_documents WHERE phase_id = '"+p.ID.String()+"'", &n); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned document row after delete")
	}

	// Rejection path: nothing is removed.
	q, err := svc.CreatePhase(ctx, projectID, "Queued", "blocked")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	seedWBSDocument(t, h, q.ID.String(), "blocked draft")
	if _, err := h.DBExec(ctx,
		"INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES (gen_random_uuid(), '"+
			q.ID.String()+"', 1, 1, 'READY')"); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if err := svc.DeletePhase(ctx, projectID, q.ID.String()); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeletePhase with plan = %v, want ErrConflict", err)
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE id = '"+q.ID.String()+"'", &n); err != nil {
		t.Fatalf("count phase: %v", err)
	}
	if n != 1 {
		t.Fatalf("rejected delete removed the phase row")
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM phase_documents WHERE phase_id = '"+q.ID.String()+"'", &n); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 1 {
		t.Fatalf("rejected delete removed the document row")
	}
}
