package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// P4-29 (WBS 4.29) — restart reconstruction: the phase/WBS state must be
// entirely reconstructable after an overseer restart. The pre-shutdown
// fixture:
//
//	Phase 1 DONE, Phase 2 EXECUTING, Phase 3 PENDING
//	Phase 2: WBS v3, plan v2 based on WBS v2, progress 14/20
//
// After the restart the instance must DERIVE every answer field-by-field:
// current phase = 2, current WBS = v3, plan = v2 (stale, derived),
// progress = 14/20 (70%). The test boots TWO harness instances on the SAME
// persisted database (caller owns the DB lifecycle) and compares the full
// captured state field-by-field — not row counts or spot checks — so a
// restarted instance that silently corrupts one column fails. This is a
// verification test over existing behavior; no production code change is
// expected (T4.29.1–T4.29.5).

// p429Snapshot is the full pre-/post-restart state the acceptance criteria
// must verify, derived through the running instance's read-model services
// (P4-18/P4-19) and repositories.
type p429Snapshot struct {
	CurrentPhaseOrder int
	CurrentPhaseName  string
	CurrentPhaseStat  phase.PhaseStatus
	Phases            []string // order|name|status, ascending order
	WBSVersion        int
	WBSHash           string
	WBSContent        string
	WBSHistory        []string // version|content|hash, ascending version
	PlanVersion       int
	PlanWBSVersion    int
	PlanStale         bool
	PlanStatus        string
	Completed         int
	Total             int
	Remaining         int
	Percentage        float64
}

func p429Capture(t *testing.T, h *Harness, projectID string) *p429Snapshot {
	t.Helper()
	ctx := context.Background()
	s := &p429Snapshot{}

	// T4.29.1 — current phase, derived from the persisted phase states.
	cur, err := newPhaseService(t, h).GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur != nil {
		s.CurrentPhaseOrder, s.CurrentPhaseName, s.CurrentPhaseStat = cur.Order, cur.Name, cur.Status
	}

	phases, err := dbrepo.NewPhaseRepository(h.App.DB().Raw()).ListByProject(ctx, parseProjectID(t, projectID))
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	for _, p := range phases {
		s.Phases = append(s.Phases, itoa(p.Order)+"|"+p.Name+"|"+string(p.Status))
	}
	if len(phases) != 3 {
		t.Fatalf("phase count = %d, want 3", len(phases))
	}
	phase2 := phases[1]

	// T4.29.2 — WBS version history restored; current = v3 (derived).
	wbsSvc := newVersionedWBSDocService(t, h)
	for v := 1; ; v++ {
		if _, err := wbsSvc.GetWBSVersion(ctx, projectID, phase2.ID.String(), v); err != nil {
			break
		}
		var content, hash string
		if err := h.DBScan(ctx,
			"SELECT content, content_hash FROM wbs_versions WHERE phase_id = '"+phase2.ID.String()+"' AND version = "+itoa(v),
			&content, &hash); err != nil {
			t.Fatal(err)
		}
		s.WBSHistory = append(s.WBSHistory, itoa(v)+"|"+content+"|"+hash)
	}
	versions, err := wbsSvc.ListWBSVersions(ctx, projectID, phase2.ID.String())
	if err != nil {
		t.Fatalf("ListWBSVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("WBS versions = %d, want 3", len(versions))
	}
	current, err := wbsSvc.GetWBS(ctx, projectID, phase2.ID.String())
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	s.WBSVersion, s.WBSHash, s.WBSContent = current.Version, wbs.HashContent(current.Content), current.Content

	// T4.29.3/T4.29.4 — plan binding and derived staleness.
	plans, err := dbrepo.NewPlanRepository(h.App.DB().Raw()).ListByPhase(ctx, phase2.ID)
	if err != nil {
		t.Fatalf("ListByPhase: %v", err)
	}
	active := wbs.ActivePlan(phase2.ID, plans)
	if active == nil {
		t.Fatal("no active plan on phase 2")
	}
	s.PlanVersion = active.Version
	s.PlanWBSVersion = active.WBSVersion
	s.PlanStale = active.WBSVersion < s.WBSVersion
	s.PlanStatus = string(active.Status)

	// T4.29.5 — progress derived from the active plan's work units.
	units, err := dbrepo.NewWorkUnitRepository(h.App.DB().Raw()).ListByPlan(ctx, active.ID)
	if err != nil {
		t.Fatalf("ListByPlan: %v", err)
	}
	wp := wbs.CalculateWorkProgress(units)
	s.Completed, s.Total, s.Remaining, s.Percentage = wp.Completed, wp.Total, wp.Remaining, wp.Percentage

	return s
}

// parseProjectID converts a project id string to the [16]byte form the
// repositories take (test ids are all uuid strings minted by this suite).
func parseProjectID(t *testing.T, s string) [16]byte {
	t.Helper()
	b, err := uuidParse(s)
	if err != nil {
		t.Fatalf("bad project id %q: %v", s, err)
	}
	return b
}

func seedP429State(t *testing.T, h *Harness, projectID string) {
	t.Helper()
	ctx := context.Background()
	phases := newPhaseService(t, h)
	var ids []string
	for _, name := range []string{"phase 1", "phase 2", "phase 3"} {
		p, err := phases.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase(%s): %v", name, err)
		}
		ids = append(ids, p.ID.String())
	}
	// Phase 2 WBS history v1..v3 through the real document service, while
	// the phase is still PENDING (the EXECUTING protection set rejects WBS
	// edits); the plan is bound to WBS v2 per the WBS 4.29 fixture.
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw()))
	if _, err := docs.CreateWBS(ctx, projectID, ids[1], "# Phase 2 WBS v1\n"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := docs.UpdateWBS(ctx, projectID, ids[1], "# Phase 2 WBS v2\n\n- refined scope\n", 1); err != nil {
		t.Fatalf("UpdateWBS v2: %v", err)
	}
	if _, err := docs.UpdateWBS(ctx, projectID, ids[1], "# Phase 2 WBS v3\n\n- refined scope\n- later edit\n", 2); err != nil {
		t.Fatalf("UpdateWBS v3: %v", err)
	}

	// Plan: create v1 and supersede it so the active plan is v2, bound to
	// WBS v2 — the exact pre-shutdown shape from WBS 4.29.
	if _, err := docs.CreatePlan(ctx, projectID, ids[1], 2); err != nil {
		t.Fatalf("CreatePlan v1: %v", err)
	}
	if _, err := docs.CreatePlan(ctx, projectID, ids[1], 2); err != nil {
		t.Fatalf("CreatePlan v2: %v", err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE execution_plans SET status = 'SUPERSEDED' WHERE phase_id = '"+ids[1]+"' AND version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'DONE', completed_at = now() WHERE id = '"+ids[0]+"';"+
			"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+ids[1]+"';"+
			"UPDATE projects SET status = 'RUNNING' WHERE id = '"+projectID+"'"); err != nil {
		t.Fatal(err)
	}

	// 20 work units on plan v2: 14 DONE, 6 PENDING -> progress 14/20.
	var vals []string
	for i := 1; i <= 20; i++ {
		st := "PENDING"
		if i <= 14 {
			st = "DONE"
		}
		vals = append(vals,
			"(gen_random_uuid(), (SELECT id FROM execution_plans WHERE phase_id = '"+ids[1]+"' AND version = 2), "+itoa(i)+
				", 'unit"+itoa(i)+"', '', 'ATOMIC', '"+st+"', '', '', '[]', '[]', '[]', now())")
	}
	if _, err := h.DBExec(ctx,
		"INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, "+
			"implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES "+
			strings.Join(vals, ",")); err != nil {
		t.Fatal(err)
	}
}

func TestP429RestartReconstruction(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	e2eBefore := 0
	if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname LIKE 'overseer_e2e_%'").Scan(&e2eBefore); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	// Caller owns the DB lifecycle; dropDB is registered FIRST so it runs
	// LAST under LIFO (after h2.Close has released the pool).
	t.Cleanup(func() {
		dropDB(t, base, dbName)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		cc, err := pgx.Connect(ctx2, base)
		if err != nil {
			return
		}
		defer cc.Close(ctx2)
		e2eAfter := 0
		if err := cc.QueryRow(ctx2, "SELECT count(*) FROM pg_database WHERE datname LIKE 'overseer_e2e_%'").Scan(&e2eAfter); err != nil {
			return
		}
		// Self-cleanup evidence for this e2e-family DB (the standard
		// overseer_it_% leak probe does not cover overseer_e2e_*): our own
		// name must be gone — the count may not have grown.
		if e2eAfter > e2eBefore {
			t.Errorf("leaked temp db: overseer_e2e count grew %d -> %d", e2eBefore, e2eAfter)
		}
	})

	// First boot (migrates) — build the full pre-shutdown state.
	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}
	projectID := createPhaseProject(t, h1)
	seedP429State(t, h1, projectID)

	before := p429Capture(t, h1, projectID)
	// Sanity: the pre-shutdown answers match the WBS 4.29 fixture.
	if before.CurrentPhaseOrder != 2 {
		t.Fatalf("pre-restart current phase order = %d, want 2", before.CurrentPhaseOrder)
	}
	if before.WBSVersion != 3 {
		t.Fatalf("pre-restart current WBS = %d, want 3", before.WBSVersion)
	}
	if before.PlanVersion != 2 || before.PlanWBSVersion != 2 {
		t.Fatalf("pre-restart plan = v%d (wbs v%d), want v2 bound to wbs v2", before.PlanVersion, before.PlanWBSVersion)
	}
	if !before.PlanStale {
		t.Fatal("pre-restart plan stale = false, want true (plan bound to v2, current WBS is v3)")
	}
	if before.Completed != 14 || before.Total != 20 {
		t.Fatalf("pre-restart progress = %d/%d, want 14/20", before.Completed, before.Total)
	}
	if before.Percentage != 70 {
		t.Fatalf("pre-restart percentage = %v, want 70", before.Percentage)
	}
	h1.Close()

	// Second boot against the SAME persisted database (never re-created).
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	t.Cleanup(h2.Close) // registered before dropDB -> runs before it under LIFO
	after := p429Capture(t, h2, projectID)

	// Field-by-field comparison — every answer the restart must reproduce.
	if after.CurrentPhaseOrder != before.CurrentPhaseOrder ||
		after.CurrentPhaseName != before.CurrentPhaseName ||
		after.CurrentPhaseStat != before.CurrentPhaseStat {
		t.Fatalf("T4.29.1 current phase = %d/%q/%v, want %d/%q/%v",
			after.CurrentPhaseOrder, after.CurrentPhaseName, after.CurrentPhaseStat,
			before.CurrentPhaseOrder, before.CurrentPhaseName, before.CurrentPhaseStat)
	}
	if len(after.Phases) != len(before.Phases) {
		t.Fatalf("phase count = %d, want %d", len(after.Phases), len(before.Phases))
	}
	for i := range before.Phases {
		if after.Phases[i] != before.Phases[i] {
			t.Fatalf("phase[%d] = %q, want %q", i, after.Phases[i], before.Phases[i])
		}
	}
	if after.WBSVersion != before.WBSVersion || after.WBSHash != before.WBSHash || after.WBSContent != before.WBSContent {
		t.Fatalf("T4.29.2 current WBS = v%d (hash %s), want v%d (hash %s)",
			after.WBSVersion, after.WBSHash, before.WBSVersion, before.WBSHash)
	}
	if len(after.WBSHistory) != len(before.WBSHistory) {
		t.Fatalf("T4.29.2 WBS history rows = %d, want %d", len(after.WBSHistory), len(before.WBSHistory))
	}
	for i := range before.WBSHistory {
		if after.WBSHistory[i] != before.WBSHistory[i] {
			t.Fatalf("T4.29.2 WBS history[%d] = %q, want %q", i, after.WBSHistory[i], before.WBSHistory[i])
		}
	}
	if after.PlanVersion != before.PlanVersion || after.PlanWBSVersion != before.PlanWBSVersion || after.PlanStatus != before.PlanStatus {
		t.Fatalf("T4.29.3 plan = v%d (wbs v%d, %s), want v%d (wbs v%d, %s)",
			after.PlanVersion, after.PlanWBSVersion, after.PlanStatus,
			before.PlanVersion, before.PlanWBSVersion, before.PlanStatus)
	}
	if after.PlanStale != before.PlanStale || !after.PlanStale {
		t.Fatalf("T4.29.4 plan stale = %v, want %v (true)", after.PlanStale, before.PlanStale)
	}
	if after.Completed != before.Completed || after.Total != before.Total ||
		after.Remaining != before.Remaining || after.Percentage != before.Percentage {
		t.Fatalf("T4.29.5 progress = %d/%d/%v%%, want %d/%d/%v%%",
			after.Completed, after.Total, after.Percentage,
			before.Completed, before.Total, before.Percentage)
	}
}
