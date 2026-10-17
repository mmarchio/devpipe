// P4-16 (TDD RED): tests for the phase-completion service operation (WBS
// 4.22). Eligible phase in VALIDATING completes to DONE; ineligible phases
// are rejected; completion is idempotent (an already-DONE phase is a no-op
// that returns nil and preserves the original completed_at); completing a
// phase never advances the next phase.
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/service"
)

// newPhaseServiceP416 wires the real PhaseService to the real repositories,
// the plan and work-unit repositories, and the transaction pool (WBS 4.22).
func newPhaseServiceP416(t *testing.T, h *Harness) *service.PhaseService {
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

// seedPhase inserts a phase with explicit order/status (setup only; tests
// need direct control of the lifecycle state).
func seedPhaseStatus(t *testing.T, h *Harness, projectID uuid.UUID, order int, name, status string) uuid.UUID {
	t.Helper()
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + projectID.String() + "', " + itoa(order) + ", '" + name + "', '" + status + "')"
	if _, err := h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	return phaseID
}

// p416ParseUUID parses the project ID created through the service.
func p416ParseUUID(t *testing.T, projectID string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	return parsed
}

// seedPlan inserts an execution plan for the phase (setup only).
func seedPlan(t *testing.T, h *Harness, phaseID uuid.UUID, wbsVersion, version int, status string) uuid.UUID {
	t.Helper()
	planID := uuid.New()
	sql := "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('" +
		planID.String() + "', '" + phaseID.String() + "', " + itoa(wbsVersion) + ", " + itoa(version) + ", '" + status + "')"
	if _, err := h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return planID
}

// seedUnit inserts a work unit with the given status (setup only).
func seedUnit(t *testing.T, h *Harness, planID uuid.UUID, seq int, name, status string) {
	t.Helper()
	u := uuid.New()
	sql := "INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES ('" +
		u.String() + "', '" + planID.String() + "', " + itoa(seq) + ", '" + name + "', '', 'ATOMIC', '" + status + "', '', '', '[]', '[]', '[]', now())"
	if _, err := h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
}

// T4.22.1 — Eligible phase completes: a VALIDATING phase whose active plan
// has all units DONE transitions to DONE with a completion timestamp, and
// the returned phase reflects the transition.
func TestT4221EligiblePhaseCompletes(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)

	parsed := p416ParseUUID(t, projectID)
	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	p2 := seedPhaseStatus(t, h, parsed, 2, "Two", "PENDING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "DONE")

	got, err := svc.CompletePhase(ctx, projectID, p1.String())
	if err != nil {
		t.Fatalf("CompletePhase: %v", err)
	}
	if got.Status != phase.StatusDONE {
		t.Fatalf("returned status = %s, want DONE", got.Status)
	}
	var status, completed string
	if err := h.DBScan(ctx,
		"SELECT status, coalesce(completed_at::text,'') FROM phases WHERE id = '"+p1.String()+"'",
		&status, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "DONE" || completed == "" {
		t.Fatalf("stored status=%q completed=%q, want DONE with a timestamp", status, completed)
	}
	// No auto-advance: the next phase stays exactly where it was.
	var nextStatus string
	if err := h.DBScan(ctx,
		"SELECT status FROM phases WHERE id = '"+p2.String()+"'", &nextStatus); err != nil {
		t.Fatal(err)
	}
	if nextStatus != "PENDING" {
		t.Fatalf("next phase status = %q, want PENDING (no auto-advance)", nextStatus)
	}
}

// T4.22.2 — Ineligible phase rejected: (a) a phase not in VALIDATING,
// (b) a VALIDATING phase with an unfinished unit, and (c) a VALIDATING
// phase with no execution plan are all rejected before any mutation.
func TestT4222IneligiblePhaseRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	// (a) not in VALIDATING.
	pending := seedPhaseStatus(t, h, parsed, 1, "Pending", "PENDING")
	if _, err := svc.CompletePhase(ctx, projectID, pending.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("PENDING phase: got %v, want ErrInvalidInput", err)
	}

	// (b) unfinished unit.
	p2 := seedPhaseStatus(t, h, parsed, 2, "Unfinished", "VALIDATING")
	plan := seedPlan(t, h, p2, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "PENDING")
	if _, err := svc.CompletePhase(ctx, projectID, p2.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("unfinished units: got %v, want ErrInvalidInput", err)
	}
	var status string
	if err := h.DBScan(ctx,
		"SELECT status FROM phases WHERE id = '"+p2.String()+"'", &status); err != nil {
		t.Fatal(err)
	}
	if status != "VALIDATING" {
		t.Fatalf("rejected phase mutated to %q, want VALIDATING untouched", status)
	}

	// (c) no execution plan.
	p3 := seedPhaseStatus(t, h, parsed, 3, "Unplanned", "VALIDATING")
	if _, err := svc.CompletePhase(ctx, projectID, p3.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("no plan: got %v, want ErrInvalidInput", err)
	}
	if err := h.DBScan(ctx,
		"SELECT status FROM phases WHERE id = '"+p3.String()+"'", &status); err != nil {
		t.Fatal(err)
	}
	if status != "VALIDATING" {
		t.Fatalf("rejected phase mutated to %q, want VALIDATING untouched", status)
	}
}

// T4.22.3 — Completion timestamp stored and readable: the stored
// completed_at is a non-empty instant and the reloaded phase exposes it.
func TestT4223CompletionTimestampStored(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")

	if _, err := svc.CompletePhase(ctx, projectID, p1.String()); err != nil {
		t.Fatalf("CompletePhase: %v", err)
	}
	var completed string
	if err := h.DBScan(ctx,
		"SELECT completed_at::text FROM phases WHERE id = '"+p1.String()+"'", &completed); err != nil {
		t.Fatal(err)
	}
	if completed == "" {
		t.Fatal("completed_at is empty after completion")
	}
	reloaded, err := svc.GetPhase(ctx, projectID, p1.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}
	if reloaded.CompletedAt == nil {
		t.Fatal("reloaded phase CompletedAt = nil, want stored timestamp")
	}
}

// T4.22.4 — Completion operation idempotent according to the defined
// policy: the policy is no-op success — completing an already-DONE phase
// returns nil, leaves status DONE, and preserves the ORIGINAL completed_at
// (the first completion's timestamp is the authoritative one).
func TestT4224CompletionIdempotent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")

	first, err := svc.CompletePhase(ctx, projectID, p1.String())
	if err != nil {
		t.Fatalf("first CompletePhase: %v", err)
	}
	if first.CompletedAt == nil {
		t.Fatal("first completion returned nil CompletedAt")
	}
	firstStored, err := svc.GetPhase(ctx, projectID, p1.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}

	// Defined policy: second call is a no-op success (nil error), not an
	// ErrConflict, and the original timestamp is preserved.
	if _, err := svc.CompletePhase(ctx, projectID, p1.String()); err != nil {
		t.Fatalf("idempotent CompletePhase: got %v, want nil (defined no-op policy)", err)
	}
	secondStored, err := svc.GetPhase(ctx, projectID, p1.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}
	if secondStored.Status != phase.StatusDONE {
		t.Fatalf("status after idempotent call = %s, want DONE", secondStored.Status)
	}
	if !secondStored.CompletedAt.Equal(*firstStored.CompletedAt) {
		t.Fatalf("completed_at changed on idempotent call: %v -> %v", *firstStored.CompletedAt, *secondStored.CompletedAt)
	}
}

// T4.22.5 — Completing a phase does not automatically execute the next
// phase: after completing phase 1, phase 2 keeps its status, and the
// project's current phase is now phase 2 (the lowest-order not-DONE).
func TestT4225NoAutoAdvanceOfNextPhase(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	p2 := seedPhaseStatus(t, h, parsed, 2, "Two", "PENDING")
	p3 := seedPhaseStatus(t, h, parsed, 3, "Three", "PENDING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "DONE")

	if _, err := svc.CompletePhase(ctx, projectID, p1.String()); err != nil {
		t.Fatalf("CompletePhase: %v", err)
	}
	for _, p := range []uuid.UUID{p2, p3} {
		var status string
		if err := h.DBScan(ctx,
			"SELECT status FROM phases WHERE id = '"+p.String()+"'", &status); err != nil {
			t.Fatal(err)
		}
		if status != "PENDING" {
			t.Fatalf("phase %s status = %q after completing phase 1, want PENDING (no auto-advance)", p, status)
		}
	}
	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.ID != p2 {
		t.Fatalf("current phase after completion = %v, want %v", cur, p2)
	}
}

// Guard: the completion operation takes exactly (ctx, projectID, phaseID)
// and returns the completed phase — a signature change breaks the build
// here (the client never supplies a status or timestamp).
var _ = func(svc *service.PhaseService, ctx context.Context, projectID, phaseID string) (*phase.Phase, error) {
	return svc.CompletePhase(ctx, projectID, phaseID)
}
