package integration

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// P4-31 (Phase 4 Definition of Done gate, WBS L1039-1102) — the invariants
// the individual task tests do not cover as a single declaration. The
// invariants proven by dedicated passing tests (P4-02/24/26/27/28/29/30)
// are verified by the suite, not re-proven here; this file covers only the
// three deltas the DoD gate requires:
//
//   - progress may decrease when legitimate scope is added (derived);
//   - historical phase numbers are not reused automatically;
//   - adding phases during development does not disturb current work.

// p431DerivedProgress computes the derived WBS progress of the plan bound to
// a phase (derived from persisted work units, never persisted).
func p431DerivedProgress(t *testing.T, h *Harness, phaseID string) wbs.WorkProgress {
	t.Helper()
	ctx := context.Background()
	var planIDStr string
	if err := h.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phaseID+"'", &planIDStr); err != nil {
		t.Fatalf("p431 find plan: %v", err)
	}
	planID, err := uuid.Parse(planIDStr)
	if err != nil {
		t.Fatalf("p431 parse plan id: %v", err)
	}
	units, err := dbrepo.NewWorkUnitRepository(h.App.DB().Raw()).ListByPlan(ctx, planID)
	if err != nil {
		t.Fatalf("p431 ListByPlan: %v", err)
	}
	return wbs.CalculateWorkProgress(units)
}

// p431UnitRows returns the persisted (sequence, status) rows of a phase's
// plan, for undisturbed-state assertions.
func p431UnitRows(t *testing.T, h *Harness, phaseID string) map[int]string {
	t.Helper()
	ctx := context.Background()
	var planIDStr string
	if err := h.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phaseID+"'", &planIDStr); err != nil {
		t.Fatalf("p431 find plan: %v", err)
	}
	planID, err := uuid.Parse(planIDStr)
	if err != nil {
		t.Fatalf("p431 parse plan id: %v", err)
	}
	units, err := dbrepo.NewWorkUnitRepository(h.App.DB().Raw()).ListByPlan(ctx, planID)
	if err != nil {
		t.Fatalf("p431 ListByPlan: %v", err)
	}
	rows := make(map[int]string, len(units))
	for _, u := range units {
		rows[u.Sequence] = string(u.Status)
	}
	return rows
}

// p431PhaseOrders returns the phase orders of a project, ascending. The
// harness DBScan accepts no parameters, so rows are paged one at a time.
func p431PhaseOrders(t *testing.T, h *Harness, projectID string) []int {
	t.Helper()
	var orders []int
	for i := 0; ; i++ {
		var order int
		err := h.DBScan(context.Background(),
			"SELECT phase_order FROM phases WHERE project_id = '"+projectID+"' ORDER BY phase_order LIMIT 1 OFFSET "+itoa(i), &order)
		if err != nil {
			break
		}
		orders = append(orders, order)
	}
	return orders
}

// DoD invariant: progress may decrease when legitimate scope is added.
// A PENDING unit appended to an active plan must drop the DERIVED
// percentage (derived, not persisted).
func TestP431ProgressDecreasesOnScopeAddition(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Foundation", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	setPhaseStatus(t, h, p.ID.String(), phase.StatusEXECUTING)
	plan := seedPlan(t, h, p.ID, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "DONE")
	seedUnit(t, h, plan, 3, "c", "EXECUTING")
	seedUnit(t, h, plan, 4, "d", "PENDING")

	before := p431DerivedProgress(t, h, p.ID.String())
	if before.Completed != 2 || before.Total != 4 || before.Percentage != 50 {
		t.Fatalf("progress before scope addition = %d/%d/%v%%, want 2/4/50%%", before.Completed, before.Total, before.Percentage)
	}

	// Legitimate scope addition: a new PENDING work unit on the active plan.
	seedUnit(t, h, plan, 5, "scope addition", "PENDING")

	after := p431DerivedProgress(t, h, p.ID.String())
	if after.Completed != 2 || after.Total != 5 {
		t.Fatalf("progress after scope addition = %d/%d/%v%%, want 2/5", after.Completed, after.Total, after.Percentage)
	}
	if after.Percentage >= before.Percentage {
		t.Fatalf("derived progress did not decrease: before %v%%, after %v%%", before.Percentage, after.Percentage)
	}
}

// DoD invariant: historical phase numbers are not reused automatically.
// Deleting a phase and appending a new one must yield a FRESH order
// (MAX+1), never a deleted historical number.
func TestP431PhaseNumbersNotReused(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p1, err := svc.CreatePhase(ctx, projectID, "one", "")
	if err != nil {
		t.Fatalf("CreatePhase(1): %v", err)
	}
	p2, err := svc.CreatePhase(ctx, projectID, "two", "")
	if err != nil {
		t.Fatalf("CreatePhase(2): %v", err)
	}
	p3, err := svc.CreatePhase(ctx, projectID, "three", "")
	if err != nil {
		t.Fatalf("CreatePhase(3): %v", err)
	}

	// Delete the highest phase (order 3) — its number is historical.
	if err := svc.DeletePhase(ctx, projectID, p3.ID.String()); err != nil {
		t.Fatalf("DeletePhase(3): %v", err)
	}
	// The persistent high-water mark survives the deletion (migration 00014).
	var mark int
	if err := h.DBScan(ctx, "SELECT max_phase_order FROM projects WHERE id = '"+projectID+"'", &mark); err != nil {
		t.Fatal(err)
	}
	if mark != 3 {
		t.Fatalf("max_phase_order = %d, want 3 after deleting the highest phase", mark)
	}

	// Append a new phase: a fresh order (MAX+1 = 4), not the reused 3.
	p4, err := svc.CreatePhase(ctx, projectID, "four", "")
	if err != nil {
		t.Fatalf("CreatePhase(4): %v", err)
	}
	if p4.Order == 3 {
		t.Fatalf("appended phase reused the deleted historical number 3 (order = %d)", p4.Order)
	}
	if p4.Order != 4 {
		t.Fatalf("appended phase order = %d, want 4 (MAX+1)", p4.Order)
	}
	orders := p431PhaseOrders(t, h, projectID)
	if len(orders) != 3 {
		t.Fatalf("phase count = %d, want 3", len(orders))
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i] < orders[j] })
	want := []int{p1.Order, p2.Order, p4.Order}
	for i := range want {
		if orders[i] != want[i] {
			t.Fatalf("phase orders = %v, want %v", orders, want)
		}
	}
}

// DoD invariant: adding phases during development does not disturb current
// work. Appending a phase while a phase is EXECUTING with an active plan
// leaves the existing plan/unit state and the derived progress unchanged.
func TestP431AppendDuringDevelopmentUndisturbed(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p1, err := svc.CreatePhase(ctx, projectID, "Active", "")
	if err != nil {
		t.Fatalf("CreatePhase(1): %v", err)
	}
	p2, err := svc.CreatePhase(ctx, projectID, "Next", "")
	if err != nil {
		t.Fatalf("CreatePhase(2): %v", err)
	}
	_ = p2
	setPhaseStatus(t, h, p1.ID.String(), phase.StatusEXECUTING)
	plan := seedPlan(t, h, p1.ID, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "EXECUTING")
	seedUnit(t, h, plan, 3, "c", "PENDING")

	beforeProgress := p431DerivedProgress(t, h, p1.ID.String())
	beforeUnits := p431UnitRows(t, h, p1.ID.String())
	curBefore, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if curBefore == nil || curBefore.ID != p1.ID {
		t.Fatalf("current phase before append = %+v, want %v", curBefore, p1)
	}

	// Append a phase while development is active.
	if _, err := svc.CreatePhase(ctx, projectID, "Added", ""); err != nil {
		t.Fatalf("CreatePhase(3): %v", err)
	}

	// The existing plan/unit state is untouched and the derived progress is
	// the same; the current phase is undisturbed.
	afterProgress := p431DerivedProgress(t, h, p1.ID.String())
	if afterProgress != beforeProgress {
		t.Fatalf("derived progress changed by phase append: %+v -> %+v", beforeProgress, afterProgress)
	}
	afterUnits := p431UnitRows(t, h, p1.ID.String())
	if len(afterUnits) != len(beforeUnits) {
		t.Fatalf("work unit count changed: %d -> %d", len(beforeUnits), len(afterUnits))
	}
	for seq, status := range beforeUnits {
		if afterUnits[seq] != status {
			t.Fatalf("unit %d status changed: %s -> %s", seq, status, afterUnits[seq])
		}
	}
	curAfter, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase after append: %v", err)
	}
	if curAfter == nil || curAfter.ID != p1.ID {
		t.Fatalf("current phase after append = %+v, want %v", curAfter, p1)
	}
	var planStatus string
	if err := h.DBScan(ctx, "SELECT status FROM execution_plans WHERE id = '"+plan.String()+"'", &planStatus); err != nil {
		t.Fatal(err)
	}
	if planStatus != "EXECUTING" {
		t.Fatalf("active plan status = %q, want EXECUTING", planStatus)
	}
}
