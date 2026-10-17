package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// P4-27 (WBS 4.27): the dedicated integration test for editing a future
// phase's WBS while an earlier phase is executing. A RUNNING project has
// Phase 3 EXECUTING and Phases 4/5 PENDING; Phase 5 already has WBS v1 and
// Plan v1 (wbs_version 1, READY). Updating Phase 5's WBS must succeed
// (T4.27.1), append WBS v2 (T4.27.2), retain Plan v1 unmutated (T4.27.3),
// derive the plan stale via plan.wbs_version < current WBS version
// (T4.27.4), and leave the executing Phase 3 unaffected (T4.27.5).
// Statuses are seeded by direct SQL (matching the p2_progress / p329 /
// P4-26 pattern); staleness is always derived, never persisted.
func TestP427WBSModificationDuringActiveDevelopment(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	names := []string{"Foundation", "Persistence", "API", "State Machine", "Prompt Engine"}
	created := make([]string, len(names))
	for i, name := range names {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase(%q): %v", name, err)
		}
		created[i] = p.ID.String()
	}

	// Seed the WBS 4.27 statuses: Phase 3 EXECUTING, 4/5 PENDING (default),
	// project RUNNING.
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+created[2]+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "UPDATE projects SET status = 'RUNNING' WHERE id = '"+projectID+"'"); err != nil {
		t.Fatal(err)
	}

	wbsSvc := newVersionedWBSDocService(t, h)
	const v1 = "# Prompt Engine\n\n## 1.1 Scope\n"
	const v2 = "# Prompt Engine\n\n## 1.1 Scope\n\n## 1.2 Added during development\n"
	if _, err := wbsSvc.CreateWBS(ctx, projectID, created[4], v1); err != nil {
		t.Fatalf("CreateWBS phase 5: %v", err)
	}

	// Seed Phase 5's Plan v1: derived from WBS v1, status READY.
	planID := uuid.New().String()
	if _, err := h.DBExec(ctx,
		"INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) "+
			"VALUES ('"+planID+"', '"+created[4]+"', 1, 1, 'READY')"); err != nil {
		t.Fatal(err)
	}

	// Capture the before-state: the plan row (T4.27.3/T4.27.4) and the
	// executing phase (T4.27.5).
	var planWBSVersion, planVersion int
	var planStatus, planCreated, planUpdated string
	if err := h.DBScan(ctx,
		"SELECT wbs_version, version, status, created_at::text, updated_at::text FROM execution_plans WHERE id = '"+planID+"'",
		&planWBSVersion, &planVersion, &planStatus, &planCreated, &planUpdated); err != nil {
		t.Fatal(err)
	}
	if planWBSVersion != 1 || planVersion != 1 || planStatus != "READY" {
		t.Fatalf("seeded plan = wbs_version %d version %d status %q, want 1/1/READY", planWBSVersion, planVersion, planStatus)
	}
	var execStatus, execUpdated string
	var execWBSCount int
	if err := h.DBScan(ctx,
		"SELECT status, updated_at::text FROM phases WHERE id = '"+created[2]+"'",
		&execStatus, &execUpdated); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+created[2]+"'", &execWBSCount); err != nil {
		t.Fatal(err)
	}
	if execStatus != "EXECUTING" || execWBSCount != 0 {
		t.Fatalf("seeded executing phase = status %q wbs rows %d, want EXECUTING/0", execStatus, execWBSCount)
	}

	// T4.27.1 — Updating a future phase's WBS while an earlier phase is
	// executing is permitted.
	rec, err := wbsSvc.UpdateWBS(ctx, projectID, created[4], v2, 0)
	if err != nil {
		t.Fatalf("UpdateWBS future phase while Phase 3 EXECUTING: %v", err)
	}

	// T4.27.2 — A new WBS version (v2) is created: the returned record, the
	// version history row, and the current-version pointer all agree.
	if rec.Version != 2 {
		t.Fatalf("updated version = %d, want 2", rec.Version)
	}
	var v2Stored string
	var vCount int
	if err := h.DBScan(ctx,
		"SELECT content FROM wbs_versions WHERE phase_id = '"+created[4]+"' AND version = 2", &v2Stored); err != nil {
		t.Fatalf("wbs_versions v2 row missing: %v", err)
	}
	if v2Stored != v2 {
		t.Fatalf("stored v2 content = %q, want the edited content", v2Stored)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+created[4]+"'", &vCount); err != nil {
		t.Fatal(err)
	}
	if vCount != 2 {
		t.Fatalf("phase 5 wbs_versions rows = %d, want 2", vCount)
	}
	var currentVersion int
	if err := h.DBScan(ctx,
		"SELECT current_version FROM phase_documents WHERE phase_id = '"+created[4]+"'", &currentVersion); err != nil {
		t.Fatal(err)
	}
	if currentVersion != 2 {
		t.Fatalf("current_version = %d, want 2", currentVersion)
	}

	// T4.27.3 — The existing Plan v1 is retained and not mutated: the row
	// still points at wbs_version 1 and its status/timestamps are unchanged.
	var afterWBSVersion, afterVersion int
	var afterStatus, afterCreated, afterUpdated string
	if err := h.DBScan(ctx,
		"SELECT wbs_version, version, status, created_at::text, updated_at::text FROM execution_plans WHERE id = '"+planID+"'",
		&afterWBSVersion, &afterVersion, &afterStatus, &afterCreated, &afterUpdated); err != nil {
		t.Fatal(err)
	}
	if afterWBSVersion != 1 || afterVersion != 1 || afterStatus != "READY" ||
		afterCreated != planCreated || afterUpdated != planUpdated {
		t.Fatalf("plan mutated by WBS edit: wbs_version %d->%d status %q->%q updated_at %q->%q",
			planWBSVersion, afterWBSVersion, planStatus, afterStatus, planUpdated, afterUpdated)
	}

	// T4.27.4 — Staleness is derived, never persisted: the plan keeps
	// wbs_version 1 while the current WBS version is 2, so
	// plan.wbs_version < current WBS version marks it stale.
	if currentVersion <= afterWBSVersion {
		t.Fatalf("plan.wbs_version %d >= current WBS version %d; plan should be derived stale", afterWBSVersion, currentVersion)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM execution_plans WHERE phase_id = '"+created[4]+"' AND status = 'READY' AND version = 1",
		&vCount); err != nil {
		t.Fatal(err)
	}
	if vCount != 1 {
		t.Fatalf("plan rows still READY/version 1 = %d, want 1 (no persisted staleness state)", vCount)
	}

	// T4.27.5 — The executing phase is unaffected: status, timestamp, and
	// WBS history are all unchanged by an edit to a future phase.
	var afterExecStatus, afterExecUpdated string
	var afterExecWBSCount int
	if err := h.DBScan(ctx,
		"SELECT status, updated_at::text FROM phases WHERE id = '"+created[2]+"'",
		&afterExecStatus, &afterExecUpdated); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+created[2]+"'", &afterExecWBSCount); err != nil {
		t.Fatal(err)
	}
	if afterExecStatus != "EXECUTING" || afterExecUpdated != execUpdated || afterExecWBSCount != 0 {
		t.Fatalf("executing phase affected: status %q->%q updated_at %q->%q wbs rows %d",
			execStatus, afterExecStatus, execUpdated, afterExecUpdated, afterExecWBSCount)
	}
}
