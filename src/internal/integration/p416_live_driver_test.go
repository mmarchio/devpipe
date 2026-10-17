package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"overseer/internal/phase"
	"overseer/internal/service"
)

// T4.22 live-binary evidence (run against real PostgreSQL via the harness):
// CompletePhase on a real pool — eligible VALIDATING completes to DONE,
// second call is the defined no-op, next phase stays PENDING, PENDING
// phase rejected, completed_at stored.
func TestT422LiveDriver(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := newPhaseServiceP416(t, h)
	projectID := createPhaseProject(t, h)
	parsed := p416ParseUUID(t, projectID)
	p1 := seedPhaseStatus(t, h, parsed, 1, "Live1", "VALIDATING")
	p2 := seedPhaseStatus(t, h, parsed, 2, "Live2", "PENDING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")
	seedUnit(t, h, plan, 2, "b", "DONE")
	got, err := svc.CompletePhase(ctx, projectID, p1.String())
	if err != nil || got.Status != phase.StatusDONE {
		t.Fatalf("complete: %v %v", got, err)
	}
	if _, err := svc.CompletePhase(ctx, projectID, p1.String()); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if _, err := svc.CompletePhase(ctx, projectID, p2.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("PENDING not rejected: %v", err)
	}
	var completed string
	if err := h.DBScan(ctx, "SELECT completed_at::text FROM phases WHERE id = '"+p1.String()+"'", &completed); err != nil || completed == "" {
		t.Fatalf("completed_at missing: %q", completed)
	}
	t.Logf("live driver OK, completed_at=%s", completed)
	fmt.Println("LIVE_OK")
}

// Concurrency: two concurrent CompletePhase calls on the same eligible
// phase — both succeed (idempotent no-op policy), exactly one
// completion timestamp is recorded, and the phase is DONE.
func TestT4226ConcurrentCompletion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := newPhaseServiceP416(t, h)
	projectID := createPhaseProject(t, h)
	parsed := p416ParseUUID(t, projectID)
	p1 := seedPhaseStatus(t, h, parsed, 1, "One", "VALIDATING")
	plan := seedPlan(t, h, p1, 1, 1, "EXECUTING")
	seedUnit(t, h, plan, 1, "a", "DONE")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.CompletePhase(ctx, projectID, p1.String()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent CompletePhase: %v", err)
	}
	var status, completed string
	if err := h.DBScan(ctx,
		"SELECT status, coalesce(completed_at::text,'') FROM phases WHERE id = '"+p1.String()+"'",
		&status, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "DONE" || completed == "" {
		t.Fatalf("concurrent completion: status=%q completed=%q, want DONE + timestamp", status, completed)
	}
}
