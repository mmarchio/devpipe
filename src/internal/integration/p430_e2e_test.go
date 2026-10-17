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
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// P4-30 (WBS 4.30) — Phase 4 end-to-end acceptance. The full flow: a
// 3-phase project (Foundation/Authentication/API) with WBS documents, a
// mock execution plan for Phase 1 created through the service/persistence
// API (no LLM), 10 work units (6 DONE / 1 EXECUTING / 3 PENDING ->
// progress 6/10 = 60%, current phase 1). Then: add Phase 4 (order 4),
// modify Phase 3's WBS (new version), complete the remaining Phase 1
// units (100%), mark Phase 1 DONE (Phase 2 becomes current), and restart
// on the SAME database — the entire state must reconstruct field-by-field.

// p430Snapshot is the full acceptance state the restart must reproduce,
// derived through the running instance's services and repositories.
// Progress, current phase, and staleness are DERIVED, never persisted.
type p430Snapshot struct {
	CurrentPhaseOrder int
	CurrentPhaseName  string
	CurrentPhaseStat  phase.PhaseStatus
	Phases            []string // order|name|status, ascending order
	WBSHistory        []string // phase order|version|content|hash
	PlanVersion       int
	PlanWBSVersion    int
	PlanStatus        string
	UnitStatuses      []string // work unit status, by sequence
	Completed         int
	Total             int
	Percentage        float64
}

func p430Capture(t *testing.T, h *Harness, projectID string) *p430Snapshot {
	t.Helper()
	ctx := context.Background()
	s := &p430Snapshot{}

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

	// WBS history for EVERY phase (ascending order, ascending version).
	for _, p := range phases {
		for v := 1; ; v++ {
			var content, hash string
			err := h.DBScan(ctx,
				"SELECT content, content_hash FROM wbs_versions WHERE phase_id = '"+p.ID.String()+"' AND version = "+itoa(v),
				&content, &hash)
			if err != nil {
				break // no such version: pgx.ErrNoRows ends the phase's history
			}
			s.WBSHistory = append(s.WBSHistory, itoa(p.Order)+"|v"+itoa(v)+"|"+content+"|"+hash)
		}
	}

	// Phase 1 plan binding and derived progress (Phase 1 is the only
	// planned phase in this scenario).
	plans, err := dbrepo.NewPlanRepository(h.App.DB().Raw()).ListByPhase(ctx, phases[0].ID)
	if err != nil {
		t.Fatalf("ListByPhase: %v", err)
	}
	active := wbs.ActivePlan(phases[0].ID, plans)
	if active == nil {
		t.Fatal("no active plan on phase 1")
	}
	s.PlanVersion, s.PlanWBSVersion, s.PlanStatus = active.Version, active.WBSVersion, string(active.Status)
	planUnits, err := dbrepo.NewWorkUnitRepository(h.App.DB().Raw()).ListByPlan(ctx, active.ID)
	if err != nil {
		t.Fatalf("ListByPlan: %v", err)
	}
	if len(planUnits) != 10 {
		t.Fatalf("work units = %d, want 10", len(planUnits))
	}
	for _, u := range planUnits {
		s.UnitStatuses = append(s.UnitStatuses, itoa(u.Sequence)+"="+string(u.Status))
	}
	wp := wbs.CalculateWorkProgress(planUnits)
	s.Completed, s.Total, s.Percentage = wp.Completed, wp.Total, wp.Percentage

	return s
}

// seedP430State builds the WBS 4.30 starting fixture: 3 phases with WBS
// documents, a mock Phase 1 plan (service API — no LLM), and 10 work
// units (6 DONE / 1 EXECUTING / 3 PENDING).
func seedP430State(t *testing.T, h *Harness, projectID string) []string {
	t.Helper()
	ctx := context.Background()
	phases := newPhaseService(t, h)
	var ids []string
	for _, name := range []string{"Foundation", "Authentication", "API"} {
		p, err := phases.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase(%s): %v", name, err)
		}
		ids = append(ids, p.ID.String())
	}
	docs := newVersionedWBSDocService(t, h)
	for i, id := range ids {
		if _, err := docs.CreateWBS(ctx, projectID, id, fmt.Sprintf("# %s WBS\n\n- initial scope %d\n", idsLabel(i), i+1)); err != nil {
			t.Fatalf("CreateWBS(phase %d): %v", i+1, err)
		}
	}
	// Mock execution plan for Phase 1 through the persistence API — no
	// LLM is invoked; the plan is bound to WBS v1.
	if _, err := docs.WithPlans(dbrepo.NewPlanRepository(h.App.DB().Raw())).CreatePlan(ctx, projectID, ids[0], 1); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	// 10 work units: 6 DONE, 1 EXECUTING, 3 PENDING (the statuses a unit
	// lifecycle can hold; no work-unit mutation service exists, so the
	// distribution is forced with direct SQL).
	var vals []string
	for i := 1; i <= 10; i++ {
		st := "PENDING"
		if i <= 6 {
			st = "DONE"
		} else if i == 7 {
			st = "EXECUTING"
		}
		vals = append(vals,
			"(gen_random_uuid(), (SELECT id FROM execution_plans WHERE phase_id = '"+ids[0]+"' AND version = 1), "+itoa(i)+
				", 'unit"+itoa(i)+"', '', 'ATOMIC', '"+st+"', '', '', '[]', '[]', '[]', now())")
	}
	if _, err := h.DBExec(ctx,
		"INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, "+
			"implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES "+
			strings.Join(vals, ",")); err != nil {
		t.Fatal(err)
	}
	return ids
}

func idsLabel(i int) string {
	switch i {
	case 0:
		return "Foundation"
	case 1:
		return "Authentication"
	default:
		return "API"
	}
}

// phase1FullService wires the real PhaseService with the plan/work-unit
// repositories so CompletePhase's eligibility path is real.
func phase1FullService(t *testing.T, h *Harness) *service.PhaseService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	)
}

func TestP430Phase4EndToEndAcceptance(t *testing.T) {
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
		// Self-cleanup evidence: our own DB name must be gone — the
		// overseer_e2e_* count must not have grown (the standard
		// overseer_it_% leak probe does not cover this prefix).
		if e2eAfter > e2eBefore {
			t.Errorf("leaked temp db: overseer_e2e count grew %d -> %d", e2eBefore, e2eAfter)
		}
	})

	ctx = context.Background()

	// First boot (migrates) — build the fixture and drive steps 1-8.
	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}
	projectID := createPhaseProject(t, h1)
	ids := seedP430State(t, h1, projectID)

	// Verify: current phase = Phase 1, progress = 6/10 = 60%.
	cur, err := newPhaseService(t, h1).GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.Order != 1 || cur.Name != "Foundation" {
		t.Fatalf("current phase = %+v, want Phase 1 Foundation", cur)
	}
	units, err := dbrepo.NewWorkUnitRepository(h1.App.DB().Raw()).ListByPlan(ctx,
		func() (id [16]byte) {
			var planID string
			if err := h1.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+ids[0]+"'", &planID); err != nil {
				t.Fatal(err)
			}
			parsed, perr := uuidParse(planID)
			if perr != nil {
				t.Fatal(perr)
			}
			return parsed
		}())
	if err != nil {
		t.Fatalf("ListByPlan: %v", err)
	}
	wp := wbs.CalculateWorkProgress(units)
	if wp.Completed != 6 || wp.Total != 10 || wp.Percentage != 60 {
		t.Fatalf("progress = %d/%d/%v%%, want 6/10/60%%", wp.Completed, wp.Total, wp.Percentage)
	}

	// Steps 1-2: add Phase 4; verify order 4.
	svc := newPhaseService(t, h1)
	p4, err := svc.CreatePhase(ctx, projectID, "Deployment", "")
	if err != nil {
		t.Fatalf("CreatePhase(4): %v", err)
	}
	if p4.Order != 4 {
		t.Fatalf("phase 4 order = %d, want 4", p4.Order)
	}

	// Steps 3-4: modify Phase 3 WBS; verify a new version was appended.
	docs := newVersionedWBSDocService(t, h1)
	if _, err := docs.UpdateWBS(ctx, projectID, ids[2], "# API WBS\n\n- revised endpoint list\n", 1); err != nil {
		t.Fatalf("UpdateWBS phase 3: %v", err)
	}
	hist, err := docs.ListWBSVersions(ctx, projectID, ids[2])
	if err != nil {
		t.Fatalf("ListWBSVersions: %v", err)
	}
	if len(hist) != 2 || hist[1].Version != 2 {
		t.Fatalf("phase 3 WBS versions = %d (last v%d), want 2 (v2 appended)", len(hist), hist[len(hist)-1].Version)
	}

	// Steps 5-6: complete the remaining Phase 1 units; progress 100%.
	if _, err := h1.DBExec(ctx,
		"UPDATE work_units SET status = 'DONE' WHERE execution_plan_id = "+
			"(SELECT id FROM execution_plans WHERE phase_id = '"+ids[0]+"') AND status <> 'DONE'"); err != nil {
		t.Fatal(err)
	}
	phase1Units, err := dbrepo.NewWorkUnitRepository(h1.App.DB().Raw()).ListByPlan(ctx,
		func() (id [16]byte) {
			var planID string
			if err := h1.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+ids[0]+"'", &planID); err != nil {
				t.Fatal(err)
			}
			parsed, perr := uuidParse(planID)
			if perr != nil {
				t.Fatal(perr)
			}
			return parsed
		}())
	if err != nil {
		t.Fatalf("ListByPlan: %v", err)
	}
	wp = wbs.CalculateWorkProgress(phase1Units)
	if wp.Completed != 10 || wp.Total != 10 || wp.Percentage != 100 {
		t.Fatalf("progress after completion = %d/%d/%v%%, want 10/10/100%%", wp.Completed, wp.Total, wp.Percentage)
	}

	// Step 7: mark Phase 1 DONE through the real service (CompletePhase
	// requires VALIDATING; drive that status with direct SQL — the
	// lifecycle path from EXECUTING to VALIDATING is not part of this
	// scenario), then step 8: verify Phase 2 becomes current.
	if _, err := h1.DBExec(ctx, "UPDATE phases SET status = 'VALIDATING' WHERE id = '"+ids[0]+"'"); err != nil {
		t.Fatal(err)
	}
	done, err := phase1FullService(t, h1).CompletePhase(ctx, projectID, ids[0])
	if err != nil {
		t.Fatalf("CompletePhase: %v", err)
	}
	if done.Status != phase.StatusDONE {
		t.Fatalf("phase 1 status = %s, want DONE", done.Status)
	}
	cur, err = newPhaseService(t, h1).GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.Order != 2 || cur.Name != "Authentication" {
		t.Fatalf("current phase after completion = %+v, want Phase 2 Authentication", cur)
	}

	before := p430Capture(t, h1, projectID)
	h1.Close()

	// Step 9: restart on the SAME database (never re-created).
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	t.Cleanup(h2.Close) // registered before dropDB -> runs first under LIFO

	// Step 10: the entire state reconstructs, field by field.
	after := p430Capture(t, h2, projectID)
	if after.CurrentPhaseOrder != before.CurrentPhaseOrder ||
		after.CurrentPhaseName != before.CurrentPhaseName ||
		after.CurrentPhaseStat != before.CurrentPhaseStat {
		t.Fatalf("current phase = %d/%q/%v, want %d/%q/%v",
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
	if len(after.WBSHistory) != len(before.WBSHistory) {
		t.Fatalf("WBS history rows = %d, want %d", len(after.WBSHistory), len(before.WBSHistory))
	}
	for i := range before.WBSHistory {
		if after.WBSHistory[i] != before.WBSHistory[i] {
			t.Fatalf("WBS history[%d] = %q, want %q", i, after.WBSHistory[i], before.WBSHistory[i])
		}
	}
	if after.PlanVersion != before.PlanVersion || after.PlanWBSVersion != before.PlanWBSVersion || after.PlanStatus != before.PlanStatus {
		t.Fatalf("plan = v%d (wbs v%d, %s), want v%d (wbs v%d, %s)",
			after.PlanVersion, after.PlanWBSVersion, after.PlanStatus,
			before.PlanVersion, before.PlanWBSVersion, before.PlanStatus)
	}
	if len(after.UnitStatuses) != len(before.UnitStatuses) {
		t.Fatalf("unit count = %d, want %d", len(after.UnitStatuses), len(before.UnitStatuses))
	}
	for i := range before.UnitStatuses {
		if after.UnitStatuses[i] != before.UnitStatuses[i] {
			t.Fatalf("unit[%d] = %q, want %q", i, after.UnitStatuses[i], before.UnitStatuses[i])
		}
	}
	if after.Completed != before.Completed || after.Total != before.Total || after.Percentage != before.Percentage {
		t.Fatalf("progress = %d/%d/%v%%, want %d/%d/%v%%",
			after.Completed, after.Total, after.Percentage,
			before.Completed, before.Total, before.Percentage)
	}
}
