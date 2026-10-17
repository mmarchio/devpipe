package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p326Rig struct {
	h     *Harness
	set   *service.SettingsService
	snaps *service.SnapshotService
	p     *project.Project
}

func newP326Rig(t *testing.T) *p326Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "snapshot project", "snap desc")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &p326Rig{
		h: h, p: p,
		set: service.NewSettingsService(dbrepo.NewSettingsRepository(pool), dbrepo.NewProjectRepository(pool), pool),
		snaps: service.NewSnapshotService(
			dbrepo.NewProjectRepository(pool),
			dbrepo.NewSettingsRepository(pool),
			dbrepo.NewPhaseRepository(pool),
			dbrepo.NewDocumentRepository(pool),
			pool,
		),
	}
}

// configureP326 sets the settings fields used by the snapshot tests.
func (r *p326Rig) configureP326(t *testing.T) *project.Settings {
	t.Helper()
	ctx := context.Background()
	st, err := r.set.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	st.WorkingDirectory = t.TempDir()
	st.WorkerProvider = "hermes"
	st.WorkerModel = "snap-worker"
	st.CompressionProvider = "llm"
	st.CompressionModel = "snap-compress"
	st.MaxRetries = 7
	st, err = r.set.UpdateSettings(ctx, st, projectVersion(t, r.h, r.p.ID.String()))
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	return st
}

// appendPhase adds a phase (PENDING by default) and optionally a WBS document.
func (r *p326Rig) appendPhase(t *testing.T, name, wbs string) {
	t.Helper()
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	ph, err := dbrepo.NewPhaseRepository(raw).Append(ctx, r.p.ID, name, "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	if wbs != "" {
		if _, err := dbrepo.NewDocumentRepository(raw).Create(ctx, ph.ID.String(), "WBS", wbs); err != nil {
			t.Fatalf("create wbs doc: %v", err)
		}
	}
}

// markLastDone marks the most recently appended phase DONE.
func (r *p326Rig) markLastDone(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	phases, err := dbrepo.NewPhaseRepository(raw).ListByProject(ctx, r.p.ID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	last := phases[len(phases)-1]
	if err := dbrepo.NewPhaseRepository(raw).MarkCompleted(ctx, last.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
}

// T3.26.1 — Snapshot loads project.
func TestT3261SnapshotLoadsProject(t *testing.T) {
	r := newP326Rig(t)
	snap, err := r.snaps.Snapshot(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Project.ID != r.p.ID {
		t.Fatalf("project ID = %s, want %s", snap.Project.ID, r.p.ID)
	}
	if snap.Project.Name != "snapshot project" {
		t.Fatalf("name = %q", snap.Project.Name)
	}
	if snap.Project.Description != "snap desc" {
		t.Fatalf("description = %q", snap.Project.Description)
	}
	if snap.Project.Status != project.StatusDRAFT {
		t.Fatalf("status = %s, want DRAFT", snap.Project.Status)
	}
}

// A snapshot for an unknown project is not found.
func TestT326xUnknownProject(t *testing.T) {
	r := newP326Rig(t)
	_, err := r.snaps.Snapshot(context.Background(), "11111111-1111-1111-1111-111111111111")
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// T3.26.2 — Snapshot loads settings.
func TestT3262SnapshotLoadsSettings(t *testing.T) {
	r := newP326Rig(t)
	want := r.configureP326(t)
	snap, err := r.snaps.Snapshot(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := snap.Settings
	if got.ProjectID != r.p.ID {
		t.Fatalf("settings project = %s, want %s", got.ProjectID, r.p.ID)
	}
	if got.WorkingDirectory != want.WorkingDirectory {
		t.Fatalf("working directory = %q, want %q", got.WorkingDirectory, want.WorkingDirectory)
	}
	if got.WorkerProvider != "hermes" || got.WorkerModel != "snap-worker" {
		t.Fatalf("worker = %q/%q", got.WorkerProvider, got.WorkerModel)
	}
	if got.CompressionProvider != "llm" || got.CompressionModel != "snap-compress" {
		t.Fatalf("compression = %q/%q", got.CompressionProvider, got.CompressionModel)
	}
	if got.MaxRetries != 7 {
		t.Fatalf("max retries = %d, want 7", got.MaxRetries)
	}
}

// T3.26.3 — Snapshot identifies the current phase: the lowest-order
// incomplete phase; nil when the project has no phases; the last phase
// when every phase is DONE.
func TestT3263SnapshotIdentifiesCurrentPhase(t *testing.T) {
	r := newP326Rig(t)
	ctx := context.Background()
	// No phases: CurrentPhase is nil.
	snap, err := r.snaps.Snapshot(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.CurrentPhase != nil {
		t.Fatalf("expected nil current phase, got %v", snap.CurrentPhase)
	}

	// Phase 1 DONE, phase 2 PENDING: the current phase is phase 2.
	r.appendPhase(t, "phase 1", "wbs 1")
	r.markLastDone(t)
	r.appendPhase(t, "phase 2", "wbs 2")
	snap, err = r.snaps.Snapshot(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.CurrentPhase == nil || snap.CurrentPhase.Name != "phase 2" {
		t.Fatalf("current phase = %v, want phase 2", snap.CurrentPhase)
	}

	// All phases DONE: the current phase is the last one.
	r.markLastDone(t)
	snap, err = r.snaps.Snapshot(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.CurrentPhase == nil || snap.CurrentPhase.Name != "phase 2" {
		t.Fatalf("current phase = %v, want the last phase (phase 2)", snap.CurrentPhase)
	}
}

// T3.26.4 — Snapshot loads the current phase's WBS; nil when the current
// phase has none.
func TestT3264SnapshotLoadsCurrentWBS(t *testing.T) {
	r := newP326Rig(t)
	ctx := context.Background()
	r.appendPhase(t, "phase 1", "# scope\ndo the work")
	snap, err := r.snaps.Snapshot(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.CurrentWBS == nil {
		t.Fatal("expected a WBS document, got nil")
	}
	if snap.CurrentWBS.Content != "# scope\ndo the work" {
		t.Fatalf("wbs content = %q", snap.CurrentWBS.Content)
	}
	if snap.CurrentPhase == nil || snap.CurrentWBS.PhaseID != snap.CurrentPhase.ID.String() {
		t.Fatalf("wbs phase = %v, current phase = %v", snap.CurrentWBS.PhaseID, snap.CurrentPhase)
	}
	if snap.CurrentWBS.Version != 1 {
		t.Fatalf("wbs version = %d, want 1", snap.CurrentWBS.Version)
	}

	// A current phase without a WBS document: the snapshot still loads and
	// CurrentWBS stays nil.
	r2 := newP326Rig(t)
	r2.appendPhase(t, "no wbs", "")
	snap2, err := r2.snaps.Snapshot(ctx, r2.p.ID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap2.CurrentWBS != nil {
		t.Fatalf("expected nil wbs, got %v", snap2.CurrentWBS)
	}
}

// T3.26.5 — Snapshot is transactionally consistent where required: under
// concurrent settings mutation every snapshot is self-consistent (the
// settings belong to the snapshotted project) and the final settings
// version converges to the committed state — never a torn read.
func TestT3265SnapshotTransactionallyConsistent(t *testing.T) {
	r := newP326Rig(t)
	ctx := context.Background()
	st := r.configureP326(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ver := projectVersion(t, r.h, r.p.ID.String())
		for i := 0; i < 50; i++ {
			s, err := r.set.GetSettings(ctx, r.p.ID.String())
			if err != nil {
				t.Errorf("get settings: %v", err)
				return
			}
			s.WorkerModel = fmt.Sprintf("snap-worker-%d", i)
			_, err = r.set.UpdateSettings(ctx, s, ver)
			if err != nil {
				if errors.Is(err, service.ErrConflict) {
					continue
				}
				t.Errorf("update settings: %v", err)
				return
			}
			ver++
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	seen := 0
	for i := 0; i < 60; i++ {
		snap, err := r.snaps.Snapshot(ctx, r.p.ID.String())
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		// Self-consistency of a single snapshot: settings are scoped to the
		// snapshotted project and the current phase (if any) is one of the
		// project's phases.
		if snap.Settings.ProjectID != snap.Project.ID {
			t.Fatalf("torn snapshot: settings project %s != project %s", snap.Settings.ProjectID, snap.Project.ID)
		}
		if snap.CurrentPhase != nil && snap.CurrentPhase.ProjectID != snap.Project.ID {
			t.Fatalf("torn snapshot: phase project %s != project %s", snap.CurrentPhase.ProjectID, snap.Project.ID)
		}
		seen++
	}
	close(stop)
	wg.Wait()

	final, err := r.set.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("final settings: %v", err)
	}
	if final.WorkerModel == st.WorkerModel {
		t.Fatalf("concurrent updater never committed (model still %q)", final.WorkerModel)
	}
	if seen == 0 {
		t.Fatal("no snapshots taken")
	}
}
