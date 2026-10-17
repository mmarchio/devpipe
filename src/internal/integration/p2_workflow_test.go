package integration

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// fullWorkflowRig bundles the harness with the repositories the WBS 2.26
// full-workflow test needs.
type fullWorkflowRig struct {
	h      *Harness
	proj   *dbrepo.ProjectRepository
	phases *dbrepo.PhaseRepository
	docs   *dbrepo.DocumentRepository
	plans  *dbrepo.PlanRepository
	units  *dbrepo.WorkUnitRepository
}

func newFullWorkflowRig(t *testing.T) *fullWorkflowRig {
	t.Helper()
	h := NewHarness(t)
	raw := h.App.DB().Raw()
	return &fullWorkflowRig{
		h:      h,
		proj:   dbrepo.NewProjectRepository(raw),
		phases: dbrepo.NewPhaseRepository(raw),
		docs:   dbrepo.NewDocumentRepository(raw),
		plans:  dbrepo.NewPlanRepository(raw),
		units:  dbrepo.NewWorkUnitRepository(raw),
	}
}

func (r *fullWorkflowRig) newUnit(t *testing.T, planID uuid.UUID, seq int, name string) *wbs.WorkUnit {
	t.Helper()
	ctx := context.Background()
	wu, err := wbs.NewWorkUnit(planID, seq, name, wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit: %v", err)
	}
	spec, err := wbs.NewWorkUnitSpecification(wbs.WorkUnitSpecification{
		Objective:          "implement " + name,
		AcceptanceCriteria: []string{"accepts valid input"},
		RequiredTests:      []string{"valid case"},
		Constraints:        []string{"touch only the target code"},
	})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}
	wu.Specification = spec
	if err := r.units.Create(ctx, wu); err != nil {
		t.Fatalf("create unit %s: %v", name, err)
	}
	return wu
}

func eligibleIDs(units []*wbs.WorkUnit) map[string]bool {
	ids := map[string]bool{}
	for _, u := range units {
		ids[u.ID.String()] = true
	}
	return ids
}

// T2.26 — Core persistence integration test: the full workflow from the WBS
// executes end-to-end against real PostgreSQL.
//
//	Project: Example
//	Phase 1 (WBS v1), plan v1 with units A, B, C; dependencies A -> C, B -> C.
//
// verify (WBS 2.26, steps 1-11):
//  1. A and B initially eligible.
//  2. C initially ineligible.
//  3. Mark A DONE.
//  4. C remains ineligible.
//  5. Mark B DONE.
//  6. C becomes eligible.
//  7. Progress is 2/3 = 66.67%.
//  8. Mark C DONE.
//  9. Progress becomes 3/3 = 100%.
//
// 10. Mark Phase 1 DONE.
// 11. Next incomplete phase is returned.
func TestT226CorePersistenceWorkflow(t *testing.T) {
	r := newFullWorkflowRig(t)
	ctx := context.Background()

	// Create Project.
	p, err := project.New("Example", "")
	if err != nil {
		t.Fatalf("construct project: %v", err)
	}
	if err := r.proj.Create(ctx, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Create Settings.
	settings, err := project.NewSettings(p.ID, "/wd", "", "", "", "", 8192, 0.8, false, 0, time.Minute)
	if err != nil {
		t.Fatalf("construct settings: %v", err)
	}
	if _, err := r.h.DBExec(ctx, insertSettingsSQL(p.ID)); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if settings.ProjectID != p.ID {
		t.Fatalf("settings bound to %v, want project %v", settings.ProjectID, p.ID)
	}

	// Append Phase 1.
	p1, err := r.phases.Append(ctx, p.ID, "Phase 1", "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	if p1.Order != 1 {
		t.Fatalf("phase order %d, want 1", p1.Order)
	}

	// Attach WBS (WBSDocument v1 for the phase).
	doc, err := r.docs.Create(ctx, p1.ID.String(), "WBS", "# WBS v1\n\nPhase 1 scope")
	if err != nil {
		t.Fatalf("attach wbs: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("wbs version %d, want 1", doc.Version)
	}

	// Create Execution Plan (WBS v1, plan version 1).
	plan, err := wbs.NewExecutionPlan(p1.ID, 1)
	if err != nil {
		t.Fatalf("construct plan: %v", err)
	}
	if err := r.plans.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Create Work Units A, B, C.
	a := r.newUnit(t, plan.ID, 1, "A")
	b := r.newUnit(t, plan.ID, 2, "B")
	c := r.newUnit(t, plan.ID, 3, "C")

	// Create Dependencies: A -> C, B -> C.
	if err := r.units.AddDependency(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("dep a->c: %v", err)
	}
	if err := r.units.AddDependency(ctx, c.ID, b.ID); err != nil {
		t.Fatalf("dep b->c: %v", err)
	}

	// 1+2 — A and B initially eligible; C ineligible.
	elig, err := r.units.ListEligible(ctx, plan.ID)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	got := eligibleIDs(elig)
	if !got[a.ID.String()] || !got[b.ID.String()] || got[c.ID.String()] || len(elig) != 2 {
		t.Fatalf("initial eligible = %v, want exactly {A, B}", got)
	}

	// 3+4 — Mark A DONE; C remains ineligible (B still pending).
	if err := r.units.MarkDone(ctx, a.ID); err != nil {
		t.Fatalf("mark a: %v", err)
	}
	elig, err = r.units.ListEligible(ctx, plan.ID)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	got = eligibleIDs(elig)
	if got[c.ID.String()] || !got[b.ID.String()] || len(elig) != 1 {
		t.Fatalf("eligible after A done = %v, want exactly {B}", got)
	}

	// 5+6 — Mark B DONE; C becomes eligible.
	if err := r.units.MarkDone(ctx, b.ID); err != nil {
		t.Fatalf("mark b: %v", err)
	}
	elig, err = r.units.ListEligible(ctx, plan.ID)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(elig) != 1 || elig[0].ID != c.ID {
		t.Fatalf("eligible after B done = %v, want exactly {C}", eligibleIDs(elig))
	}

	// 7 — Progress is 2/3 = 66.67%.
	units, err := r.units.ListByPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	prog := wbs.CalculateWorkProgress(units)
	if prog.Total != 3 || prog.Completed != 2 || prog.Remaining != 1 {
		t.Fatalf("progress = %+v, want 2/3", prog)
	}
	if math.Abs(prog.Percentage-200.0/3.0) > 1e-9 {
		t.Fatalf("percentage = %v, want 66.67%%", prog.Percentage)
	}

	// 8+9 — Mark C DONE; progress becomes 3/3 = 100%.
	if err := r.units.MarkDone(ctx, c.ID); err != nil {
		t.Fatalf("mark c: %v", err)
	}
	units, err = r.units.ListByPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	prog = wbs.CalculateWorkProgress(units)
	if prog.Total != 3 || prog.Completed != 3 || prog.Remaining != 0 {
		t.Fatalf("progress = %+v, want 3/3", prog)
	}
	if prog.Percentage != 100 {
		t.Fatalf("percentage = %v, want 100%%", prog.Percentage)
	}

	// 10 — Mark Phase 1 DONE.
	if err := r.phases.MarkCompleted(ctx, p1.ID); err != nil {
		t.Fatalf("mark phase: %v", err)
	}
	gotPhase, err := r.phases.Get(ctx, p1.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if gotPhase.Status != phase.StatusDONE {
		t.Fatalf("phase status = %v, want DONE", gotPhase.Status)
	}

	// 11 — Next incomplete phase is returned (phase 2, appended after phase 1
	// is DONE).
	p2, err := r.phases.Append(ctx, p.ID, "Phase 2", "")
	if err != nil {
		t.Fatalf("append phase 2: %v", err)
	}
	next, err := r.phases.GetNextIncomplete(ctx, p.ID)
	if err != nil {
		t.Fatalf("next incomplete: %v", err)
	}
	if next == nil || next.ID != p2.ID {
		t.Fatalf("next incomplete = %+v, want phase 2", next)
	}
}
