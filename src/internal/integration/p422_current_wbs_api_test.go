package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P4-22 Current WBS API (WBS 4.25, T4.25.1-T4.25.5):
// GET /api/v1/projects/{id}/current-wbs returns the project's current
// phase, its current WBS version, derived progress, and the current work
// unit. The current work unit is the active plan's first eligible unit in
// ascending sequence order (the ListEligible contract: PENDING/READY with
// all dependencies DONE — i.e. the first non-DONE unit whose turn it is);
// null when there is no active plan. A project with no current phase
// returns a defined empty response: 200 with null phase/wbs/current_work_unit
// and 0/0/0 progress.

func currentWBSURL(t *testing.T, h *Harness, projectID string) string {
	t.Helper()
	return h.BaseURL + "/api/v1/projects/" + projectID + "/current-wbs"
}

// currentWBSRig creates a project + phase through the service layer and
// returns the id plus the service handles the tests seed with.
type currentWBSRig struct {
	h         *Harness
	projectID string
	phaseID   string
	docs      *service.WBSDocumentService
	units     *dbrepo.WorkUnitRepository
}

func newCurrentWBSRig(t *testing.T, h *Harness, name string) *currentWBSRig {
	t.Helper()
	pool := h.App.DB().Raw()
	projectID := createPhaseProject(t, h)
	p, err := newPhaseService(t, h).CreatePhase(context.Background(), projectID, name, "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	return &currentWBSRig{
		h:         h,
		projectID: projectID,
		phaseID:   p.ID.String(),
		docs: newVersionedWBSDocService(t, h).
			WithPlans(dbrepo.NewPlanRepository(pool)),
		units: dbrepo.NewWorkUnitRepository(pool),
	}
}

func (r *currentWBSRig) wbs(t *testing.T, content string) {
	t.Helper()
	if _, err := r.docs.CreateWBS(context.Background(), r.projectID, r.phaseID, content); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
}

func (r *currentWBSRig) get(t *testing.T) (int, map[string]any) {
	t.Helper()
	return doJSON(t, http.MethodGet, currentWBSURL(t, r.h, r.projectID), "")
}

// T4.25.1 — Current phase resolved correctly: with phase 1 DONE and phase
// 2 EXECUTING, the endpoint reports phase 2 (its order and name), not the
// completed phase 1.
func TestT4251CurrentPhaseResolved(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p425 phase")
	s.addPhase(t, 1, "Foundation", "DONE")
	s.addPhase(t, 2, "Persistence", "EXECUTING")
	s.addPhase(t, 3, "API", "PENDING")
	status, out := doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs = %d, want 200", status)
	}
	ph, _ := out["phase"].(map[string]any)
	if ph == nil {
		t.Fatalf("phase missing: %+v", out)
	}
	if ph["name"] != "Persistence" {
		t.Fatalf("phase = %+v, want the EXECUTING phase 2", ph)
	}
	if ph["order"] != float64(2) {
		t.Fatalf("phase.order = %v, want 2", ph["order"])
	}
}

// T4.25.2 — Current WBS returned: the endpoint reports the phase's current
// WBS version, and after a second WBS edit it reports the NEW version
// (the current one, not a historical one).
func TestT4252CurrentWBSReturned(t *testing.T) {
	h := NewHarness(t)
	r := newCurrentWBSRig(t, h, "p425 wbs")
	r.wbs(t, "# v1")
	status, out := r.get(t)
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs = %d, want 200", status)
	}
	wbs, _ := out["wbs"].(map[string]any)
	if wbs == nil {
		t.Fatalf("wbs missing: %+v", out)
	}
	if wbs["version"] != float64(1) {
		t.Fatalf("wbs.version = %v, want 1", wbs["version"])
	}
	// A second edit appends version 2; the endpoint must track it.
	if _, err := r.docs.UpdateWBS(context.Background(), r.projectID, r.phaseID, "# v2", 1); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	status, out = r.get(t)
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs after update = %d, want 200", status)
	}
	wbs, _ = out["wbs"].(map[string]any)
	if wbs == nil || wbs["version"] != float64(2) {
		t.Fatalf("wbs = %+v, want version 2 after the edit", wbs)
	}
}

// T4.25.3 — Progress returned: with 1 DONE + 2 PENDING work units on the
// active plan, the endpoint reports derived 1/3/33.33 (DONE only, two
// decimal rounding per the established progress convention).
func TestT4253ProgressReturned(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p425 progress")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "DONE")
	s.addUnit(t, plan, 2, "b", "PENDING")
	s.addUnit(t, plan, 3, "c", "PENDING")
	status, out := doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs = %d, want 200", status)
	}
	prog, _ := out["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("progress missing: %+v", out)
	}
	if prog["completed"] != float64(1) || prog["total"] != float64(3) {
		t.Fatalf("completed/total = %v/%v, want 1/3", prog["completed"], prog["total"])
	}
	if !approx(prog["percentage"].(float64), 33.33) {
		t.Fatalf("percentage = %v, want ~33.33", prog["percentage"])
	}
}

// T4.25.4 — Current unit returned where applicable: the endpoint reports
// the active plan's first eligible unit in sequence order (its id, name,
// status). After the first unit is completed, the NEXT eligible unit is
// reported.
func TestT4254CurrentUnitReturned(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p425 unit")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	u1 := s.addUnit(t, plan, 1, "first", "PENDING")
	u2 := s.addUnit(t, plan, 2, "second", "PENDING")
	status, out := doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs = %d, want 200", status)
	}
	cu, _ := out["current_work_unit"].(map[string]any)
	if cu == nil {
		t.Fatalf("current_work_unit missing: %+v", out)
	}
	if cu["id"] != u1.String() || cu["name"] != "first" || cu["status"] != "PENDING" {
		t.Fatalf("current_work_unit = %+v, want unit 1 (first in sequence)", cu)
	}
	// Complete unit 1: unit 2 becomes the current eligible unit.
	s.setUnitStatus(t, u1, "DONE")
	status, out = doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs after completion = %d, want 200", status)
	}
	cu, _ = out["current_work_unit"].(map[string]any)
	if cu == nil || cu["id"] != u2.String() || cu["name"] != "second" {
		t.Fatalf("current_work_unit = %+v, want unit 2 after unit 1 completed", cu)
	}
}

// T4.25.5 — Completed project returns the defined empty/no-current
// response: with every phase DONE the endpoint is 200 with null phase,
// wbs, and current_work_unit and 0/0/0 progress — a defined shape, not an
// error. Unknown and malformed project ids follow the established
// project-scope contract (404 / 400).
func TestT4255CompletedProjectDefinedEmptyResponse(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p425 empty")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "PENDING")
	s.addPhase(t, 2, "Billing", "PENDING")

	status, out := doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs (has current phase) = %d, want 200", status)
	}
	// Mark every phase DONE: no current phase remains.
	if _, err := s.h.DBExec(context.Background(), "UPDATE phases SET status = 'DONE' WHERE project_id = '"+s.projectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	status, out = doJSON(t, http.MethodGet, currentWBSURL(t, h, s.projectID.String()), "")
	if status != http.StatusOK {
		t.Fatalf("GET current-wbs (completed project) = %d, want 200 (defined empty, not an error)", status)
	}
	if out["phase"] != nil || out["wbs"] != nil || out["current_work_unit"] != nil {
		t.Fatalf("completed project must return null sections: %+v", out)
	}
	prog, _ := out["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("progress missing: %+v", out)
	}
	if prog["completed"] != float64(0) || prog["total"] != float64(0) || prog["percentage"] != float64(0) {
		t.Fatalf("progress = %+v, want 0/0/0", prog)
	}

	// Unknown project id → 404 (established project-scope contract).
	status, _ = doJSON(t, http.MethodGet, currentWBSURL(t, h, "00000000-0000-0000-0000-000000000000"), "")
	if status != http.StatusNotFound {
		t.Fatalf("GET current-wbs (unknown project) = %d, want 404", status)
	}
	// Malformed project id → 400.
	status, _ = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/not-a-uuid/current-wbs", "")
	if status != http.StatusBadRequest {
		t.Fatalf("GET current-wbs (malformed id) = %d, want 400", status)
	}
}
