package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// tsRig bundles a harness with the repositories touched by the timestamp
// consistency tests (WBS 2.23).
type tsRig struct {
	h        *Harness
	projects *dbrepo.ProjectRepository
	phases   *dbrepo.PhaseRepository
	plans    *dbrepo.PlanRepository
	units    *dbrepo.WorkUnitRepository
	docs     *dbrepo.DocumentRepository
}

func newTsRig(t *testing.T) *tsRig {
	t.Helper()
	h := NewHarness(t)
	raw := h.App.DB().Raw()
	return &tsRig{
		h:        h,
		projects: dbrepo.NewProjectRepository(raw),
		phases:   dbrepo.NewPhaseRepository(raw),
		plans:    dbrepo.NewPlanRepository(raw),
		units:    dbrepo.NewWorkUnitRepository(raw),
		docs:     dbrepo.NewDocumentRepository(raw),
	}
}

func (r *tsRig) newProject(t *testing.T) *project.Project {
	t.Helper()
	ctx := context.Background()
	p, err := project.New("TS", "ts project")
	if err != nil {
		t.Fatalf("construct project: %v", err)
	}
	if err := r.projects.Create(ctx, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func (r *tsRig) newPhase(t *testing.T, projectID uuid.UUID) *phase.Phase {
	t.Helper()
	p, err := r.phases.Append(context.Background(), projectID, "Phase", "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	return p
}

func (r *tsRig) newPlan(t *testing.T, phaseID uuid.UUID) *wbs.ExecutionPlan {
	t.Helper()
	p, err := wbs.NewExecutionPlan(phaseID, 1)
	if err != nil {
		t.Fatalf("construct plan: %v", err)
	}
	if err := r.plans.Create(context.Background(), p); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func (r *tsRig) newUnit(t *testing.T, planID uuid.UUID, seq int) *wbs.WorkUnit {
	t.Helper()
	u, err := wbs.NewWorkUnit(planID, seq, "unit", wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("construct unit: %v", err)
	}
	if err := r.units.Create(context.Background(), u); err != nil {
		t.Fatalf("create unit: %v", err)
	}
	return u
}

func requireUTCTime(t *testing.T, label string, ts time.Time) {
	t.Helper()
	if ts.IsZero() {
		t.Fatalf("%s: zero time", label)
	}
	if name, offset := ts.Zone(); name != "UTC" || offset != 0 {
		t.Fatalf("%s: %v (zone %q, offset %d) is not normalized to UTC", label, ts, name, offset)
	}
}

// T2.23.1 — CreatedAt assigned.
func TestT2231CreatedAtAssigned(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	requireUTCTime(t, "project created (in-memory)", p.CreatedAt)

	ph := r.newPhase(t, p.ID)
	requireUTCTime(t, "phase created (in-memory)", ph.CreatedAt)
	got, err := r.phases.Get(ctx, ph.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	requireUTCTime(t, "phase created (stored)", got.CreatedAt)

	pl := r.newPlan(t, ph.ID)
	requireUTCTime(t, "plan created (in-memory)", pl.CreatedAt)

	u := r.newUnit(t, pl.ID, 1)
	requireUTCTime(t, "unit created (in-memory)", u.CreatedAt)
	gotU, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	requireUTCTime(t, "unit created (stored)", gotU.CreatedAt)
}

// T2.23.2 — UpdatedAt changes on mutation.
func TestT2232UpdatedAtChangesOnMutation(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	ph := r.newPhase(t, p.ID)
	pl := r.newPlan(t, ph.ID)
	u := r.newUnit(t, pl.ID, 1)

	before, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	u.Name = "renamed"
	if err := r.units.Update(ctx, u); err != nil {
		t.Fatalf("update unit: %v", err)
	}
	after, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit after update: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("work unit updated_at did not change on mutation: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}

	p.Name = "renamed project"
	if err := r.projects.Update(ctx, p); err != nil {
		t.Fatalf("update project: %v", err)
	}
	got, err := r.projects.Get(ctx, p.ID.String())
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("project updated_at not advanced by Update: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	if _, err := r.docs.Create(ctx, ph.ID.String(), "WBS", "doc"); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if err := r.docs.UpdateContent(ctx, ph.ID.String(), "changed"); err != nil {
		t.Fatalf("update doc: %v", err)
	}
	doc, err := r.docs.GetByPhase(ctx, ph.ID.String())
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if !doc.UpdatedAt.After(doc.CreatedAt) {
		t.Fatalf("document updated_at not advanced by UpdateContent: created=%v updated=%v", doc.CreatedAt, doc.UpdatedAt)
	}
}

// T2.23.3 — Stored timestamps normalized to UTC.
func TestT2233StoredTimestampsNormalizedToUTC(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	ph := r.newPhase(t, p.ID)
	pl := r.newPlan(t, ph.ID)
	u := r.newUnit(t, pl.ID, 1)
	if _, err := r.docs.Create(ctx, ph.ID.String(), "WBS", "doc"); err != nil {
		t.Fatalf("create doc: %v", err)
	}

	var c, uq time.Time
	if err := r.h.DBScan(ctx, "SELECT created_at, updated_at FROM projects WHERE id = '"+p.ID.String()+"'", &c, &uq); err != nil {
		t.Fatalf("scan projects: %v", err)
	}
	requireUTCTime(t, "projects.created_at", c)
	requireUTCTime(t, "projects.updated_at", uq)

	if err := r.h.DBScan(ctx, "SELECT created_at, updated_at FROM phases WHERE id = '"+ph.ID.String()+"'", &c, &uq); err != nil {
		t.Fatalf("scan phases: %v", err)
	}
	requireUTCTime(t, "phases.created_at", c)
	requireUTCTime(t, "phases.updated_at", uq)

	if err := r.h.DBScan(ctx, "SELECT created_at, updated_at FROM execution_plans WHERE id = '"+pl.ID.String()+"'", &c, &uq); err != nil {
		t.Fatalf("scan plans: %v", err)
	}
	requireUTCTime(t, "execution_plans.created_at", c)
	requireUTCTime(t, "execution_plans.updated_at", uq)

	if err := r.h.DBScan(ctx, "SELECT created_at FROM work_units WHERE id = '"+u.ID.String()+"'", &c); err != nil {
		t.Fatalf("scan units: %v", err)
	}
	requireUTCTime(t, "work_units.created_at", c)

	if err := r.h.DBScan(ctx, "SELECT created_at, updated_at FROM phase_documents WHERE phase_id = '"+ph.ID.String()+"'", &c, &uq); err != nil {
		t.Fatalf("scan documents: %v", err)
	}
	requireUTCTime(t, "phase_documents.created_at", c)
	requireUTCTime(t, "phase_documents.updated_at", uq)
}

// T2.23.4 — StartedAt initially null.
func TestT2234StartedAtInitiallyNull(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	ph := r.newPhase(t, p.ID)
	pl := r.newPlan(t, ph.ID)
	u := r.newUnit(t, pl.ID, 1)

	if ph.StartedAt != nil {
		t.Fatalf("phase started_at not null on creation: %v", *ph.StartedAt)
	}
	got, err := r.phases.Get(ctx, ph.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if got.StartedAt != nil {
		t.Fatalf("phase started_at not null in storage: %v", *got.StartedAt)
	}
	if u.StartedAt != nil {
		t.Fatalf("unit started_at not null on creation: %v", *u.StartedAt)
	}
	gotU, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	if gotU.StartedAt != nil {
		t.Fatalf("unit started_at not null in storage: %v", *gotU.StartedAt)
	}
}

// T2.23.5 — CompletedAt initially null.
func TestT2235CompletedAtInitiallyNull(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	ph := r.newPhase(t, p.ID)
	pl := r.newPlan(t, ph.ID)
	u := r.newUnit(t, pl.ID, 1)

	if ph.CompletedAt != nil {
		t.Fatalf("phase completed_at not null on creation: %v", *ph.CompletedAt)
	}
	got, err := r.phases.Get(ctx, ph.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if got.CompletedAt != nil {
		t.Fatalf("phase completed_at not null in storage: %v", *got.CompletedAt)
	}
	if u.CompletedAt != nil {
		t.Fatalf("unit completed_at not null on creation: %v", *u.CompletedAt)
	}
	gotU, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	if gotU.CompletedAt != nil {
		t.Fatalf("unit completed_at not null in storage: %v", *gotU.CompletedAt)
	}
}

// T2.23.6 — Completion records timestamp.
func TestT2236CompletionRecordsTimestamp(t *testing.T) {
	r := newTsRig(t)
	ctx := context.Background()
	p := r.newProject(t)
	ph := r.newPhase(t, p.ID)
	pl := r.newPlan(t, ph.ID)
	u := r.newUnit(t, pl.ID, 1)

	if err := r.phases.MarkCompleted(ctx, ph.ID); err != nil {
		t.Fatalf("mark phase completed: %v", err)
	}
	got, err := r.phases.Get(ctx, ph.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatal("phase completed_at nil after MarkCompleted")
	}
	requireUTCTime(t, "phase completed_at", *got.CompletedAt)
	if !got.CompletedAt.After(got.CreatedAt) {
		t.Fatalf("phase completed_at %v not after created_at %v", *got.CompletedAt, got.CreatedAt)
	}

	if err := r.units.MarkDone(ctx, u.ID); err != nil {
		t.Fatalf("mark unit done: %v", err)
	}
	gotU, err := r.units.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	if gotU.CompletedAt == nil {
		t.Fatal("unit completed_at nil after MarkDone")
	}
	requireUTCTime(t, "unit completed_at", *gotU.CompletedAt)
	if !gotU.CompletedAt.After(gotU.CreatedAt) {
		t.Fatalf("unit completed_at %v not after created_at %v", *gotU.CompletedAt, gotU.CreatedAt)
	}
}
