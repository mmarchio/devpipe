package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P4-12 (WBS 4.13): plan ↔ WBS binding. An execution plan retains the
// exact WBS version it was generated from: the source wbs_version is set
// at creation, WBS updates never mutate an existing plan row, the plan's
// historical source WBS content is retrievable after newer versions
// exist, and a plan referencing a non-existent WBS version is rejected.

func newPlanBindingService(t *testing.T, h *Harness) *service.WBSDocumentService {
	t.Helper()
	svc := newVersionedWBSDocService(t, h)
	return svc.WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
}

// snapshotPlanRows captures every plan row's binding-relevant state (source
// wbs_version, plan version, status, timestamps) for direct-SQL comparison
// across a WBS update.
func snapshotPlanRows(t *testing.T, h *Harness, phaseID string) string {
	t.Helper()
	var snapshot string
	if err := h.DBScan(context.Background(),
		`SELECT coalesce(string_agg(wbs_version::text || '|' || version::text || '|' || status || '|' || updated_at::text, ',' ORDER BY version), '') FROM execution_plans WHERE phase_id = '`+phaseID+`'`,
		&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// T4.13.1 — Plan references exact WBS version: a plan created against
// WBS v1 stores wbs_version = 1, and a later plan against v2 stores 2;
// the binding is persisted, not derived.
func TestT4131PlanReferencesExactWBSVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan1, err := svc.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan (v1): %v", err)
	}
	if plan1.WBSVersion != 1 {
		t.Fatalf("plan1.WBSVersion = %d, want 1", plan1.WBSVersion)
	}
	var stored int
	if err := h.DBScan(ctx,
		"SELECT wbs_version FROM execution_plans WHERE id = '"+plan1.ID.String()+"'",
		&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored wbs_version = %d, want 1", stored)
	}

	// A second plan generated from WBS v2 is bound to v2.
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "wbs v2", 1); err != nil {
		t.Fatalf("UpdateWBS to v2: %v", err)
	}
	plan2, err := svc.CreatePlan(ctx, projectID, phaseID, 2)
	if err != nil {
		t.Fatalf("CreatePlan (v2): %v", err)
	}
	if plan2.WBSVersion != 2 {
		t.Fatalf("plan2.WBSVersion = %d, want 2", plan2.WBSVersion)
	}
	if plan1.WBSVersion != 1 {
		t.Fatalf("plan1 re-read WBSVersion = %d, want 1 (never mutated)", plan1.WBSVersion)
	}
}

// T4.13.2 — WBS update doesn't change existing plan binding: after a WBS
// version is appended, every pre-existing plan row is byte-identical
// (wbs_version, version, status, updated_at) per direct SQL.
func TestT4132WBSUpdateDoesNotChangePlanBinding(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	for v := 1; v <= 2; v++ {
		if _, err := svc.CreatePlan(ctx, projectID, phaseID, v); err != nil {
			t.Fatalf("CreatePlan (v%d): %v", v, err)
		}
		if v == 1 {
			if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "wbs v2", 1); err != nil {
				t.Fatalf("UpdateWBS: %v", err)
			}
		}
	}
	before := snapshotPlanRows(t, h, phaseID)
	if before == "" {
		t.Fatal("no plan rows seeded")
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "wbs v3", 2); err != nil {
		t.Fatalf("UpdateWBS to v3: %v", err)
	}
	after := snapshotPlanRows(t, h, phaseID)
	if before != after {
		t.Fatalf("plan rows changed by WBS update:\nbefore: %s\nafter:  %s", before, after)
	}
}

// T4.13.3 — Historical source WBS retrievable from plan: after WBS v2
// exists, the v1-sourced plan resolves to the exact v1 content, not the
// current content.
func TestT4133HistoricalSourceWBSRtrievableFromPlan(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	const v1content = "historical v1 content"
	if _, err := svc.CreateWBS(ctx, projectID, phaseID, v1content); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := svc.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "current v2 content", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}

	rec, err := svc.GetPlanSourceWBS(ctx, projectID, phaseID, plan.ID.String())
	if err != nil {
		t.Fatalf("GetPlanSourceWBS: %v", err)
	}
	if rec.Version != 1 {
		t.Fatalf("source version = %d, want 1", rec.Version)
	}
	if rec.Content != v1content {
		t.Fatalf("source content = %q, want the historical v1 content %q", rec.Content, v1content)
	}

	// The current version is v2, so a source fetch that followed "current"
	// would have returned v2 — the historical row is the distinct one.
	cur, err := svc.GetWBS(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	if cur.Version != 2 || cur.Content == v1content {
		t.Fatalf("current = v%d %q, want v2 with different content", cur.Version, cur.Content)
	}
}

// T4.13.4 — Invalid WBS version reference rejected: a plan referencing a
// non-existent WBS version fails, and a phase with no WBS versions cannot
// be planned against an explicit version.
func TestT4134InvalidWBSVersionReferenceRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.CreatePlan(ctx, projectID, phaseID, 99); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("CreatePlan (non-existent v99): got %v, want ErrNotFound", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM execution_plans WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected plan persisted %d rows, want 0", count)
	}

	// A phase without any WBS has no valid version to reference.
	projectID2, phaseID2 := wbsPhaseForService(t, h)
	if _, err := svc.CreatePlan(ctx, projectID2, phaseID2, 1); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("CreatePlan (no WBS): got %v, want ErrNotFound", err)
	}
}
