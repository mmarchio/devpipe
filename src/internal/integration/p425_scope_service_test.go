package integration

import (
	"context"
	"testing"
)

// T4.20.1-T4.20.3 at the service level (WBS 4.20 live driver): progress is
// re-derived from the real work-unit rows on every read — appending four
// PENDING units to a 12/16 (75%) plan recalculates to 12/20 (60%) with
// completed unchanged. No percentage is persisted anywhere; the read model
// simply reflects the unit set at read time.
func TestP425ScopeChangeServiceDerived(t *testing.T) {
	r := newProgressRig(t)
	ctx := context.Background()
	pid := r.project(t)
	ph := r.addPhase(t, pid, 1, "P1", "EXECUTING")
	plan := r.addPlan(t, ph, 1, 1, "READY")
	for seq := 1; seq <= 12; seq++ {
		r.addUnit(t, plan, seq, "done-"+itoa(seq), "DONE")
	}
	for seq := 13; seq <= 16; seq++ {
		r.addUnit(t, plan, seq, "open-"+itoa(seq), "PENDING")
	}

	before, err := r.svc.CurrentPhaseProgress(ctx, pid)
	if err != nil {
		t.Fatalf("progress before append: %v", err)
	}
	if before.Progress.Total != 16 || before.Progress.Completed != 12 || before.Progress.Percentage != 75 {
		t.Fatalf("baseline = %+v, want 12/16 = 75%%", before.Progress)
	}

	// Planner adds four required (PENDING) units.
	for seq := 17; seq <= 20; seq++ {
		r.addUnit(t, plan, seq, "added-"+itoa(seq), "PENDING")
	}

	after, err := r.svc.CurrentPhaseProgress(ctx, pid)
	if err != nil {
		t.Fatalf("progress after append: %v", err)
	}
	if after.Progress.Total != 20 {
		t.Fatalf("total = %d after append, want 20 (T4.20.1)", after.Progress.Total)
	}
	if after.Progress.Completed != 12 {
		t.Fatalf("completed = %d after append, want 12 (T4.20.2)", after.Progress.Completed)
	}
	if after.Progress.Percentage != 60 || after.Progress.Percentage >= before.Progress.Percentage {
		t.Fatalf("percentage = %v (before %v), want 60 and a decrease (T4.20.3)", after.Progress.Percentage, before.Progress.Percentage)
	}
	if after.ActivePlan == nil || after.ActivePlan.ID != plan {
		t.Fatalf("active plan changed by the append: %+v", after.ActivePlan)
	}
}
