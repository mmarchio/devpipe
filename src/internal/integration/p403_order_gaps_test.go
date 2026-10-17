package integration

import (
	"context"
	"testing"

	"overseer/internal/phase"
)

// T4.4.1 — Deleted order isn't reused: with phases 1..5, deleting the
// order-4 phase and appending again yields order 6, never 4.
func TestT441DeletedOrderNotReused(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		if p.Order != i+1 {
			t.Fatalf("phase %d order = %d, want %d", i+1, p.Order, i+1)
		}
		ids[i] = p.ID.String()
	}

	// Delete the order-4 phase.
	if err := svc.DeletePhase(ctx, projectID, ids[3]); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}

	// The next append must take 6, not 4.
	p, err := svc.CreatePhase(ctx, projectID, "next", "")
	if err != nil {
		t.Fatalf("CreatePhase after delete: %v", err)
	}
	if p.Order != 6 {
		t.Fatalf("append after delete took order %d, want 6 (deleted order 4 must not be reused)", p.Order)
	}
}

// T4.4.2 — MAX(order)+1 allocation: deleting a NON-max order leaves the
// maximum intact and the next append takes max+1.
func TestT442MaxPlusOneAllocation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}

	// Delete order 2 (not the max).
	if err := svc.DeletePhase(ctx, projectID, ids[1]); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}
	var maxOrder int
	if err := h.DBScan(ctx,
		"SELECT coalesce(MAX(phase_order), 0) FROM phases WHERE project_id = '"+projectID+"'", &maxOrder); err != nil {
		t.Fatal(err)
	}
	if maxOrder != 5 {
		t.Fatalf("max order after deleting a non-max phase = %d, want 5 (max must stay intact)", maxOrder)
	}

	p, err := svc.CreatePhase(ctx, projectID, "next", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if p.Order != 6 {
		t.Fatalf("next order = %d, want MAX(order)+1 = 6", p.Order)
	}
}

// T4.4.3 — Existing phases aren't renumbered: after a delete and a
// follow-up append, every surviving phase keeps its original order.
func TestT443NoRenumbering(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	type row struct {
		id    string
		order int
	}
	rows := make([]row, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		rows[i] = row{id: p.ID.String(), order: p.Order}
	}

	if err := svc.DeletePhase(ctx, projectID, rows[3].id); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}
	if _, err := svc.CreatePhase(ctx, projectID, "next", ""); err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}

	// Every surviving phase must keep its historical order.
	for _, want := range []row{rows[0], rows[1], rows[2], rows[4]} {
		var order int
		if err := h.DBScan(ctx,
			"SELECT phase_order FROM phases WHERE id = '"+want.id+"'", &order); err != nil {
			t.Fatal(err)
		}
		if order != want.order {
			t.Fatalf("phase %s renumbered to %d, want %d (historical orders are immutable)", want.id, order, want.order)
		}
	}
	// The new phase sits at 6, above all survivors.
	var newOrder int
	if err := h.DBScan(ctx,
		"SELECT phase_order FROM phases WHERE project_id = '"+projectID+"' AND phase_order = 6", &newOrder); err != nil {
		t.Fatalf("expected exactly one phase at order 6: %v", err)
	}
	if newOrder != 6 {
		t.Fatalf("new phase order = %d, want 6", newOrder)
	}
}

// T4.4.4 — Listing tolerates gaps: after deleting order 4 of 1..5, the
// list returns 1,2,3,5 in order — no fill-in, no reindex.
func TestT444ListingToleratesGaps(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	if err := svc.DeletePhase(ctx, projectID, ids[3]); err != nil {
		t.Fatalf("DeletePhase: %v", err)
	}

	got, err := svc.ListPhases(ctx, projectID)
	if err != nil {
		t.Fatalf("ListPhases: %v", err)
	}
	var orders []int
	for _, p := range got {
		if p.Status != phase.StatusPENDING {
			t.Fatalf("phase order %d status = %s", p.Order, p.Status)
		}
		orders = append(orders, p.Order)
	}
	want := []int{1, 2, 3, 5}
	if len(orders) != len(want) {
		t.Fatalf("listed %d phases (%v), want %v", len(orders), orders, want)
	}
	for i := range want {
		if orders[i] != want[i] {
			t.Fatalf("listed orders = %v, want %v (gaps must be preserved in order, no fill-in)", orders, want)
		}
	}
}
