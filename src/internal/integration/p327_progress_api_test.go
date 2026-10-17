package integration

import (
	"net/http"
	"strings"
	"testing"
)

// P3-27 API-level progress data (WBS 3.27, T3.27.1-T3.27.7).
// Decision recorded in ACTIVE_TASK NOTES: the WBS 3.27 response shape
// {completed,total,remaining,percentage} is exposed on the existing
// GET /api/v1/projects/{id}/summary progress block (extended with
// "remaining"), sourced from the Phase 2 calculation only.

func p327Progress(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	prog, _ := out["progress"].(map[string]any)
	if prog == nil {
		t.Fatalf("progress missing: %+v", out)
	}
	return prog
}

func p327checkShape(t *testing.T, prog map[string]any, completed, total, remaining float64, pct float64) {
	t.Helper()
	if prog["completed"] != completed || prog["total"] != total {
		t.Fatalf("completed/total = %v/%v, want %v/%v", prog["completed"], prog["total"], completed, total)
	}
	if prog["remaining"] != remaining {
		t.Fatalf("remaining = %v, want %v", prog["remaining"], remaining)
	}
	if !approx(prog["percentage"].(float64), pct) {
		t.Fatalf("percentage = %v, want ~%v", prog["percentage"], pct)
	}
}

// T3.27.1 — Progress derived from work units (WBS example: 36/50 → 72.0).
func TestT3271DerivedFromWorkUnits(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 derive")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	for i := 1; i <= 36; i++ {
		s.addUnit(t, plan, i, "done"+itoa(i), "DONE")
	}
	for i := 37; i <= 50; i++ {
		s.addUnit(t, plan, i, "open"+itoa(i), "PENDING")
	}
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 36, 50, 14, 72.0)
}

// T3.27.2 — Client cannot set progress: a write attempt with a progress
// payload is rejected and the value stays server-computed.
func TestT3272ClientCannotSetProgress(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 immutable")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "DONE")
	s.addUnit(t, plan, 2, "b", "PENDING")
	// Force a rejection: a write carrying a client-chosen percentage.
	req, _ := http.NewRequest(http.MethodPut, summaryURL(t, h, s.projectID.String()),
		strings.NewReader(`{"progress":{"completed":1,"total":2,"percentage":99}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("write attempt accepted with %d, want rejection", resp.StatusCode)
	}
	// The reported progress is unchanged (server-computed 50%).
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 1, 2, 1, 50)
}

// T3.27.3 — EXECUTING doesn't count.
func TestT3273ExecutingDoesNotCount(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 executing")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "EXECUTING")
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 0, 2, 2, 0)
}

// T3.27.4 — INCOMPLETE doesn't count.
func TestT3274IncompleteDoesNotCount(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 incomplete")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "INCOMPLETE")
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 0, 2, 2, 0)
}

// T3.27.5 — VALIDATING doesn't count.
func TestT3275ValidatingDoesNotCount(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 validating")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "VALIDATING")
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 0, 2, 2, 0)
}

// T3.27.6 — DONE counts.
func TestT3276DoneCounts(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 done")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "DONE")
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, out := s.get(t)
	p327checkShape(t, p327Progress(t, out), 1, 2, 1, 50)
}

// T3.27.7 — Added work unit recalculates percentage.
func TestT3277AddedUnitRecalculates(t *testing.T) {
	h := NewHarness(t)
	_, s := summaryRig(t, h, "p327 recalc")
	ph := s.addPhase(t, 1, "Auth", "EXECUTING")
	plan := s.addPlan(t, ph, 1, 1, "READY")
	s.addUnit(t, plan, 1, "a", "DONE")
	s.addUnit(t, plan, 2, "b", "PENDING")
	_, before := s.get(t)
	p327checkShape(t, p327Progress(t, before), 1, 2, 1, 50)
	s.addUnit(t, plan, 3, "c", "PENDING")
	_, after := s.get(t)
	p327checkShape(t, p327Progress(t, after), 1, 3, 2, 33.33)
}
