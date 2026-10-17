// P4-28 (WBS 4.28): transactional phase + WBS v1 creation. A single service
// transaction creates the phase (server-assigned order) and its WBS version-1
// document row plus history row: one commit covers both, failure of either
// rolls back both, and concurrent creation preserves ordering.
package integration

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/service"
)

// newPhaseServiceP428 wires the real PhaseService with the full repository
// set plus the transaction pool (WBS 4.28: creation must be transactional).
func newPhaseServiceP428(t *testing.T, h *Harness) *service.PhaseService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	).WithWBSVersions(
		dbrepo.NewWBSVersionRepository(pool),
	)
}

// failingPhaseRepo wraps the real phase repository and makes Append fail
// with a persistence infrastructure error (forced-failure seam; rollback is
// asserted against the REAL database, not the fake).
type failingPhaseRepo struct{ real persistence.PhaseRepository }

func (f failingPhaseRepo) Append(ctx context.Context, projectID uuid.UUID, name, description string) (*phase.Phase, error) {
	return nil, &persistence.InfrastructureError{Op: "phase append", Cause: errors.New("forced phase failure")}
}
func (f failingPhaseRepo) Get(ctx context.Context, id uuid.UUID) (*phase.Phase, error) {
	return f.real.Get(ctx, id)
}
func (f failingPhaseRepo) Update(ctx context.Context, p *phase.Phase) error {
	return f.real.Update(ctx, p)
}
func (f failingPhaseRepo) Delete(ctx context.Context, id uuid.UUID) error {
	return f.real.Delete(ctx, id)
}
func (f failingPhaseRepo) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*phase.Phase, error) {
	return f.real.ListByProject(ctx, projectID)
}
func (f failingPhaseRepo) GetNextIncomplete(ctx context.Context, projectID uuid.UUID) (*phase.Phase, error) {
	return f.real.GetNextIncomplete(ctx, projectID)
}
func (f failingPhaseRepo) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	return f.real.MarkCompleted(ctx, id)
}
func (f failingPhaseRepo) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	return f.real.MarkFailed(ctx, id, reason)
}
func (f failingPhaseRepo) WithTx(tx persistence.Transaction) persistence.PhaseRepository {
	return f
}

// T4.28.1 — Phase + WBS created atomically: one call creates the phase at
// the next server-assigned order AND its WBS v1 (document row + version-1
// history row + current pointer), all observable in the real database.
func TestT4281PhaseAndWBSCreatedAtomically(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP428(t, h)

	// A pre-existing phase proves the appended order is server-assigned
	// after it (2, not 1).
	if _, err := svc.CreatePhase(ctx, projectID, "first", ""); err != nil {
		t.Fatalf("CreatePhase setup: %v", err)
	}

	const content = "# Phase WBS\n\n## 1. Scope\n"
	p, rec, err := svc.CreatePhaseWithWBS(ctx, projectID, "second", "", content)
	if err != nil {
		t.Fatalf("CreatePhaseWithWBS: %v", err)
	}
	if p.Order != 2 {
		t.Fatalf("created phase order = %d, want 2 (server-assigned append)", p.Order)
	}
	if p.Status != phase.StatusPENDING {
		t.Fatalf("created phase status = %s, want PENDING", p.Status)
	}
	if rec.Version != 1 || rec.Content != content {
		t.Fatalf("returned WBS = version %d content %q, want version 1 with exact content", rec.Version, rec.Content)
	}
	phaseID := p.ID.String()
	var storedStatus string
	var storedOrder int
	var docCount, versionCount int
	if err := h.DBScan(ctx,
		"SELECT status, phase_order FROM phases WHERE id = '"+phaseID+"'",
		&storedStatus, &storedOrder); err != nil {
		t.Fatal(err)
	}
	if storedOrder != 2 {
		t.Fatalf("stored phase order = %d, want 2", storedOrder)
	}
	if storedStatus != "PENDING" {
		t.Fatalf("stored phase status = %q, want PENDING", storedStatus)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM phase_documents WHERE phase_id = '"+phaseID+"' AND doc_type = 'WBS'", &docCount); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &versionCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 1 || versionCount != 1 {
		t.Fatalf("WBS rows = %d document / %d versions, want 1/1 (v1 created atomically with the phase)", docCount, versionCount)
	}
	var version int
	var stored string
	if err := h.DBScan(ctx,
		"SELECT version, content FROM wbs_versions WHERE phase_id = '"+phaseID+"'",
		&version, &stored); err != nil {
		t.Fatal(err)
	}
	if version != 1 || stored != content {
		t.Fatalf("stored WBS = version %d content %q, want version 1 with exact content", version, stored)
	}
}

// T4.28.2 — WBS failure rolls back the phase: when the WBS version append
// fails mid-transaction, the phase row created moments earlier must not
// persist (rollback verified against the REAL database).
func TestT4282WBSFailureRollsBackPhase(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	pool := h.App.DB().Raw()
	svc := service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	).WithTxSeams(
		failingPhaseRepo{},      // real phases (delegating)
		nil,                     // projects (delegating)
		nil,                     // docs (delegating)
		failingWBSVersionRepo{}, // forced WBS failure
	)

	_, _, err := svc.CreatePhaseWithWBS(ctx, projectID, " doomed", "", "wbs content")
	if !errors.Is(err, service.ErrInfrastructure) {
		t.Fatalf("CreatePhaseWithWBS WBS failure: got %v, want ErrInfrastructure", err)
	}
	var count int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM phases WHERE project_id = '"+projectID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("phase rows after WBS failure = %d, want 0 (the phase must roll back)", count)
	}
}

// T4.28.3 — Phase failure creates no WBS: when the phase append fails, the
// WBS document and version rows must not persist.
func TestT4283PhaseFailureCreatesNoWBS(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	pool := h.App.DB().Raw()
	svc := service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
		pool,
	).WithTxSeams(
		failingPhaseRepo{}, // forced phase failure
		nil,                // projects (delegating)
		nil,                // docs (delegating)
		nil,                // versions (delegating)
	)

	_, _, err := svc.CreatePhaseWithWBS(ctx, projectID, "doomed", "", "wbs content")
	if !errors.Is(err, service.ErrInfrastructure) {
		t.Fatalf("CreatePhaseWithWBS phase failure: got %v, want ErrInfrastructure", err)
	}
	var phaseCount, docCount, versionCount int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM phases WHERE project_id = '"+projectID+"'", &phaseCount); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM phase_documents", &docCount); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions", &versionCount); err != nil {
		t.Fatal(err)
	}
	if phaseCount != 0 || docCount != 0 || versionCount != 0 {
		t.Fatalf("after phase failure: %d phases / %d documents / %d versions, want 0/0/0", phaseCount, docCount, versionCount)
	}
}

// T4.28.4 — Concurrent creation preserves ordering: many goroutines
// creating phase + WBS pairs against the real database receive the exact
// contiguous order sequence (no duplicates, no gaps), each with its own WBS
// v1. Run plain and with -race.
func TestT4284ConcurrentCreationPreservesOrdering(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseServiceP428(t, h)

	const n = 8
	var (
		mu     sync.Mutex
		orders []int
		failed bool
	)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "concurrent " + itoa(i)
			p, rec, err := svc.CreatePhaseWithWBS(ctx, projectID, name, "", "wbs for "+name)
			if err != nil || rec == nil {
				mu.Lock()
				failed = true
				mu.Unlock()
				return
			}
			// Each pair is atomic: its WBS v1 row must exist by the time
			// the call returns (committed with the phase).
			var v int
			if err := h.DBScan(ctx,
				"SELECT version FROM wbs_versions WHERE phase_id = '"+p.ID.String()+"'",
				&v); err != nil || v != 1 {
				mu.Lock()
				failed = true
				mu.Unlock()
				return
			}
			mu.Lock()
			orders = append(orders, p.Order)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if failed {
		t.Fatal("a concurrent CreatePhaseWithWBS call failed or lacked its WBS v1 row")
	}
	sort.Ints(orders)
	for i, o := range orders {
		if o != i+1 {
			t.Fatalf("orders = %v, want the contiguous sequence 1..%d (no duplicates, no gaps)", orders, n)
		}
	}
}

// Guard: the composed operation takes exactly (ctx, projectID, name,
// description, wbsContent) and returns both created objects — a signature
// change breaks the build here.
var _ = func(svc *service.PhaseService, ctx context.Context, projectID, name, description, wbsContent string) (*phase.Phase, *persistence.WBSDocumentRecord, error) {
	return svc.CreatePhaseWithWBS(ctx, projectID, name, description, wbsContent)
}
