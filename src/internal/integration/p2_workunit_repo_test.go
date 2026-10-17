package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// workUnitRig bundles a harness with the work unit repository.
type workUnitRig struct {
	h    *Harness
	repo *dbrepo.WorkUnitRepository
}

func newWorkUnitRig(t *testing.T) *workUnitRig {
	t.Helper()
	h := NewHarness(t)
	return &workUnitRig{h: h, repo: dbrepo.NewWorkUnitRepository(h.App.DB().Raw())}
}

func (r *workUnitRig) plan(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pid := uuid.New()
	if _, err := r.h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	phaseID := uuid.New()
	sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
		phaseID.String() + "', '" + pid.String() + "', 1, 'Phase', 'PENDING')"
	if _, err := r.h.DBExec(ctx, sql); err != nil {
		t.Fatalf("phase: %v", err)
	}
	planID := uuid.New()
	sql = "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES ('" +
		planID.String() + "', '" + phaseID.String() + "', 1, 1, 'READY')"
	if _, err := r.h.DBExec(ctx, sql); err != nil {
		t.Fatalf("plan: %v", err)
	}
	return planID
}

func (r *workUnitRig) unit(t *testing.T, planID uuid.UUID, seq int, name string) *wbs.WorkUnit {
	t.Helper()
	ctx := context.Background()
	wu, err := wbs.NewWorkUnit(planID, seq, name, wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit: %v", err)
	}
	spec, err := wbs.NewWorkUnitSpecification(wbs.WorkUnitSpecification{
		Objective:          "implement " + name,
		AcceptanceCriteria: []string{"accepts valid input", "rejects malformed input"},
		RequiredTests:      []string{"valid case", "invalid case"},
		Constraints:        []string{"touch only the target code"},
	})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}
	wu.Specification = spec
	if err := r.repo.Create(ctx, wu); err != nil {
		t.Fatalf("create unit: %v", err)
	}
	return wu
}

// T2.16.1 — Work unit created.
func TestT2161WorkUnitCreated(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	wu := r.unit(t, planID, 1, "unit-one")
	got, err := r.repo.Get(ctx, wu.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != wu.ID || got.ExecutionPlanID != planID || got.Sequence != 1 || got.Name != "unit-one" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Kind != wbs.WorkUnitKindATOMIC || got.Status != wbs.WorkUnitStatusPENDING {
		t.Fatalf("kind/status = %v/%v, want ATOMIC/PENDING", got.Kind, got.Status)
	}
	if got.Specification == nil ||
		got.Specification.Objective != "implement unit-one" ||
		len(got.Specification.AcceptanceCriteria) != 2 ||
		len(got.Specification.RequiredTests) != 2 ||
		len(got.Specification.Constraints) != 1 {
		t.Fatalf("spec round trip mismatch: %+v", got.Specification)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not stored")
	}
	if got.StartedAt != nil || got.CompletedAt != nil {
		t.Fatalf("fresh unit must have null start/completion: %+v", got)
	}
	// Unknown id -> ErrNotFound.
	if _, err := r.repo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing unit: got %v, want ErrNotFound", err)
	}
	// Duplicate (plan, sequence) -> ErrConflict.
	dup, _ := wbs.NewWorkUnit(planID, 1, "dup", wbs.WorkUnitKindATOMIC)
	if err := r.repo.Create(ctx, dup); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate sequence: got %v, want ErrConflict", err)
	}
}

// T2.16.2 — Units listed in sequence order.
func TestT2162ListByPlanSequenceOrder(t *testing.T) {
	r := newWorkUnitRig(t)
	planID := r.plan(t)
	other := r.plan(t)
	// Insert out of order across plans, then verify ordering and isolation.
	r.unit(t, planID, 2, "two")
	r.unit(t, planID, 1, "one")
	r.unit(t, planID, 3, "three")
	r.unit(t, other, 1, "other-plan")
	list, err := r.repo.ListByPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d units, want 3", len(list))
	}
	for i, u := range list {
		if u.Sequence != i+1 {
			t.Fatalf("list order at %d = %d, want %d", i, u.Sequence, i+1)
		}
		if u.ExecutionPlanID != planID {
			t.Fatalf("unit %d belongs to %v, want %v", i, u.ExecutionPlanID, planID)
		}
	}
}

// T2.16.3 — Status persisted.
func TestT2163StatusPersisted(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	wu := r.unit(t, planID, 1, "status")
	if err := r.repo.MarkReady(ctx, wu.ID); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	got, err := r.repo.Get(ctx, wu.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != wbs.WorkUnitStatusREADY {
		t.Fatalf("status = %v, want READY", got.Status)
	}
	// Direct status change via Update is persisted as well.
	got.Status = wbs.WorkUnitStatusINCOMPLETE
	if err := r.repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := r.repo.Get(ctx, wu.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Status != wbs.WorkUnitStatusINCOMPLETE {
		t.Fatalf("status = %v, want INCOMPLETE", again.Status)
	}
	// Marking an unknown id -> ErrNotFound.
	if err := r.repo.MarkReady(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing mark: got %v, want ErrNotFound", err)
	}
}

// T2.16.4 — Started timestamp persisted.
func TestT2164StartedTimestampPersisted(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	wu := r.unit(t, planID, 1, "start")
	if err := r.repo.MarkExecuting(ctx, wu.ID); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	got, err := r.repo.Get(ctx, wu.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != wbs.WorkUnitStatusEXECUTING {
		t.Fatalf("status = %v, want EXECUTING", got.Status)
	}
	if got.StartedAt == nil {
		t.Fatal("StartedAt not stored")
	}
	if got.CompletedAt != nil {
		t.Fatalf("CompletedAt must stay null: %v", got.CompletedAt)
	}
}

// T2.16.5 — Completed timestamp persisted.
func TestT2165CompletedTimestampPersisted(t *testing.T) {
	r := newWorkUnitRig(t)
	ctx := context.Background()
	planID := r.plan(t)
	done := r.unit(t, planID, 1, "done")
	failed := r.unit(t, planID, 2, "failed")
	if err := r.repo.MarkDone(ctx, done.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := r.repo.MarkFailed(ctx, failed.ID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	for _, tc := range []struct {
		id   uuid.UUID
		want wbs.WorkUnitStatus
	}{
		{done.ID, wbs.WorkUnitStatusDONE},
		{failed.ID, wbs.WorkUnitStatusFAILED},
	} {
		got, err := r.repo.Get(ctx, tc.id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != tc.want {
			t.Fatalf("status = %v, want %v", got.Status, tc.want)
		}
		if got.CompletedAt == nil {
			t.Fatalf("CompletedAt not stored for %v", tc.want)
		}
	}
	// Re-marking must not overwrite the first completion time.
	again, err := r.repo.Get(ctx, done.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	first := again.CompletedAt
	if err := r.repo.MarkDone(ctx, done.ID); err != nil {
		t.Fatalf("re-mark done: %v", err)
	}
	again, _ = r.repo.Get(ctx, done.ID)
	if !again.CompletedAt.Equal(*first) {
		t.Fatalf("CompletedAt changed on re-mark: %v -> %v", first, again.CompletedAt)
	}
}
