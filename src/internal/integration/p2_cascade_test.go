package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// cascadeRig bundles a harness with a fully seeded hierarchy:
// project -> settings, phase -> document, plan -> two units, one dependency
// edge. Each T2.22.x test deletes at one level and proves no orphans survive.
type cascadeRig struct {
	h       *Harness
	proj    *dbrepo.ProjectRepository
	ph      *dbrepo.PhaseRepository
	doc     *dbrepo.DocumentRepository
	plan    *dbrepo.PlanRepository
	wu      *dbrepo.WorkUnitRepository
	projID  uuid.UUID
	phaseID uuid.UUID
	planID  uuid.UUID
	unitA   uuid.UUID
	unitB   uuid.UUID
}

func newCascadeRig(t *testing.T) *cascadeRig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	r := &cascadeRig{
		h:    h,
		proj: dbrepo.NewProjectRepository(pool),
		ph:   dbrepo.NewPhaseRepository(pool),
		doc:  dbrepo.NewDocumentRepository(pool),
		plan: dbrepo.NewPlanRepository(pool),
		wu:   dbrepo.NewWorkUnitRepository(pool),
	}
	// project + settings
	p, err := project.New("Cascade project", "")
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	if err := r.proj.Create(ctx, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r.projID = p.ID
	if _, err := h.DBExec(ctx, insertSettingsSQL(r.projID)); err != nil {
		t.Fatalf("create settings: %v", err)
	}
	// phase + WBS document
	ph, err := r.ph.Append(ctx, r.projID, "Phase 1", "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	r.phaseID = ph.ID
	if _, err := r.doc.Create(ctx, ph.ID.String(), "WBS", "# WBS"); err != nil {
		t.Fatalf("create document: %v", err)
	}
	// plan + two units + one dependency edge
	plan, err := wbs.NewExecutionPlan(ph.ID, 1)
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	if err := r.plan.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	r.planID = plan.ID
	for i, name := range []string{"A", "B"} {
		u, err := wbs.NewWorkUnit(plan.ID, i+1, name, wbs.WorkUnitKindATOMIC)
		if err != nil {
			t.Fatalf("new unit %s: %v", name, err)
		}
		if err := r.wu.Create(ctx, u); err != nil {
			t.Fatalf("create unit %s: %v", name, err)
		}
		if i == 0 {
			r.unitA = u.ID
		} else {
			r.unitB = u.ID
		}
	}
	if err := r.wu.AddDependency(ctx, r.unitB, r.unitA); err != nil {
		t.Fatalf("add dependency: %v", err)
	}
	return r
}

func (r *cascadeRig) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM "+table, &n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// requireFullHierarchy asserts every level is present before a delete.
func (r *cascadeRig) requireFullHierarchy(t *testing.T) {
	t.Helper()
	for table, want := range map[string]int{
		"projects":               1,
		"project_settings":       1,
		"phases":                 1,
		"phase_documents":        1,
		"execution_plans":        1,
		"work_units":             2,
		"work_unit_dependencies": 1,
	} {
		if n := r.count(t, table); n != want {
			t.Fatalf("precondition: %s has %d rows, want %d", table, n, want)
		}
	}
}

// T2.22.1 — No orphan settings: deleting a project removes its settings.
func TestT2221NoOrphanSettings(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	if err := r.proj.Delete(ctx, r.projID.String()); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if n := r.count(t, "project_settings"); n != 0 {
		t.Fatalf("orphan settings after project delete: %d", n)
	}
}

// T2.22.2 — No orphan phases: deleting a project removes its phases, and a
// sibling project's phases survive (scoped cascade).
func TestT2222NoOrphanPhases(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	// A second, unrelated project with its own phase.
	p2, err := project.New("Sibling", "")
	if err != nil {
		t.Fatalf("new sibling: %v", err)
	}
	if err := r.proj.Create(ctx, p2); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	if _, err := r.ph.Append(ctx, p2.ID, "Phase 1", ""); err != nil {
		t.Fatalf("sibling phase: %v", err)
	}
	if err := r.proj.Delete(ctx, r.projID.String()); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if n := r.count(t, "phases"); n != 1 {
		t.Fatalf("phases after delete: %d, want 1 (only the sibling's)", n)
	}
	var remaining uuid.UUID
	if err := r.h.DBScan(ctx, "SELECT project_id FROM phases", &remaining); err != nil {
		t.Fatalf("scan remaining phase: %v", err)
	}
	if remaining != p2.ID {
		t.Fatalf("remaining phase belongs to %s, want sibling %s", remaining, p2.ID)
	}
}

// T2.22.3 — No orphan WBS documents: deleting a phase removes its documents.
func TestT2223NoOrphanWBSDocuments(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	if err := r.ph.Delete(ctx, r.phaseID); err != nil {
		t.Fatalf("delete phase: %v", err)
	}
	if n := r.count(t, "phase_documents"); n != 0 {
		t.Fatalf("orphan documents after phase delete: %d", n)
	}
}

// T2.22.4 — No orphan execution plans: deleting a phase removes its plans.
func TestT2224NoOrphanExecutionPlans(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	if err := r.ph.Delete(ctx, r.phaseID); err != nil {
		t.Fatalf("delete phase: %v", err)
	}
	if n := r.count(t, "execution_plans"); n != 0 {
		t.Fatalf("orphan plans after phase delete: %d", n)
	}
}

// T2.22.5 — No orphan work units: deleting a plan removes its units.
func TestT2225NoOrphanWorkUnits(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	if _, err := r.h.DBExec(ctx, "DELETE FROM execution_plans WHERE id = '"+r.planID.String()+"'"); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if n := r.count(t, "work_units"); n != 0 {
		t.Fatalf("orphan work units after plan delete: %d", n)
	}
}

// T2.22.6 — No orphan dependency edges: deleting a work unit removes its
// dependency edges (both directions).
func TestT2226NoOrphanDependencyEdges(t *testing.T) {
	r := newCascadeRig(t)
	ctx := context.Background()
	r.requireFullHierarchy(t)
	if _, err := r.h.DBExec(ctx, "DELETE FROM work_units WHERE id = '"+r.unitB.String()+"'"); err != nil {
		t.Fatalf("delete unit B: %v", err)
	}
	if n := r.count(t, "work_unit_dependencies"); n != 0 {
		t.Fatalf("orphan dependency edges after unit delete: %d", n)
	}
	// Delete the remaining unit as well: no edges can survive.
	if _, err := r.h.DBExec(ctx, "DELETE FROM work_units WHERE id = '"+r.unitA.String()+"'"); err != nil {
		t.Fatalf("delete unit A: %v", err)
	}
	if n := r.count(t, "work_unit_dependencies"); n != 0 {
		t.Fatalf("orphan dependency edges after all units deleted: %d", n)
	}
}
