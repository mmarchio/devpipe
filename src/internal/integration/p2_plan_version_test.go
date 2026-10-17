package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/wbs"
)

// planVersionRig bundles a harness with the repositories P2-20 exercises.
type planVersionRig struct {
	h     *Harness
	docs  *dbrepo.DocumentRepository
	plans *dbrepo.PlanRepository
	units *dbrepo.WorkUnitRepository
}

func newPlanVersionRig(t *testing.T) *planVersionRig {
	t.Helper()
	h := NewHarness(t)
	raw := h.App.DB().Raw()
	return &planVersionRig{
		h:     h,
		docs:  dbrepo.NewDocumentRepository(raw),
		plans: dbrepo.NewPlanRepository(raw),
		units: dbrepo.NewWorkUnitRepository(raw),
	}
}

// seed builds a project, a phase, a WBS document, and returns the IDs.
func (r *planVersionRig) seed(t *testing.T) (projectID, phaseID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	projectID = uuid.New()
	if _, err := r.h.DBExec(ctx, insertProjectSQL(projectID, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	phaseID = uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + projectID.String() + "', 1, 'Phase', 'PENDING')"
	if _, err := r.h.DBExec(ctx, sql); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if _, err := r.docs.Create(ctx, phaseID.String(), "WBS", "original scope"); err != nil {
		t.Fatalf("doc: %v", err)
	}
	return projectID, phaseID
}

func (r *planVersionRig) createPlan(t *testing.T, phaseID uuid.UUID, wbsVersion, version int) *wbs.ExecutionPlan {
	t.Helper()
	p, err := wbs.NewExecutionPlan(phaseID, wbsVersion)
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	p.Version = version
	if err := r.plans.Create(context.Background(), p); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

// T2.20.1 — WBS update increments version.
func TestT2201WBSUpdateIncrementsVersion(t *testing.T) {
	r := newPlanVersionRig(t)
	ctx := context.Background()
	_, phaseID := r.seed(t)
	d1, err := r.docs.GetByPhase(ctx, phaseID.String())
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if d1.Version != 1 {
		t.Fatalf("initial version = %d, want 1", d1.Version)
	}
	if err := r.docs.UpdateContent(ctx, phaseID.String(), "revised scope"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	d2, err := r.docs.GetByPhase(ctx, phaseID.String())
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if d2.Version != 2 {
		t.Fatalf("version after update = %d, want 2", d2.Version)
	}
	if d2.Content != "revised scope" {
		t.Fatalf("content = %q, want the revision", d2.Content)
	}
}

// T2.20.2 — Existing plan retains original WBS version after WBS update.
func TestT2202ExistingPlanRetainsWBSVersion(t *testing.T) {
	r := newPlanVersionRig(t)
	ctx := context.Background()
	_, phaseID := r.seed(t)
	p1 := r.createPlan(t, phaseID, 1, 1)
	if err := r.docs.UpdateContent(ctx, phaseID.String(), "revised scope"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	got, err := r.plans.Get(ctx, p1.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.WBSVersion != 1 {
		t.Fatalf("plan WBSVersion = %d, want 1 (historical plans are immutable)", got.WBSVersion)
	}
	if got.Version != 1 {
		t.Fatalf("plan version = %d, want 1", got.Version)
	}
}

// T2.20.3 — Historical plan remains retrievable after a WBS update and a
// successor plan exists.
func TestT2203HistoricalPlanRetrievable(t *testing.T) {
	r := newPlanVersionRig(t)
	ctx := context.Background()
	_, phaseID := r.seed(t)
	p1 := r.createPlan(t, phaseID, 1, 1)
	if err := r.docs.UpdateContent(ctx, phaseID.String(), "revised scope"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	r.createPlan(t, phaseID, 2, 2)
	got, err := r.plans.Get(ctx, p1.ID)
	if err != nil {
		t.Fatalf("historical plan not retrievable: %v", err)
	}
	if got.ID != p1.ID || got.WBSVersion != 1 {
		t.Fatalf("historical plan = %+v, want v1 on WBS v1", got)
	}
}

// T2.20.4 — New plan may reference the new WBS version.
func TestT2204NewPlanReferencesNewWBSVersion(t *testing.T) {
	r := newPlanVersionRig(t)
	ctx := context.Background()
	_, phaseID := r.seed(t)
	r.createPlan(t, phaseID, 1, 1)
	if err := r.docs.UpdateContent(ctx, phaseID.String(), "revised scope"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	p2, err := r.docs.GetByPhase(ctx, phaseID.String())
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	p := r.createPlan(t, phaseID, p2.Version, 2)
	if p.WBSVersion != 2 {
		t.Fatalf("new plan WBSVersion = %d, want 2", p.WBSVersion)
	}
	got, err := r.plans.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.WBSVersion != 2 || got.Version != 2 {
		t.Fatalf("plan = WBSVersion %d version %d, want 2/2", got.WBSVersion, got.Version)
	}
}

// T2.20.5 — Updating WBS doesn't delete completed work history.
func TestT2205WorkHistorySurvivesWBSUpdate(t *testing.T) {
	r := newPlanVersionRig(t)
	ctx := context.Background()
	_, phaseID := r.seed(t)
	p1 := r.createPlan(t, phaseID, 1, 1)
	u, err := wbs.NewWorkUnit(p1.ID, 1, "shipped work", wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit: %v", err)
	}
	if err := r.units.Create(ctx, u); err != nil {
		t.Fatalf("create unit: %v", err)
	}
	if err := r.units.MarkDone(ctx, u.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	// The WBS changes and a successor plan exists.
	if err := r.docs.UpdateContent(ctx, phaseID.String(), "revised scope"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	r.createPlan(t, phaseID, 2, 2)
	got, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("completed work history lost: %v", err)
	}
	if got.Status != wbs.WorkUnitStatusDONE || got.Name != "shipped work" {
		t.Fatalf("work unit = %+v, want the original DONE unit", got)
	}
}
