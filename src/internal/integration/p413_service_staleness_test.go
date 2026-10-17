package integration

import (
	"context"
	"testing"

	"overseer/internal/service"
)

// P4-13 (WBS 4.14): staleness detection. Staleness is always DERIVED —
// the active plan's stored source wbs_version compared against the
// phase's current WBS version — so a WBS update marks the previous plan
// stale on the same tick, with no new persisted state.

// T4.14.1 — Same version reports current: a plan generated from the
// current WBS version is not stale.
func TestT4141SameVersionReportsCurrent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.CreatePlan(ctx, projectID, phaseID, 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	got, err := svc.PlanStaleness(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PlanStaleness: %v", err)
	}
	if got != service.PlanStaleCurrent {
		t.Fatalf("PlanStaleness = %v, want current", got)
	}
}

// T4.14.2 — Older plan version reports stale: a plan bound to an older
// WBS version than the current one is stale.
func TestT4142OlderPlanVersionReportsStale(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.CreatePlan(ctx, projectID, phaseID, 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "wbs v2", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	got, err := svc.PlanStaleness(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PlanStaleness: %v", err)
	}
	if got != service.PlanStaleStale {
		t.Fatalf("PlanStaleness = %v, want stale", got)
	}
}

// T4.14.3 — No plan reports unplanned: a phase with no execution plan
// (and a phase with a WBS but no plan, and a phase with neither) is
// unplanned.
func TestT4143NoPlanReportsUnplanned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	got, err := svc.PlanStaleness(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PlanStaleness (no WBS, no plan): %v", err)
	}
	if got != service.PlanStaleUnplanned {
		t.Fatalf("no WBS/no plan: got %v, want unplanned", got)
	}

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	got, err = svc.PlanStaleness(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PlanStaleness (WBS, no plan): %v", err)
	}
	if got != service.PlanStaleUnplanned {
		t.Fatalf("WBS/no plan: got %v, want unplanned", got)
	}
}

// T4.14.4 — Updating WBS immediately marks previous plan stale: the plan
// row is untouched by the WBS update (WBS 4.13) and the staleness flip
// is derived in the same call path, not persisted.
func TestT4144WBSUpdateImmediatelyMarksPreviousPlanStale(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newPlanBindingService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "wbs v1"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	plan, err := svc.CreatePlan(ctx, projectID, phaseID, 1)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if got, err := svc.PlanStaleness(ctx, projectID, phaseID); err != nil {
		t.Fatalf("PlanStaleness (before): %v", err)
	} else if got != service.PlanStaleCurrent {
		t.Fatalf("before update: got %v, want current", got)
	}

	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "wbs v2", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	got, err := svc.PlanStaleness(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("PlanStaleness (after): %v", err)
	}
	if got != service.PlanStaleStale {
		t.Fatalf("after update: got %v, want stale", got)
	}

	// Derived, not persisted: the plan row still records its original
	// source version and no staleness column exists on it.
	var stored int
	if err := h.DBScan(ctx,
		"SELECT wbs_version FROM execution_plans WHERE id = '"+plan.ID.String()+"'",
		&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored wbs_version = %d, want 1 (binding never mutated)", stored)
	}
	var hasStaleCol bool
	if err := h.DBScan(ctx,
		`SELECT count(*) > 0 FROM information_schema.columns
		 WHERE table_name = 'execution_plans' AND column_name IN ('stale', 'plan_stale', 'is_stale')`,
		&hasStaleCol); err != nil {
		t.Fatal(err)
	}
	if hasStaleCol {
		t.Fatal("staleness is persisted as a column, want derived")
	}
}
