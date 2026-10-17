package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// wbsNewUnit builds a work unit for a plan's sequence with a PENDING
// default status; callers override Status for the fixture they need.
func wbsNewUnit(t *testing.T, planID uuid.UUID, seq int, name string) (*wbs.WorkUnit, error) {
	t.Helper()
	return wbs.NewWorkUnit(planID, seq, name, wbs.WorkUnitKindATOMIC)
}

// P4-19 (WBS 4.18): the phase read model — a comprehensive per-phase
// representation combining the phase (order, status), its WBS (version +
// derived stale flag), its active execution plan version, and work progress
// (completed/total/percentage). It is the building block the P4-20 phase
// list consumes. Read-only; staleness and progress are always derived,
// never persisted.
//
// Representation choice (deliberate, per P4-16/P4-17 precedent): the WBS
// 4.18 "stale?" flag is defined ONLY against an active plan. A phase with a
// WBS but no plan is unplanned — WBS.Version present, WBS.Stale false,
// Plan nil, Progress 0/0/0. A phase with no WBS at all is pending-unplanned
// — WBS nil, Plan nil, Progress 0/0/0.

func newPhaseReadModelService(t *testing.T, h *Harness) *service.PhaseReadModelService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewPhaseReadModelService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	)
}

// T4.18.1 — Pending unplanned phase represented: a PENDING phase with no
// WBS document and no plan yields a fully defined shape: phase section with
// order + PENDING status, nil WBS section, nil plan section, and 0/0/0
// progress (nothing to count).
func TestT4181PendingUnplannedPhaseRepresented(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	if _, err := newPhaseService(t, h).CreatePhase(ctx, projectID, "bare", ""); err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	var phaseID string
	if err := h.DBScan(ctx, "SELECT id::text FROM phases WHERE project_id = '"+projectID+"'", &phaseID); err != nil {
		t.Fatal(err)
	}

	got, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel: %v", err)
	}
	if got.Phase == nil {
		t.Fatal("read model has no phase section")
	}
	if got.Phase.Order != 1 {
		t.Fatalf("phase order = %d, want 1", got.Phase.Order)
	}
	if got.Phase.Status != phase.StatusPENDING {
		t.Fatalf("phase status = %v, want PENDING", got.Phase.Status)
	}
	if got.WBS != nil {
		t.Fatalf("wbs section = %+v, want nil for an unplanned phase", got.WBS)
	}
	if got.Plan != nil {
		t.Fatalf("plan section = %+v, want nil for an unplanned phase", got.Plan)
	}
	if got.Progress.Completed != 0 || got.Progress.Total != 0 || got.Progress.Percentage != 0 {
		t.Fatalf("progress = %+v, want 0/0/0%% for an unplanned phase", got.Progress)
	}
}

// T4.18.2 — Planned phase represented: a phase with a WBS v1 and a plan
// bound to v1 reports the WBS version, a non-stale flag, the active plan
// version, and 0/0/0 progress (no work units exist yet).
func TestT4182PlannedPhaseRepresented(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docs.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := docs.CreatePlan(ctx, projectID, phaseID, 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	got, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel: %v", err)
	}
	if got.Phase == nil || got.Phase.Order != 1 {
		t.Fatalf("phase section = %+v, want order 1", got.Phase)
	}
	if got.WBS == nil {
		t.Fatal("read model has no wbs section")
	}
	if got.WBS.Version != 1 {
		t.Fatalf("wbs version = %d, want 1", got.WBS.Version)
	}
	if got.WBS.Stale {
		t.Fatal("wbs stale = true, want false (plan bound to the current WBS version)")
	}
	if got.Plan == nil || got.Plan.Version != 1 {
		t.Fatalf("plan section = %+v, want active plan version 1", got.Plan)
	}
	if got.Progress.Completed != 0 || got.Progress.Total != 0 || got.Progress.Percentage != 0 {
		t.Fatalf("progress = %+v, want 0/0/0%% with no work units", got.Progress)
	}
}

// T4.18.3 — Executing phase represented: an EXECUTING phase whose plan
// carries 1 DONE + 1 PENDING work unit reports order, EXECUTING status, the
// current WBS version (not stale), the plan version, and the derived
// 1/2/50% progress.
func TestT4183ExecutingPhaseRepresented(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docs.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := docs.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	units := dbrepo.NewWorkUnitRepository(h.App.DB().Raw())
	done, _ := wbsNewUnit(t, plan.ID, 1, "done")
	done.Status = wbs.WorkUnitStatusDONE
	if err := units.Create(ctx, done); err != nil {
		t.Fatalf("Create unit: %v", err)
	}
	pending, _ := wbsNewUnit(t, plan.ID, 2, "pending")
	if err := units.Create(ctx, pending); err != nil {
		t.Fatalf("Create unit: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'EXECUTING' WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatal(err)
	}

	got, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel: %v", err)
	}
	if got.Phase == nil || got.Phase.Order != 1 {
		t.Fatalf("phase section = %+v, want order 1", got.Phase)
	}
	if got.Phase.Status != phase.StatusEXECUTING {
		t.Fatalf("phase status = %v, want EXECUTING", got.Phase.Status)
	}
	if got.WBS == nil || got.WBS.Version != 1 {
		t.Fatalf("wbs section = %+v, want version 1", got.WBS)
	}
	if got.WBS.Stale {
		t.Fatal("wbs stale = true, want false")
	}
	if got.Plan == nil || got.Plan.Version != 1 {
		t.Fatalf("plan section = %+v, want version 1", got.Plan)
	}
	if got.Progress.Completed != 1 || got.Progress.Total != 2 {
		t.Fatalf("progress = %+v, want 1/2", got.Progress)
	}
	if got.Progress.Percentage != 50 {
		t.Fatalf("progress percentage = %v, want 50", got.Progress.Percentage)
	}
}

// T4.18.4 — Completed phase reports 100% where all units are DONE: a DONE
// phase whose plan has three DONE work units reports the DONE status and
// the derived 3/3/100% progress.
func TestT4184CompletedPhaseReports100Percent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docs.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := docs.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	units := dbrepo.NewWorkUnitRepository(h.App.DB().Raw())
	for i := 1; i <= 3; i++ {
		u, _ := wbsNewUnit(t, plan.ID, i, "u"+itoa(i))
		u.Status = wbs.WorkUnitStatusDONE
		if err := units.Create(ctx, u); err != nil {
			t.Fatalf("Create unit %d: %v", i, err)
		}
	}
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatal(err)
	}

	got, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel: %v", err)
	}
	if got.Phase == nil || got.Phase.Status != phase.StatusDONE {
		t.Fatalf("phase section = %+v, want DONE", got.Phase)
	}
	if got.Progress.Completed != 3 || got.Progress.Total != 3 {
		t.Fatalf("progress = %+v, want 3/3", got.Progress)
	}
	if got.Progress.Percentage != 100 {
		t.Fatalf("progress percentage = %v, want 100", got.Progress.Percentage)
	}
}

// T4.18.5 — Stale plan indicated: after the WBS moves to v2 the read model
// reports the stale flag true, derived in the read from the plan's stored
// source version — the plan row itself is untouched (derived, never
// persisted).
func TestT4185StalePlanIndicated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docs.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := docs.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := docs.UpdateWBS(ctx, projectID, phaseID, "# v2", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}

	got, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel: %v", err)
	}
	if got.WBS == nil || got.WBS.Version != 2 {
		t.Fatalf("wbs section = %+v, want version 2", got.WBS)
	}
	if !got.WBS.Stale {
		t.Fatal("wbs stale = false, want true (plan bound to v1, current WBS is v2)")
	}
	if got.Plan == nil || got.Plan.Version != 1 {
		t.Fatalf("plan section = %+v, want version 1", got.Plan)
	}

	// Derived, not persisted: the plan row still records its source version.
	var stored int
	if err := h.DBScan(ctx,
		"SELECT wbs_version FROM execution_plans WHERE id = '"+plan.ID.String()+"'",
		&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored wbs_version = %d, want 1 (binding never mutated)", stored)
	}

	// Scope and id error contracts (getScopedPhase semantics):
	// cross-project phase -> ErrNotFound; malformed ids -> ErrInvalidInput;
	// unknown phase -> ErrNotFound.
	if _, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, uuid.Nil.String(), phaseID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
	if _, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, "not-a-uuid", phaseID); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed project id: got %v, want ErrInvalidInput", err)
	}
	if _, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed phase id: got %v, want ErrInvalidInput", err)
	}
	other := createPhaseProject(t, h)
	if _, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, other, phaseID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project phase: got %v, want ErrNotFound", err)
	}
}

// The sequential (nil-pool) fallback must agree with the transactional
// path field-for-field on a fully-populated phase (WBS + plan + progress).
func TestT418SequentialFallbackSameRead(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	pool := h.App.DB().Raw()
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(pool))
	if _, err := docs.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := docs.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	units := dbrepo.NewWorkUnitRepository(pool)
	done, _ := wbsNewUnit(t, plan.ID, 1, "done")
	done.Status = wbs.WorkUnitStatusDONE
	if err := units.Create(ctx, done); err != nil {
		t.Fatalf("Create unit: %v", err)
	}
	pending, _ := wbsNewUnit(t, plan.ID, 2, "pending")
	if err := units.Create(ctx, pending); err != nil {
		t.Fatalf("Create unit: %v", err)
	}

	sequential := service.NewPhaseReadModelService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		nil,
	)
	got, err := sequential.PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel (sequential): %v", err)
	}
	if got.Phase == nil || got.Phase.Order != 1 {
		t.Fatalf("sequential phase section = %+v, want order 1", got.Phase)
	}
	if got.WBS == nil || got.WBS.Version != 1 || got.WBS.Stale {
		t.Fatalf("sequential wbs = %+v, want version 1 not stale", got.WBS)
	}
	if got.Plan == nil || got.Plan.Version != 1 {
		t.Fatalf("sequential plan = %+v, want version 1", got.Plan)
	}
	if got.Progress.Completed != 1 || got.Progress.Total != 2 || got.Progress.Percentage != 50 {
		t.Fatalf("sequential progress = %+v, want 1/2/50", got.Progress)
	}

	want, err := newPhaseReadModelService(t, h).PhaseReadModel(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PhaseReadModel (tx): %v", err)
	}
	if *got.Phase != *want.Phase {
		t.Fatalf("phase differs: %+v vs %+v", got.Phase, want.Phase)
	}
	if *got.WBS != *want.WBS {
		t.Fatalf("wbs differs: %+v vs %+v", got.WBS, want.WBS)
	}
	if *got.Plan != *want.Plan {
		t.Fatalf("plan differs: %+v vs %+v", got.Plan, want.Plan)
	}
	if got.Progress != want.Progress {
		t.Fatalf("progress differs: %+v vs %+v", got.Progress, want.Progress)
	}
}
