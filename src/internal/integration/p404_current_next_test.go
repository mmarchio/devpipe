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

// markPhaseDone flips a phase to DONE through the real repository,
// bypassing the service (no service-side completion transition exists
// yet in Phase 4).
func markPhaseDone(t *testing.T, h *Harness, phaseID string) {
	t.Helper()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPhaseRepository(pool)
	if err := repo.MarkCompleted(context.Background(), uuid.MustParse(phaseID)); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
}

// T4.7.1 — Lowest incomplete phase returned: with orders 1 DONE, 2
// DONE, 3 EXECUTING, 4 PENDING, 5 PENDING the current phase is 3.
func TestT471LowestIncompletePhaseReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	markPhaseDone(t, h, ids[0])
	markPhaseDone(t, h, ids[1])
	if err := dbrepo.NewPhaseRepository(h.App.DB().Raw()).MarkStarted(ctx, uuid.MustParse(ids[2])); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}

	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil {
		t.Fatal("GetCurrentPhase = nil, want order 3")
	}
	if cur.Order != 3 || cur.ID.String() != ids[2] {
		t.Fatalf("current = order %d, want 3", cur.Order)
	}
}

// T4.7.2 — DONE phases skipped, independent of timestamps: creation
// timestamps are scrambled (phase 1 created last), and DONE phases are
// skipped purely by status/order.
func TestT472DonePhasesSkipped(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 4)
	for i := 0; i < 4; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	// Scramble creation timestamps so any timestamp-based logic would
	// pick a different phase: phase 1 most recent, phase 4 oldest.
	for i, ts := range []string{"now() + interval '3 days'", "now() + interval '2 days'", "now() + interval '1 day'", "now() - interval '1 day'"} {
		if _, err := h.DBExec(ctx, "UPDATE phases SET created_at = "+ts+" WHERE id = '"+ids[i]+"'"); err != nil {
			t.Fatal(err)
		}
	}
	markPhaseDone(t, h, ids[0])
	markPhaseDone(t, h, ids[1])

	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.Order != 3 {
		t.Fatalf("current = %v, want order 3 (skipped by status, not timestamp)", cur)
	}
}

// T4.7.3 — Order gaps handled: with order 2 deleted, the resolution
// walks the surviving orders and never errors or skips the gap.
func TestT473OrderGapsHandled(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	if err := svc.DeletePhase(ctx, projectID, ids[1]); err != nil { // gap at order 2
		t.Fatalf("DeletePhase: %v", err)
	}
	markPhaseDone(t, h, ids[0])

	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.Order != 3 {
		t.Fatalf("current = %v, want order 3 (gap at 2 must not break resolution)", cur)
	}
}

// T4.7.4 — No incomplete phase returns empty result: all phases DONE
// yields (nil, nil); a project with no phases also yields (nil, nil).
func TestT474NoIncompletePhaseEmptyResult(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	for _, name := range []string{"a", "b"} {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase: %v", err)
		}
		markPhaseDone(t, h, p.ID.String())
	}
	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase (all DONE): %v", err)
	}
	if cur != nil {
		t.Fatalf("GetCurrentPhase (all DONE) = %+v, want nil", cur)
	}

	empty := createPhaseProject(t, h)
	cur, err = svc.GetCurrentPhase(ctx, empty)
	if err != nil {
		t.Fatalf("GetCurrentPhase (no phases): %v", err)
	}
	if cur != nil {
		t.Fatalf("GetCurrentPhase (no phases) = %+v, want nil", cur)
	}
}

// T4.7.5 — Newly appended future phase doesn't become current
// prematurely: with order 3 already executing, appending order 6 leaves
// the current phase at 3.
func TestT475AppendedPhaseNotPrematureCurrent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	markPhaseDone(t, h, ids[0])
	markPhaseDone(t, h, ids[1])
	if err := dbrepo.NewPhaseRepository(h.App.DB().Raw()).MarkStarted(ctx, uuid.MustParse(ids[2])); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}

	if _, err := svc.CreatePhase(ctx, projectID, "future", ""); err != nil { // order 4
		t.Fatalf("CreatePhase: %v", err)
	}
	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatalf("GetCurrentPhase: %v", err)
	}
	if cur == nil || cur.Order != 3 {
		t.Fatalf("current = %v, want order 3 (the appended phase must not become current)", cur)
	}
}

// T4.8.1 — Next ordered phase returned: from order 3 the next phase is
// the smallest order greater than 3 (order 4 of 1..5).
func TestT481NextOrderedPhaseReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}

	next, err := svc.GetNextPhase(ctx, projectID, ids[2])
	if err != nil {
		t.Fatalf("GetNextPhase: %v", err)
	}
	if next == nil || next.Order != 4 || next.ID.String() != ids[3] {
		t.Fatalf("next = %v, want order 4", next)
	}
}

// T4.8.2 — Order gaps handled: with orders {1,2,3,5} (4 deleted), the
// next phase after 3 is order 5.
func TestT482OrderGapsHandled(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		ids[i] = p.ID.String()
	}
	if err := svc.DeletePhase(ctx, projectID, ids[3]); err != nil { // gap at order 4
		t.Fatalf("DeletePhase: %v", err)
	}

	next, err := svc.GetNextPhase(ctx, projectID, ids[2])
	if err != nil {
		t.Fatalf("GetNextPhase: %v", err)
	}
	if next == nil || next.Order != 5 {
		t.Fatalf("next = %v, want order 5 (gap at 4 skipped)", next)
	}
}

// T4.8.3 — Last phase returns no next phase: (nil, nil), no error.
func TestT483LastPhaseNoNext(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	var lastID string
	for i := 0; i < 3; i++ {
		p, err := svc.CreatePhase(ctx, projectID, "phase", "")
		if err != nil {
			t.Fatalf("CreatePhase %d: %v", i+1, err)
		}
		lastID = p.ID.String()
	}

	next, err := svc.GetNextPhase(ctx, projectID, lastID)
	if err != nil {
		t.Fatalf("GetNextPhase (last): %v", err)
	}
	if next != nil {
		t.Fatalf("GetNextPhase (last) = %+v, want nil", next)
	}
}

// T4.8.4 — Phase from another project rejected: a phase belonging to a
// different project is ErrNotFound, and a malformed phase id is
// ErrInvalidInput.
func TestT484CrossProjectRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := newPhaseService(t, h)

	otherID := createPhaseProject(t, h)
	other, err := svc.CreatePhase(ctx, otherID, "foreign", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	projectID := createPhaseProject(t, h)
	if _, err := svc.CreatePhase(ctx, projectID, "local", ""); err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}

	if _, err := svc.GetNextPhase(ctx, projectID, other.ID.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.GetNextPhase(ctx, projectID, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed phase id: got %v, want ErrInvalidInput", err)
	}
}

// Guard: the service signatures are exactly (ctx, projectID) and
// (ctx, projectID, phaseID) — both returning (*phase.Phase, error).
var _ = func(svc *service.PhaseService, ctx context.Context, projectID, phaseID string) {
	_ = func() (*phase.Phase, error) { return svc.GetCurrentPhase(ctx, projectID) }
	_ = func() (*phase.Phase, error) { return svc.GetNextPhase(ctx, projectID, phaseID) }
}
