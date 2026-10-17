package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

type p305Rig struct {
	h     *Harness
	proj  *service.ProjectService
	set   *service.SettingsService
	ready *service.ReadinessService
	p     *project.Project
}

func newP305Rig(t *testing.T) *p305Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "readiness project", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &p305Rig{
		h: h, proj: projSvc, p: p,
		set: service.NewSettingsService(dbrepo.NewSettingsRepository(pool), dbrepo.NewProjectRepository(pool), pool),
		ready: service.NewReadinessService(
			dbrepo.NewProjectRepository(pool),
			dbrepo.NewSettingsRepository(pool),
			dbrepo.NewPhaseRepository(pool),
			dbrepo.NewDocumentRepository(pool),
		),
	}
}

// configure makes the project readiness-complete: valid working directory,
// all provider/model fields, and one phase whose WBS document exists when
// wbsContent != "".
func (r *p305Rig) configure(t *testing.T, dir, wbsContent string) {
	t.Helper()
	ctx := context.Background()
	st, err := r.set.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	st.WorkingDirectory = dir
	st.WorkerProvider = "hermes"
	st.WorkerModel = "default"
	st.CompressionProvider = "llm"
	st.CompressionModel = "default"
	if _, err := r.set.UpdateSettings(ctx, st, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	ph, err := dbrepo.NewPhaseRepository(r.h.App.DB().Raw()).Append(ctx, r.p.ID, "p1", "")
	if err != nil {
		t.Fatalf("append phase: %v", err)
	}
	if wbsContent != "" {
		if _, err := dbrepo.NewDocumentRepository(r.h.App.DB().Raw()).Create(ctx, ph.ID.String(), "WBS", wbsContent); err != nil {
			t.Fatalf("create wbs doc: %v", err)
		}
	}
}

func hasCode(codes []string, want string) bool { return slices.Contains(codes, want) }

// T3.5.1 — Fully configured project is ready.
func TestT351FullyConfiguredReady(t *testing.T) {
	r := newP305Rig(t)
	dir := t.TempDir()
	r.configure(t, dir, "do the thing")
	rep, err := r.ready.CheckReadiness(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rep.Ready || len(rep.Failures) != 0 {
		t.Fatalf("expected ready, got ready=%v failures=%v", rep.Ready, rep.Failures)
	}
}

// T3.5.2 — Project with no phases rejected.
func TestT352NoPhases(t *testing.T) {
	r := newP305Rig(t)
	rep, err := r.ready.CheckReadiness(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Ready {
		t.Fatal("project without phases reported ready")
	}
	if !hasCode(rep.Failures, "phase_missing") {
		t.Fatalf("expected phase_missing in %v", rep.Failures)
	}
}

// T3.5.3 — Missing WBS rejected.
func TestT353MissingWBS(t *testing.T) {
	r := newP305Rig(t)
	r.configure(t, t.TempDir(), "") // phase exists, no WBS document
	rep, err := r.ready.CheckReadiness(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Ready {
		t.Fatal("project without WBS reported ready")
	}
	if !hasCode(rep.Failures, "phase_wbs_missing") {
		t.Fatalf("expected phase_wbs_missing in %v", rep.Failures)
	}
}

// T3.5.4 — Missing worker model rejected.
func TestT354MissingWorkerModel(t *testing.T) {
	r := newP305Rig(t)
	r.configure(t, t.TempDir(), "wbs")
	ctx := context.Background()
	st, _ := r.set.GetSettings(ctx, r.p.ID.String())
	st.WorkerModel = ""
	if _, err := r.set.UpdateSettings(ctx, st, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update: %v", err)
	}
	rep, err := r.ready.CheckReadiness(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Ready || !hasCode(rep.Failures, "worker_model_missing") {
		t.Fatalf("expected worker_model_missing, got ready=%v %v", rep.Ready, rep.Failures)
	}
}

// T3.5.5 — Missing compression model rejected.
func TestT355MissingCompressionModel(t *testing.T) {
	r := newP305Rig(t)
	r.configure(t, t.TempDir(), "wbs")
	ctx := context.Background()
	st, _ := r.set.GetSettings(ctx, r.p.ID.String())
	st.CompressionModel = ""
	if _, err := r.set.UpdateSettings(ctx, st, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update: %v", err)
	}
	rep, err := r.ready.CheckReadiness(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Ready || !hasCode(rep.Failures, "compression_model_missing") {
		t.Fatalf("expected compression_model_missing, got ready=%v %v", rep.Ready, rep.Failures)
	}
}

// T3.5.6 — Invalid working directory rejected (unset, missing, file).
func TestT356InvalidWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		dir  string
		code string
	}{
		{"unset", "", "working_directory_not_configured"},
		{"missing", filepath.Join(t.TempDir(), "nope"), "working_directory_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newP305Rig(t)
			r.configure(t, tc.dir, "wbs")
			rep, err := r.ready.CheckReadiness(ctx, r.p.ID.String())
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if rep.Ready || !hasCode(rep.Failures, tc.code) {
				t.Fatalf("expected %s, got ready=%v %v", tc.code, rep.Ready, rep.Failures)
			}
		})
	}
	t.Run("file", func(t *testing.T) {
		r := newP305Rig(t)
		f := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.configure(t, f, "wbs")
		rep, err := r.ready.CheckReadiness(ctx, r.p.ID.String())
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if rep.Ready || !hasCode(rep.Failures, "working_directory_not_a_directory") {
			t.Fatalf("expected working_directory_not_a_directory, got ready=%v %v", rep.Ready, rep.Failures)
		}
	})
}

// T3.5.7 — Multiple failures returned together.
func TestT357MultipleFailures(t *testing.T) {
	r := newP305Rig(t)
	// Defaults after CreateProject: no working dir, no worker/compression,
	// no phases.
	rep, err := r.ready.CheckReadiness(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Ready {
		t.Fatal("incomplete project reported ready")
	}
	for _, want := range []string{
		"working_directory_not_configured",
		"worker_provider_missing",
		"worker_model_missing",
		"compression_provider_missing",
		"compression_model_missing",
		"phase_missing",
	} {
		if !hasCode(rep.Failures, want) {
			t.Fatalf("missing code %s in %v", want, rep.Failures)
		}
	}
}

// T3.5.8 — Failed readiness validation leaves project DRAFT.
func TestT358FailureLeavesDraft(t *testing.T) {
	r := newP305Rig(t)
	if _, err := r.ready.CheckReadiness(context.Background(), r.p.ID.String()); err != nil {
		t.Fatalf("check: %v", err)
	}
	p, err := r.proj.GetProject(context.Background(), r.p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Status != project.StatusDRAFT {
		t.Fatalf("status after failed readiness = %s, want DRAFT", p.Status)
	}
}

// A readiness check for an unknown project is not found.
func TestT35xUnknownProject(t *testing.T) {
	r := newP305Rig(t)
	if _, err := r.ready.CheckReadiness(context.Background(), "11111111-1111-1111-1111-111111111111"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
