package integration

import (
	"context"
	"testing"

	"overseer/internal/wbs"

	"github.com/google/uuid"
)

// T2.11.5 — Multiple historical plan versions supported (same phase).
func TestT2115MultiplePlanVersions(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(proj, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + proj.String() + "', 1, 'Phase', 'PENDING')"
	if _, err := h.DBExec(ctx, sql); err != nil {
		t.Fatalf("phase: %v", err)
	}
	p1, _ := wbs.NewExecutionPlan(phaseID, 1)
	p2, _ := wbs.NewExecutionPlan(phaseID, 2)
	for _, p := range []struct {
		id          string
		wbsVer, ver int
	}{
		{p1.ID.String(), 1, 1}, {p2.ID.String(), 2, 2},
	} {
		if _, err := h.DBExec(ctx, "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('"+
			p.id+"', '"+phaseID.String()+"', "+itoa(p.wbsVer)+", "+itoa(p.ver)+", 'READY')"); err != nil {
			t.Fatalf("plan v%d: %v", p.wbsVer, err)
		}
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM execution_plans WHERE phase_id = '"+phaseID.String()+"'", &count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("plans for phase = %d, want 2", count)
	}
	// A duplicate version for the same phase must be rejected.
	dup := uuid.New()
	if _, err := h.DBExec(ctx, "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('"+
		dup.String()+"', '"+phaseID.String()+"', 1, 1, 'READY')"); err == nil {
		t.Fatal("duplicate plan version accepted")
	}
}
