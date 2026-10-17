package integration

import (
	"context"
	"math"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// P3-21 summary endpoint (WBS 3.21, T3.21.1-T3.21.6):
// GET /api/v1/projects/{id}/summary.

func summaryURL(t *testing.T, h *Harness, projectID string) string {
	t.Helper()
	return h.BaseURL + "/api/v1/projects/" + projectID + "/summary"
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// summaryRig creates a project and seeds phases/plans/units directly (the
// same pattern as p2_phase_progress_test.go) and returns the project id.
func summaryRig(t *testing.T, h *Harness, name string) (uuid.UUID, *summarySeeds) {
	t.Helper()
	created := createViaAPI(t, h, name, "")
	pid := uuid.MustParse(created.ID)
	return pid, &summarySeeds{h: h, projectID: pid}
}

type summarySeeds struct {
	h         *Harness
	projectID uuid.UUID
}

func (s *summarySeeds) addPhase(t *testing.T, order int, name, status string) uuid.UUID {
	t.Helper()
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + s.projectID.String() + "', " + itoa(order) + ", '" + name + "', '" + status + "')"
	if _, err := s.h.DBExec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
	return phaseID
}

func (s *summarySeeds) addPlan(t *testing.T, phaseID uuid.UUID, wbsVersion, version int, status string) uuid.UUID {
	t.Helper()
	planID := uuid.New()
	sql := "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('" +
		planID.String() + "', '" + phaseID.String() + "', " + itoa(wbsVersion) + ", " + itoa(version) + ", '" + status + "')"
	if _, err := s.h.DBExec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
	return planID
}

func (s *summarySeeds) addUnit(t *testing.T, planID uuid.UUID, seq int, name, status string) uuid.UUID {
	t.Helper()
	u := uuid.New()
	sql := "INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES ('" +
		u.String() + "', '" + planID.String() + "', " + itoa(seq) + ", '" + name + "', '', 'ATOMIC', '" + status + "', '', '', '[]', '[]', '[]', now())"
	if _, err := s.h.DBExec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
	return u
}

func (s *summarySeeds) setUnitStatus(t *testing.T, unitID uuid.UUID, status string) {
	t.Helper()
	if _, err := s.h.DBExec(context.Background(),
		"UPDATE work_units SET status = '"+status+"' WHERE id = '"+unitID.String()+"'"); err != nil {
		t.Fatal(err)
	}
}

func (s *summarySeeds) setProjectStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := s.h.DBExec(context.Background(),
		"UPDATE projects SET status = '"+status+"' WHERE id = '"+s.projectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
}

func (s *summarySeeds) get(t *testing.T) (int, map[string]any) {
	t.Helper()
	return doJSON(t, http.MethodGet, summaryURL(t, s.h, s.projectID.String()), "")
}

// T3.21.1 — Summary returns project lifecycle state.
func TestT3211LifecycleState(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 state")
	s.setProjectStatus(t, "RUNNING")
	status, out := s.get(t)
	if status != http.StatusOK {
		t.Fatalf("GET summary = %d, want 200", status)
	}
	if out["id"] != s.projectID.String() || out["name"] != "p321 state" {
		t.Fatalf("id/name = %v/%v, want the project", out["id"], out["name"])
	}
	if out["status"] != "RUNNING" {
		t.Fatalf("status = %v, want RUNNING", out["status"])
	}
}

// T3.21.2 — Summary returns current phase.
func TestT3212CurrentPhase(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 phase")
	s.addPhase(t, 1, "Auth", "DONE")
	ph2 := s.addPhase(t, 2, "Billing", "EXECUTING")
	s.addPhase(t, 3, "Deploy", "PENDING")
	_, out := s.get(t)
	ph, _ := out["phase"].(map[string]any)
	if ph == nil {
		t.Fatalf("phase missing: %+v", out)
	}
	if ph["id"] != ph2.String() || ph["name"] != "Billing" || ph["status"] != "EXECUTING" {
		t.Fatalf("phase = %+v, want the EXECUTING phase", ph)
	}
	if ph["order"] != float64(2) {
		t.Fatalf("phase.order = %v, want 2", ph["order"])
	}
}

// T3.21.3 — Summary returns WBS progress.
func TestT3213Progress(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 progress")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "DONE")
	s.addUnit(t, plan, 2, "b", "DONE")
	s.addUnit(t, plan, 3, "c", "PENDING")
	_, out := s.get(t)
	prog, _ := out["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("progress missing: %+v", out)
	}
	if prog["completed"] != float64(2) || prog["total"] != float64(3) {
		t.Fatalf("completed/total = %v/%v, want 2/3", prog["completed"], prog["total"])
	}
	if !approx(prog["percentage"].(float64), 66.67) {
		t.Fatalf("percentage = %v, want ~66.67", prog["percentage"])
	}
}

// T3.21.4 — No plan returns 0/0/0.
func TestT3214NoPlanZero(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 zero")
	s.addPhase(t, 1, "Auth", "EXECUTING") // no plan
	_, out := s.get(t)
	prog, _ := out["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("progress missing: %+v", out)
	}
	if prog["completed"] != float64(0) || prog["total"] != float64(0) || prog["percentage"] != float64(0) {
		t.Fatalf("progress = %+v, want 0/0/0", prog)
	}
}

// T3.21.5 — Completed work reflected immediately.
func TestT3215CompletionImmediate(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 immediate")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	u1 := s.addUnit(t, plan, 1, "a", "PENDING")
	u2 := s.addUnit(t, plan, 2, "b", "PENDING")
	_, before := s.get(t)
	if before["progress"].(map[string]any)["completed"] != float64(0) {
		t.Fatalf("before completed = %v, want 0", before["progress"].(map[string]any)["completed"])
	}
	s.setUnitStatus(t, u1, "DONE")
	s.setUnitStatus(t, u2, "DONE")
	_, after := s.get(t)
	prog := after["progress"].(map[string]any)
	if prog["completed"] != float64(2) || !approx(prog["percentage"].(float64), 100) {
		t.Fatalf("after = %v, want 2/2 100%%", prog)
	}
}

// T3.21.6 — Added work unit changes denominator.
func TestT3216AddedUnitDenominator(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p321 denom")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "PENDING")
	_, before := s.get(t)
	if before["progress"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("before total = %v, want 1", before["progress"].(map[string]any)["total"])
	}
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, after := s.get(t)
	if after["progress"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("after total = %v, want 2", after["progress"].(map[string]any)["total"])
	}
}
