// P4-17 (TDD RED): tests for the phase-failure service operation (WBS
// 4.23). A phase can be marked failed from the ALLOWED STATES — the
// deliberate choice for this task is the in-flight set EXECUTING and
// VALIDATING (a failure is a record of work that was in progress; a
// PENDING/PLANNING/READY phase has no in-flight work, and a DONE phase is
// finished) — with the failure reason and a failure timestamp stored, and
// the WBS document and execution plan retained. A failed phase remains the
// project's current phase until resolved per future recovery rules.
package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/service"
)

// T4.23.1 — Phase can be marked failed from allowed state. The allowed
// states (deliberate choice, stated here and in the completion report) are
// the in-flight set: EXECUTING and VALIDATING. Non-allowed states
// (PENDING, PLANNING, READY, DONE, already FAILED) are rejected before any
// mutation.
func TestT4231MarkFailedFromAllowedStates(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	executing := seedPhaseStatus(t, h, parsed, 1, "Executing", "EXECUTING")
	_, err := svc.MarkPhaseFailed(ctx, projectID, executing.String(), "worker crashed")
	if err != nil {
		t.Fatalf("EXECUTING phase: MarkPhaseFailed: %v", err)
	}
	var status string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+executing.String()+"'", &status); err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("stored status = %q, want FAILED", status)
	}

	validating := seedPhaseStatus(t, h, parsed, 2, "Validating", "VALIDATING")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, validating.String(), "validation errors"); err != nil {
		t.Fatalf("VALIDATING phase: MarkPhaseFailed: %v", err)
	}

	nonAllowed := map[int]string{3: "PENDING", 4: "PLANNING", 5: "READY", 6: "DONE"}
	for order, st := range nonAllowed {
		id := seedPhaseStatus(t, h, parsed, order, "Phase"+st, st)
		if _, err := svc.MarkPhaseFailed(ctx, projectID, id.String(), "nope"); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("%s phase: got %v, want ErrInvalidInput", st, err)
		}
		var now string
		if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+id.String()+"'", &now); err != nil {
			t.Fatal(err)
		}
		if now != st {
			t.Fatalf("rejected phase mutated to %q, want %q untouched", now, st)
		}
	}
}

// T4.23.2 — Failure reason retained: the stored failure_reason survives a
// reload and is exposed on the phase; an empty reason is rejected before
// any mutation; failing an already-FAILED phase is the defined no-op that
// preserves the ORIGINAL reason and the original failed_at.
func TestT4232FailureReasonRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	// Empty reason rejected, nothing mutated.
	empty := seedPhaseStatus(t, h, parsed, 1, "Empty", "EXECUTING")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, empty.String(), "   "); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("empty reason: got %v, want ErrInvalidInput", err)
	}

	p := seedPhaseStatus(t, h, parsed, 2, "Reasoned", "EXECUTING")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, p.String(), "unit 2 build failed"); err != nil {
		t.Fatalf("MarkPhaseFailed: %v", err)
	}
	var stored string
	if err := h.DBScan(ctx, "SELECT failure_reason FROM phases WHERE id = '"+p.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != "unit 2 build failed" {
		t.Fatalf("stored failure_reason = %q, want the given reason", stored)
	}
	reloaded, err := svc.GetPhase(ctx, projectID, p.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}
	if reloaded.FailureReason == nil || *reloaded.FailureReason != "unit 2 build failed" {
		t.Fatalf("reloaded FailureReason = %q, want the stored reason", *reloaded.FailureReason)
	}

	// Defined no-op policy: an already-FAILED phase keeps its first reason
	// and its original failed_at.
	first, err := svc.GetPhase(ctx, projectID, p.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}
	if _, err := svc.MarkPhaseFailed(ctx, projectID, p.String(), "second reason"); err != nil {
		t.Fatalf("idempotent MarkPhaseFailed: got %v, want nil (defined no-op policy)", err)
	}
	second, err := svc.GetPhase(ctx, projectID, p.String())
	if err != nil {
		t.Fatalf("GetPhase: %v", err)
	}
	if second.FailureReason == nil || *second.FailureReason != "unit 2 build failed" {
		t.Fatalf("failure_reason after no-op = %q, want the original", *second.FailureReason)
	}
	if !second.FailedAt.Equal(*first.FailedAt) {
		t.Fatalf("failed_at changed on no-op: %v -> %v", *first.FailedAt, *second.FailedAt)
	}
}

// T4.23.3 — WBS retained: after the phase is marked failed, the WBS
// document row and its content are exactly as before.
func TestT4233WBSRetainedAfterFailure(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	seedWBSDoc(t, h, p.String(), "# WBS v1")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, p.String(), "worker crashed"); err != nil {
		t.Fatalf("MarkPhaseFailed: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM phase_documents WHERE phase_id = '"+p.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("phase_documents rows after failure = %d, want 1", n)
	}
	var content string
	if err := h.DBScan(ctx, "SELECT content FROM phase_documents WHERE phase_id = '"+p.String()+"'", &content); err != nil {
		t.Fatal(err)
	}
	if content != "# WBS v1" {
		t.Fatalf("WBS content after failure = %q, want unchanged", content)
	}
}

// T4.23.4 — Execution plan retained: the plan and its work units survive
// the failure untouched (same plan ID, same status, same unit count).
func TestT4234ExecutionPlanRetainedAfterFailure(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p := seedPhaseStatus(t, h, parsed, 1, "One", "EXECUTING")
	plan := seedPlan(t, h, p, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "EXECUTING")

	if _, err := svc.MarkPhaseFailed(ctx, projectID, p.String(), "unit b hung"); err != nil {
		t.Fatalf("MarkPhaseFailed: %v", err)
	}
	var storedPlan string
	var planStatus string
	if err := h.DBScan(ctx,
		"SELECT id, status FROM execution_plans WHERE phase_id = '"+p.String()+"'",
		&storedPlan, &planStatus); err != nil {
		t.Fatal(err)
	}
	if storedPlan != plan.String() || planStatus != "EXECUTING" {
		t.Fatalf("plan after failure = %s/%s, want %s/EXECUTING (retained)", storedPlan, planStatus, plan)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM work_units WHERE execution_plan_id = '"+plan.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("work units after failure = %d, want 2", n)
	}
}

// T4.23.5 — Failed phase remains current until resolved: a FAILED phase is
// not DONE, so GetCurrentPhase (lowest-order not-DONE) and the repository's
// GetNextIncomplete both still resolve to it, ahead of a later PENDING
// phase.
func TestT4235FailedPhaseRemainsCurrent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "EXECUTING")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, p1.String(), "worker crashed"); err != nil {
		t.Fatalf("MarkPhaseFailed: %v", err)
	}
	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.ID != p1 {
		t.Fatalf("current phase after failure = %v, want the FAILED phase %v", cur, p1)
	}
	pool := h.App.DB().Raw()
	next, err := dbrepo.NewPhaseRepository(pool).GetNextIncomplete(ctx, parsed)
	if err != nil {
		t.Fatalf("GetNextIncomplete: %v", err)
	}
	if next == nil || next.ID != p1 {
		t.Fatalf("GetNextIncomplete after failure = %v, want the FAILED phase %v", next, p1)
	}
}

// Compile-level guard: the failure operation takes exactly
// (ctx, projectID, phaseID, reason) and returns the failed phase — a
// signature change breaks the build here.
var _ = func(svc *service.PhaseService, ctx context.Context, projectID, phaseID, reason string) (*phase.Phase, error) {
	return svc.MarkPhaseFailed(ctx, projectID, phaseID, reason)
}

// P4-17 migration: the inverse of 00013 (the runner applies .up.sql files;
// the down half is exercised here exactly as it would run on rollback)
// drops both columns and leaves pre-existing data rows intact.
func TestP417DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP416(t, h)
	parsed := p416ParseUUID(t, projectID)

	p := seedPhaseStatus(t, h, parsed, 1, "One", "EXECUTING")
	if _, err := svc.MarkPhaseFailed(ctx, projectID, p.String(), "worker crashed"); err != nil {
		t.Fatalf("MarkPhaseFailed: %v", err)
	}
	// Inverse of 00013.
	if _, err := h.DBExec(ctx, "ALTER TABLE phases DROP COLUMN failure_reason"); err != nil {
		t.Fatalf("down: drop failure_reason: %v", err)
	}
	if _, err := h.DBExec(ctx, "ALTER TABLE phases DROP COLUMN failed_at"); err != nil {
		t.Fatalf("down: drop failed_at: %v", err)
	}
	var n int
	for _, col := range []string{"failure_reason", "failed_at"} {
		if err := h.DBScan(ctx,
			"SELECT count(*) FROM information_schema.columns WHERE table_name = 'phases' AND column_name = '"+col+"'",
			&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("column %s still present after down migration", col)
		}
	}
	// Pre-existing data rows are intact: the phase row survives with its
	// name and status.
	var name, status string
	if err := h.DBScan(ctx, "SELECT name, status FROM phases WHERE id = '"+p.String()+"'", &name, &status); err != nil {
		t.Fatal(err)
	}
	if name != "One" || status != "FAILED" {
		t.Fatalf("pre-existing row after down = %q/%q, want One/FAILED", name, status)
	}
}
