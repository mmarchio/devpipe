package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// countingExecutor is a persistence.Executor over the harness pool that
// counts every executed statement (Exec, Query, QueryRow) — the
// measurement vehicle for T4.24.5 (query count bounded as phase count
// grows).
type countingExecutor struct {
	pool  *pgxpool.Pool
	calls atomic.Int64
}

func (c *countingExecutor) Exec(ctx context.Context, sql string, args ...any) (persistence.CommandTag, error) {
	c.calls.Add(1)
	return c.pool.Exec(ctx, sql, args...)
}
func (c *countingExecutor) Query(ctx context.Context, sql string, args ...any) (persistence.Rows, error) {
	c.calls.Add(1)
	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rowsCounter{rows}, nil
}
func (c *countingExecutor) QueryRow(ctx context.Context, sql string, args ...any) persistence.QueryRowScanner {
	c.calls.Add(1)
	return c.pool.QueryRow(ctx, sql, args...)
}

// rowsCounter passes a driver cursor through the domain Rows surface.
type rowsCounter struct {
	rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
}

func (r rowsCounter) Next() bool             { return r.rows.Next() }
func (r rowsCounter) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r rowsCounter) Close()                 { r.rows.Close() }
func (r rowsCounter) Err() error             { return r.rows.Err() }

func newCountingExecutor(h *Harness) *countingExecutor {
	return &countingExecutor{pool: h.App.DB().Raw()}
}

// newPhaseListSummaryServiceTx wires the transactional (pool) path with a
// counting executor for the set-based plan/unit reads.
func newPhaseListSummaryServiceTx(t *testing.T, h *Harness) (*service.PhaseListSummaryService, *countingExecutor) {
	t.Helper()
	pool := h.App.DB().Raw()
	c := newCountingExecutor(h)
	return service.NewPhaseListSummaryService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewPlanRepositoryForExecutor(c),
		dbrepo.NewWorkUnitRepositoryForExecutor(c),
		pool,
	), c
}

// newPhaseListSummaryServiceSeq wires the sequential (nil-pool) fallback.
// newPhaseListSummaryServiceSeq wires the sequential (nil-tx-pool)
// fallback. phase/project repos need a live executor (they are not
// counted); plans/units run on the counting executor.
func newPhaseListSummaryServiceSeq(h *Harness, c *countingExecutor) *service.PhaseListSummaryService {
	return service.NewPhaseListSummaryService(
		dbrepo.NewPhaseRepository(h.App.DB().Raw()),
		dbrepo.NewProjectRepository(h.App.DB().Raw()),
		dbrepo.NewPlanRepositoryForExecutor(c),
		dbrepo.NewWorkUnitRepositoryForExecutor(c),
		nil,
	)
}

// setupRoadmapProject creates a project with n phases; phases numbered
// plannedFrom (1-based, 0 = none planned) onward get a WBS v1, a plan, and
// two work units (first DONE, second PENDING, 50 percent); earlier phases
// stay unplanned (0 percent). Returns (projectID, phaseIDs in ascending order).
func setupRoadmapProject(t *testing.T, h *Harness, n, plannedFrom int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	ph := newPhaseService(t, h)
	pool := h.App.DB().Raw()
	docs := newVersionedWBSDocService(t, h).WithPlans(dbrepo.NewPlanRepository(pool))
	units := dbrepo.NewWorkUnitRepository(pool)
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		if _, err := ph.CreatePhase(ctx, projectID, "p"+itoa(i), ""); err != nil {
			t.Fatalf("CreatePhase %d: %v", i, err)
		}
		var id string
		if err := h.DBScan(ctx,
			"SELECT id::text FROM phases WHERE project_id = '"+projectID+"' AND phase_order = "+itoa(i),
			&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if i >= plannedFrom {
			if _, err := docs.CreateWBS(ctx, projectID, id, "# v1"); err != nil {
				t.Fatalf("CreateWBS %d: %v", i, err)
			}
			plan, err := docs.CreatePlan(ctx, projectID, id, 1)
			if err != nil {
				t.Fatalf("CreatePlan %d: %v", i, err)
			}
			done, _ := wbsNewUnit(t, plan.ID, 1, "done")
			done.Status = wbs.WorkUnitStatusDONE
			if err := units.Create(ctx, done); err != nil {
				t.Fatalf("Create unit %d: %v", i, err)
			}
			pending, _ := wbsNewUnit(t, plan.ID, 2, "pending")
			if err := units.Create(ctx, pending); err != nil {
				t.Fatalf("Create unit %d: %v", i, err)
			}
		}
	}
	return projectID, ids
}

// T4.24.1 — All phases returned in order: the summary lists every phase of
// the project in ascending phase order, one entry per phase.
func TestT4241AllPhasesInOrder(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _ := setupRoadmapProject(t, h, 5, 3)

	svc, _ := newPhaseListSummaryServiceTx(t, h)
	got, err := svc.PhaseListSummary(ctx, projectID)
	if err != nil {
		t.Fatalf("PhaseListSummary: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("summary has %d entries, want 5", len(got))
	}
	for i, e := range got {
		if e.Order != i+1 {
			t.Fatalf("entry %d order = %d, want %d (ascending phase order)", i, e.Order, i+1)
		}
	}

	// Error contract: unknown project -> ErrNotFound; malformed id ->
	// ErrInvalidInput.
	if _, err := svc.PhaseListSummary(ctx, uuid.Nil.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
	if _, err := svc.PhaseListSummary(ctx, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed project id: got %v, want ErrInvalidInput", err)
	}

	// Sequential (nil-pool) fallback must agree with the transactional path.
	seq := newPhaseListSummaryServiceSeq(h, newCountingExecutor(h))
	gotSeq, err := seq.PhaseListSummary(ctx, projectID)
	if err != nil {
		t.Fatalf("PhaseListSummary (sequential): %v", err)
	}
	if len(gotSeq) != len(got) {
		t.Fatalf("sequential summary has %d entries, want %d", len(gotSeq), len(got))
	}
	for i := range got {
		if gotSeq[i] != got[i] {
			t.Fatalf("sequential entry %d = %+v, transactional %+v", i, gotSeq[i], got[i])
		}
	}
}

// T4.24.2 — Status included: every summary entry carries its phase's
// lifecycle status (PENDING for unplanned phases, EXECUTING for the phase
// driven into execution).
func TestT4242StatusIncluded(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseIDs := setupRoadmapProject(t, h, 3, 2)
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'EXECUTING' WHERE id = '"+phaseIDs[1]+"'"); err != nil {
		t.Fatal(err)
	}

	svc, _ := newPhaseListSummaryServiceTx(t, h)
	got, err := svc.PhaseListSummary(ctx, projectID)
	if err != nil {
		t.Fatalf("PhaseListSummary: %v", err)
	}
	wantStatuses := []phase.PhaseStatus{phase.StatusPENDING, phase.StatusEXECUTING, phase.StatusPENDING}
	for i, e := range got {
		if e.Status != wantStatuses[i] {
			t.Fatalf("entry order %d status = %v, want %v", e.Order, e.Status, wantStatuses[i])
		}
	}
}

// T4.24.3 — Progress included: entries for planned phases carry the
// derived work-unit progress (completed/total/percentage, DONE-only
// counting).
func TestT4243ProgressIncluded(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _ := setupRoadmapProject(t, h, 3, 2)

	svc, _ := newPhaseListSummaryServiceTx(t, h)
	got, err := svc.PhaseListSummary(ctx, projectID)
	if err != nil {
		t.Fatalf("PhaseListSummary: %v", err)
	}
	for i, e := range got {
		if i == 0 {
			continue
		}
		if e.Progress.Completed != 1 || e.Progress.Total != 2 {
			t.Fatalf("entry order %d progress = %+v, want 1/2", e.Order, e.Progress)
		}
		if e.Progress.Percentage != 50 {
			t.Fatalf("entry order %d percentage = %v, want 50", e.Order, e.Progress.Percentage)
		}
	}
}

// T4.24.4 — Unplanned phase reports 0%: a phase with no WBS/plan yields a
// defined 0/0/0 progress entry (never an error, never nil).
func TestT4244UnplannedPhaseReportsZero(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _ := setupRoadmapProject(t, h, 2, 2) // only the last phase planned

	svc, _ := newPhaseListSummaryServiceTx(t, h)
	got, err := svc.PhaseListSummary(ctx, projectID)
	if err != nil {
		t.Fatalf("PhaseListSummary: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("summary has %d entries, want 2", len(got))
	}
	first := got[0]
	if first.Progress.Completed != 0 || first.Progress.Total != 0 || first.Progress.Percentage != 0 {
		t.Fatalf("unplanned progress = %+v, want 0/0/0", first.Progress)
	}
	second := got[1]
	if second.Progress.Completed != 1 || second.Progress.Total != 2 || second.Progress.Percentage != 50 {
		t.Fatalf("planned progress = %+v, want 1/2/50", second.Progress)
	}
}

// T4.24.5 — Query count remains bounded as phase count grows: the
// transactional summary's statement count is identical for 5 and 15 phases
// (set-based reads, no per-phase round-trip), and the sequential fallback
// is bounded the same way.
func TestT4245QueryCountBounded(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	small, _ := setupRoadmapProject(t, h, 5, 1)
	large, _ := setupRoadmapProject(t, h, 15, 1)

	svc5, c5 := newPhaseListSummaryServiceTx(t, h)
	if _, err := svc5.PhaseListSummary(ctx, small); err != nil {
		t.Fatalf("PhaseListSummary (5 phases): %v", err)
	}
	q5 := c5.calls.Load()

	svc15, c15 := newPhaseListSummaryServiceTx(t, h)
	if _, err := svc15.PhaseListSummary(ctx, large); err != nil {
		t.Fatalf("PhaseListSummary (15 phases): %v", err)
	}
	q15 := c15.calls.Load()

	if q15 != q5 {
		t.Fatalf("transactional query count: 5 phases = %d, 15 phases = %d — count must stay bounded", q5, q15)
	}

	// Sequential fallback: same boundedness.
	cSeq5 := newCountingExecutor(h)
	if _, err := newPhaseListSummaryServiceSeq(h, cSeq5).PhaseListSummary(ctx, small); err != nil {
		t.Fatalf("PhaseListSummary sequential (5): %v", err)
	}
	s5 := cSeq5.calls.Load()
	cSeq15 := newCountingExecutor(h)
	if _, err := newPhaseListSummaryServiceSeq(h, cSeq15).PhaseListSummary(ctx, large); err != nil {
		t.Fatalf("PhaseListSummary sequential (15): %v", err)
	}
	s15 := cSeq15.calls.Load()
	if s15 != s5 {
		t.Fatalf("sequential query count: 5 phases = %d, 15 phases = %d — count must stay bounded", s5, s15)
	}
}
