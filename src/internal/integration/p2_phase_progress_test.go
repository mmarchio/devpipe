package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/orchestration"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// progressRig bundles a harness with the phase-progress service.
type progressRig struct {
	h   *Harness
	svc *orchestration.PhaseProgressService
}

func newProgressRig(t *testing.T) *progressRig {
	t.Helper()
	h := NewHarness(t)
	raw := h.App.DB().Raw()
	return &progressRig{
		h: h,
		svc: orchestration.NewPhaseProgressService(
			dbrepo.NewProjectRepository(raw),
			dbrepo.NewPhaseRepository(raw),
			dbrepo.NewPlanRepository(raw),
			dbrepo.NewWorkUnitRepository(raw),
		),
	}
}

func (r *progressRig) project(t *testing.T) uuid.UUID {
	t.Helper()
	pid := uuid.New()
	if _, err := r.h.DBExec(context.Background(), insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	return pid
}

// addPhase inserts a phase with the given order/status directly (order is
// normally assigned by Append; the tests need explicit control).
func (r *progressRig) addPhase(t *testing.T, projectID uuid.UUID, order int, name, status string) uuid.UUID {
	t.Helper()
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + projectID.String() + "', " + itoa(order) + ", '" + name + "', '" + status + "')"
	if _, err := r.h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("phase: %v", err)
	}
	return phaseID
}

// addPlan inserts an execution plan for a phase.
func (r *progressRig) addPlan(t *testing.T, phaseID uuid.UUID, wbsVersion, version int, status string) uuid.UUID {
	t.Helper()
	planID := uuid.New()
	sql := "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('" +
		planID.String() + "', '" + phaseID.String() + "', " + itoa(wbsVersion) + ", " + itoa(version) + ", '" + status + "')"
	if _, err := r.h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("plan: %v", err)
	}
	return planID
}

// addUnit inserts a work unit with the given status.
func (r *progressRig) addUnit(t *testing.T, planID uuid.UUID, seq int, name, status string) {
	t.Helper()
	u := uuid.New()
	sql := "INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES ('" +
		u.String() + "', '" + planID.String() + "', " + itoa(seq) + ", '" + name + "', '', 'ATOMIC', '" + status + "', '', '', '[]', '[]', '[]', now())"
	if _, err := r.h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("unit: %v", err)
	}
}

// T2.19.1 — Current phase identified by lowest non-DONE order.
func TestT2191CurrentPhase(t *testing.T) {
	r := newProgressRig(t)
	pid := r.project(t)
	_ = r.addPhase(t, pid, 1, "A", "DONE")
	_ = r.addPhase(t, pid, 2, "B", "EXECUTING")
	_ = r.addPhase(t, pid, 3, "C", "PENDING")
	prog, err := r.svc.CurrentPhaseProgress(context.Background(), pid)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.CurrentPhase == nil {
		t.Fatal("current phase nil")
	}
	if got := prog.CurrentPhase.Order; got != 2 {
		t.Fatalf("current phase order = %d, want 2 (lowest non-DONE)", got)
	}
	if prog.CurrentPhase.Name != "B" {
		t.Fatalf("current phase = %q, want B", prog.CurrentPhase.Name)
	}
}

// T2.19.2 — Active plan identified: highest-version non-SUPERSEDED plan of
// the current phase.
func TestT2192ActivePlan(t *testing.T) {
	r := newProgressRig(t)
	pid := r.project(t)
	ph := r.addPhase(t, pid, 1, "P1", "EXECUTING")
	_ = r.addPlan(t, ph, 1, 1, "SUPERSEDED")
	v2 := r.addPlan(t, ph, 2, 2, "EXECUTING")
	prog, err := r.svc.CurrentPhaseProgress(context.Background(), pid)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.ActivePlan == nil {
		t.Fatal("active plan nil, want the v2 plan")
	}
	if prog.ActivePlan.ID != v2 {
		t.Fatalf("active plan = %s, want v2 %s", prog.ActivePlan.ID, v2)
	}
}

// T2.19.3 — Progress counts correct: 1 of 4 DONE -> 25%.
func TestT2193ProgressCounts(t *testing.T) {
	r := newProgressRig(t)
	pid := r.project(t)
	ph := r.addPhase(t, pid, 1, "P1", "EXECUTING")
	plan := r.addPlan(t, ph, 1, 1, "READY")
	r.addUnit(t, plan, 1, "u1", "DONE")
	r.addUnit(t, plan, 2, "u2", "EXECUTING")
	r.addUnit(t, plan, 3, "u3", "PENDING")
	r.addUnit(t, plan, 4, "u4", "PENDING")
	prog, err := r.svc.CurrentPhaseProgress(context.Background(), pid)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.Progress.Total != 4 || prog.Progress.Completed != 1 || prog.Progress.Remaining != 3 {
		t.Fatalf("progress = %+v, want 1/4", prog.Progress)
	}
	if prog.Progress.Percentage != 25 {
		t.Fatalf("percentage = %v, want 25", prog.Progress.Percentage)
	}
}

// T2.19.4 — Current work unit returned where one exists: the first eligible
// (PENDING/READY with all dependencies DONE) unit in sequence order.
func TestT2194CurrentWorkUnit(t *testing.T) {
	r := newProgressRig(t)
	pid := r.project(t)
	ph := r.addPhase(t, pid, 1, "P1", "EXECUTING")
	plan := r.addPlan(t, ph, 1, 1, "READY")
	r.addUnit(t, plan, 1, "done-unit", "DONE")
	r.addUnit(t, plan, 2, "executing-unit", "EXECUTING")
	r.addUnit(t, plan, 3, "current-unit", "PENDING")
	prog, err := r.svc.CurrentPhaseProgress(context.Background(), pid)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.CurrentWorkUnit == nil {
		t.Fatal("current work unit nil, want the first eligible unit")
	}
	if prog.CurrentWorkUnit.Name != "current-unit" {
		t.Fatalf("current work unit = %q, want %q", prog.CurrentWorkUnit.Name, "current-unit")
	}
}

// T2.19.5 — Completed project produces no current phase.
func TestT2195CompletedProject(t *testing.T) {
	r := newProgressRig(t)
	pid := r.project(t)
	ph := r.addPhase(t, pid, 1, "P1", "DONE")
	plan := r.addPlan(t, ph, 1, 1, "READY")
	r.addUnit(t, plan, 1, "u1", "DONE")
	prog, err := r.svc.CurrentPhaseProgress(context.Background(), pid)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.CurrentPhase != nil {
		t.Fatalf("current phase = %+v, want nil for an all-DONE project", prog.CurrentPhase)
	}
	if prog.ActivePlan != nil || prog.CurrentWorkUnit != nil || prog.Progress != (wbs.WorkProgress{}) {
		t.Fatalf("progress for a completed project must be empty: %+v", prog)
	}
}

// A project with no phases also produces no current phase; an unknown project
// yields persistence.ErrNotFound.
func TestT2195EdgeCases(t *testing.T) {
	r := newProgressRig(t)
	ctx := context.Background()
	empty := r.project(t)
	prog, err := r.svc.CurrentPhaseProgress(ctx, empty)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if prog.CurrentPhase != nil {
		t.Fatalf("current phase = %+v, want nil for a project with no phases", prog.CurrentPhase)
	}
	if _, err := r.svc.CurrentPhaseProgress(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
}
