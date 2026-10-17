package service

import (
	"testing"

	"github.com/google/uuid"

	"overseer/internal/readmodel"
)

func states(f readmodel.Flow) []string {
	var out []string
	for _, s := range f.Steps {
		out = append(out, s.State)
	}
	return out
}

// P17-118 (WBS 17.168) — COMPLETE and DONE are distinct steps; a unit at
// COMPLETE has DONE pending.
func TestP17118UnitFlow(t *testing.T) {
	f := UnitFlow(readmodel.StageComplete)
	if f.Steps[4].Label != "COMPLETE (worker report)" || f.Steps[4].State != "ACTIVE" || f.Steps[7].State != "PENDING" {
		t.Fatalf("%+v", f.Steps)
	}
	f = UnitFlow(readmodel.StageDone)
	if f.Steps[7].State != "DONE" || f.Steps[7].Label != "DONE (validated, checkpointed)" {
		t.Fatalf("%+v", f.Steps)
	}
}

// P17-118 (WBS 17.169 / 17.170) — 100 % units with phase validation running
// is not DONE.
func TestP17118PhaseFlow(t *testing.T) {
	p := readmodel.PhaseRow{Order: 3, Status: "VALIDATING", Progress: readmodel.NewProgress(10, 10)}
	f := PhaseFlow(p, "RUNNING")
	if got := states(f); got[0] != "DONE" || got[1] != "ACTIVE" || got[2] != "PENDING" {
		t.Fatalf("%v", got)
	}
	p.Status = "DONE"
	if got := states(PhaseFlow(p, "PASS")); got[2] != "DONE" {
		t.Fatalf("%v", got)
	}
}

// P17-118 (WBS 17.171) — gate order.
func TestP17118ProjectFlow(t *testing.T) {
	f := ProjectFlow(3, 3, "PASS", "", "RUNNING")
	labels := []string{f.Steps[0].Label, f.Steps[1].Label, f.Steps[2].Label, f.Steps[3].Label}
	if labels[0] != "All Phases DONE (3 / 3)" || labels[3] != "COMPLETED" {
		t.Fatalf("%v", labels)
	}
	if got := states(f); got[0] != "DONE" || got[1] != "DONE" || got[2] != "PENDING" || got[3] != "PENDING" {
		t.Fatalf("%v", got)
	}
	if got := states(ProjectFlow(3, 3, "PASS", "CREATED", "COMPLETED")); got[3] != "DONE" {
		t.Fatalf("%v", got)
	}
}

// P17-61 (WBS 17.167) — planning flow follows persisted events.
func TestP1761PlanningFlow(t *testing.T) {
	rows := []readmodel.TimelineRow{ // newest first
		{Type: "PLAN_ACTIVATED"},
		{Type: "EXECUTION_STEP_INTENT", Status: "PLAN_VALIDATE"},
		{Type: "WORKER_DISPATCH_INTENT"},
		{Type: "EXECUTION_STEP_INTENT", Status: "PLAN_DISPATCH"},
	}
	if got := states(PlanningFlow("3", rows)); got[4] != "DONE" {
		t.Fatalf("activated: %v", got)
	}
	if got := states(PlanningFlow("3", rows[2:])); got[1] != "ACTIVE" || got[4] != "PENDING" {
		t.Fatalf("waiting: %v", got)
	}
}

// P17-50 (WBS 17.23) — staleness from persisted versions.
func TestP1750Staleness(t *testing.T) {
	id := uuid.New()
	if s := stalenessOf(&id, 4, 5); !s.Stale || s.State != "STALE" || s.CurrentWBS != 5 || s.PlanSource != 4 {
		t.Fatalf("%+v", s)
	}
	if s := stalenessOf(&id, 5, 5); s.Stale || s.State != "CURRENT" {
		t.Fatalf("%+v", s)
	}
	if s := stalenessOf(nil, 0, 5); s.State != "UNPLANNED" {
		t.Fatalf("%+v", s)
	}
}

// P17-58 (WBS 17.91) — manual criteria never displayed as passed without
// approval; P17-55 (WBS 17.83) — DELIVERY_UNKNOWN distinct from FAILED.
func TestP1758AcceptanceAndDeliveryLabels(t *testing.T) {
	if it := acceptanceItem(0, "x", "MANUAL", "UNVERIFIED"); it.Symbol == "✓" || it.Label != "Manual review required" {
		t.Fatalf("%+v", it)
	}
	if it := acceptanceItem(0, "x", "COMMAND", "PASS"); it.Symbol != "✓" {
		t.Fatalf("%+v", it)
	}
	if it := acceptanceItem(0, "x", "TEST", "UNVERIFIED"); it.Label != "Review" {
		t.Fatalf("%+v", it)
	}
	if deliveryLabel("UNKNOWN", "DELIVERY_UNKNOWN") == deliveryLabel("FAILED", "") {
		t.Fatal("DELIVERY_UNKNOWN not distinct")
	}
	if l := deliveryLabel("UNKNOWN", "DELIVERY_UNKNOWN"); l != "DELIVERY UNKNOWN (not resent)" {
		t.Fatal(l)
	}
}
