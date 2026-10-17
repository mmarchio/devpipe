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

// P4-18 (WBS 4.15): WBS read model. A single assembled read combining
// phase metadata, the current WBS (version, hash, content), and the plan
// relationship (plan version, source wbs_version, DERIVED staleness).
// Read-only; one transaction when a pool is wired, sequential fallback
// without one.

func newWBSReadModelService(t *testing.T, h *Harness) *service.WBSReadModelService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewWBSReadModelService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		dbrepo.NewPlanRepository(pool),
		pool,
	)
}

// T4.15.1 — Phase metadata returned: the read model's phase section
// carries the current phase's order, name, and status.
func TestT4151PhaseMetadataReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	other := createPhaseProject(t, h)
	if _, err := newPhaseService(t, h).CreatePhase(ctx, other, "foreign", ""); err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if _, err := service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(h.App.DB().Raw()),
		dbrepo.NewProjectRepository(h.App.DB().Raw()),
		dbrepo.NewDocumentRepository(h.App.DB().Raw()),
		dbrepo.NewWBSVersionRepository(h.App.DB().Raw()),
		h.App.DB().Raw(),
	).CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'EXECUTING' WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatal(err)
	}

	got, err := newWBSReadModelService(t, h).WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel: %v", err)
	}
	if got.Phase == nil {
		t.Fatal("read model has no phase section, want phase metadata")
	}
	if got.Phase.Order != 1 {
		t.Fatalf("phase order = %d, want 1", got.Phase.Order)
	}
	if got.Phase.Name != "versioned" {
		t.Fatalf("phase name = %q, want %q", got.Phase.Name, "versioned")
	}
	if got.Phase.Status != phase.StatusEXECUTING {
		t.Fatalf("phase status = %v, want EXECUTING", got.Phase.Status)
	}
}

// T4.15.2 — Current WBS returned: the read model's wbs section carries
// the current version's content (markdown retained exactly) and its
// deterministic content hash.
func TestT4152CurrentWBSReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docSvc := newVersionedWBSDocService(t, h)
	if _, err := docSvc.CreateWBS(ctx, projectID, phaseID, "# v1\n"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	const content = "# Phase WBS v2\n\n- keep exact markdown\n"
	if _, err := docSvc.UpdateWBS(ctx, projectID, phaseID, content, 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}

	got, err := newWBSReadModelService(t, h).WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel: %v", err)
	}
	if got.WBS == nil {
		t.Fatal("read model has no wbs section, want the current WBS")
	}
	if got.WBS.Version != 2 {
		t.Fatalf("wbs version = %d, want 2", got.WBS.Version)
	}
	if got.WBS.Content != content {
		t.Fatalf("wbs content = %q, want exact markdown retained", got.WBS.Content)
	}
	if got.WBS.Hash != wbs.HashContent(content) {
		t.Fatalf("wbs hash = %q, want %q", got.WBS.Hash, wbs.HashContent(content))
	}
}

// T4.15.3 — Current version returned: after three appends the read model
// reports version 3 with the latest content, and the phase section stays
// consistent with the same read.
func TestT4153CurrentVersionReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docSvc := newVersionedWBSDocService(t, h)
	for i, c := range []string{"# v1", "# v2", "# v3"} {
		exp := i // 0 = undeclared on the create
		if _, err := docSvc.UpdateWBS(ctx, projectID, phaseID, c, exp); err != nil {
			t.Fatalf("UpdateWBS %d: %v", i+1, err)
		}
	}

	got, err := newWBSReadModelService(t, h).WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel: %v", err)
	}
	if got.WBS == nil {
		t.Fatal("read model has no wbs section")
	}
	if got.WBS.Version != 3 {
		t.Fatalf("wbs version = %d, want 3", got.WBS.Version)
	}
	if got.WBS.Content != "# v3" {
		t.Fatalf("wbs content = %q, want %q", got.WBS.Content, "# v3")
	}
	if got.Phase == nil || got.Phase.Order != 1 {
		t.Fatalf("phase section inconsistent within the same read: %+v", got.Phase)
	}
}

// T4.15.4 — Plan relationship returned: the read model's plan section
// carries the active plan's version and the exact WBS version it was
// generated from.
func TestT4154PlanRelationshipReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docSvc := newVersionedWBSDocService(t, h)
	if _, err := docSvc.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	docSvc = docSvc.WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docSvc.CreatePlan(ctx, projectID, phaseID, 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	got, err := newWBSReadModelService(t, h).WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel: %v", err)
	}
	if got.Plan == nil {
		t.Fatal("read model has no plan section, want the plan relationship")
	}
	if got.Plan.Version != 1 {
		t.Fatalf("plan version = %d, want 1", got.Plan.Version)
	}
	if got.Plan.WBSVersion != 1 {
		t.Fatalf("plan wbs_version = %d, want 1", got.Plan.WBSVersion)
	}
}

// T4.15.5 — Staleness correctly derived: the plan section's stale flag is
// derived in the same read from the plan's stored source version and the
// current WBS version (never persisted) — false while the plan matches
// the current version, true after the WBS moves ahead, with the plan row
// itself untouched.
func TestT4155StalenessCorrectlyDerived(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	docSvc := newVersionedWBSDocService(t, h)
	if _, err := docSvc.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	docSvc = docSvc.WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	plan, err := docSvc.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := docSvc.UpdateWBS(ctx, projectID, phaseID, "# v2", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}

	got, err := newWBSReadModelService(t, h).WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel: %v", err)
	}
	if got.Plan == nil {
		t.Fatal("read model has no plan section")
	}
	if !got.Plan.Stale {
		t.Fatal("plan stale = false, want true (plan bound to v1, current WBS is v2)")
	}
	if got.WBS == nil || got.WBS.Version != 2 {
		t.Fatalf("wbs section = %+v, want version 2", got.WBS)
	}

	// Derived, not persisted: the plan row still records its original
	// source version.
	var stored int
	if err := h.DBScan(ctx,
		"SELECT wbs_version FROM execution_plans WHERE id = '"+plan.ID.String()+"'",
		&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored wbs_version = %d, want 1 (binding never mutated)", stored)
	}

	// Read-model edge shapes: no current phase, no WBS, no plan — each
	// absent section is nil, nothing fabricated.
	empty := createPhaseProject(t, h)
	got, err = newWBSReadModelService(t, h).WBSReadModel(ctx, empty)
	if err != nil {
		t.Fatalf("WBSReadModel (no phases): %v", err)
	}
	if got.Phase != nil || got.WBS != nil || got.Plan != nil {
		t.Fatalf("no-phase read = %+v, want all sections nil", got)
	}

	// A phase with no WBS has a nil wbs AND nil plan section (the plan
	// section requires a WBS to be planned against).
	plain := createPhaseProject(t, h)
	if _, err := newPhaseService(t, h).CreatePhase(ctx, plain, "bare", ""); err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	got, err = newWBSReadModelService(t, h).WBSReadModel(ctx, plain)
	if err != nil {
		t.Fatalf("WBSReadModel (phase, no WBS): %v", err)
	}
	if got.Phase == nil {
		t.Fatal("phase section missing for a phase without a WBS")
	}
	if got.WBS != nil || got.Plan != nil {
		t.Fatalf("no-WBS read = %+v, want nil wbs and nil plan", got)
	}

	// Malformed/unknown project ids keep the service error contract.
	if _, err := newWBSReadModelService(t, h).WBSReadModel(ctx, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed project id: got %v, want ErrInvalidInput", err)
	}
	if _, err := newWBSReadModelService(t, h).WBSReadModel(ctx, uuid.Nil.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project id: got %v, want ErrNotFound", err)
	}
}

// The sequential (nil-pool) fallback must return the same read: the same
// assembly without a transaction pool.
func TestT415SequentialFallbackSameRead(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	pool := h.App.DB().Raw()
	docSvc := service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		nil,
	).WithPlans(dbrepo.NewPlanRepository(pool))
	if _, err := docSvc.CreateWBS(ctx, projectID, phaseID, "# v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := docSvc.CreatePlan(ctx, projectID, phaseID, 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Sequential fallback (nil pool).
	sequential := service.NewWBSReadModelService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		dbrepo.NewPlanRepository(pool),
		nil,
	)
	got, err := sequential.WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel (sequential): %v", err)
	}
	if got.WBS == nil || got.WBS.Version != 1 || got.WBS.Content != "# v1" {
		t.Fatalf("sequential read wbs = %+v, want version 1", got.WBS)
	}
	if got.Plan == nil || got.Plan.Version != 1 || got.Plan.WBSVersion != 1 || got.Plan.Stale {
		t.Fatalf("sequential read plan = %+v, want version 1, wbs 1, not stale", got.Plan)
	}

	// The transactional path must agree field-for-field (explicit
	// compare: the sections are pointers).
	tx := newWBSReadModelService(t, h)
	want, err := tx.WBSReadModel(ctx, projectID)
	if err != nil {
		t.Fatalf("WBSReadModel (tx): %v", err)
	}
	if (got.Phase == nil) != (want.Phase == nil) ||
		(got.WBS == nil) != (want.WBS == nil) ||
		(got.Plan == nil) != (want.Plan == nil) {
		t.Fatalf("section presence differs: sequential %+v vs transactional %+v", got, want)
	}
	if got.Phase != nil && *got.Phase != *want.Phase {
		t.Fatalf("phase differs: %+v vs %+v", got.Phase, want.Phase)
	}
	if got.WBS != nil && *got.WBS != *want.WBS {
		t.Fatalf("wbs differs: %+v vs %+v", got.WBS, want.WBS)
	}
	if got.Plan != nil && *got.Plan != *want.Plan {
		t.Fatalf("plan differs: %+v vs %+v", got.Plan, want.Plan)
	}
}
