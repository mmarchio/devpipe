package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-07 (WBS 7.2): default state-file type seeding — the six global
// defaults, idempotent, with project overrides that leave the global
// defaults untouched.

// p707GlobalSignature is a full-row signature over the global-scope type
// rows: every column, one record per row, deterministic order by key.
func p707GlobalSignature(t *testing.T, h *Harness) string {
	t.Helper()
	var sig string
	sql := `SELECT coalesce(string_agg(id::text || '|' || key || '|' || name || '|' || description || '|' || default_filename || '|' || scope || '|' || source || '|' || required::text || '|' || enabled::text || '|' || created_at::text || '|' || updated_at::text, '@' ORDER BY key), '') FROM state_file_types WHERE project_id IS NULL`
	if err := h.DBScan(context.Background(), sql, &sig); err != nil {
		t.Fatalf("global signature: %v", err)
	}
	return sig
}

// T7.2.1 + T7.2.2 — Seeding creates all six defaults in the global scope
// with the WBS 7.2 default filenames.
func TestP707DefaultTypesSeeded(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := service.NewStateFileService(dbrepo.NewStateFileTypeRepository(h.App.DB().Raw()), dbrepo.NewStateFileRepository(h.App.DB().Raw()), dbrepo.NewStateFileVersionRepository(h.App.DB().Raw()))
	if err := svc.SeedDefaultFileTypes(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repo := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw())
	list, err := repo.ListByScope(ctx, nil)
	if err != nil {
		t.Fatalf("list global: %v", err)
	}
	want := map[string]string{
		"PROJECT_STATE": "PROJECT_STATE.md",
		"PHASE_STATE":   "PHASE_STATE.md",
		"ACTIVE_TASK":   "ACTIVE_TASK.md",
		"TASK_ARCHIVE":  "TASK_ARCHIVE.md",
		"ARCHITECTURE":  "ARCHITECTURE.md",
		"TEST_STATE":    "TEST_STATE.md",
	}
	if len(list) != len(want) {
		t.Fatalf("global type count = %d, want %d", len(list), len(want))
	}
	for _, r := range list {
		if r.ProjectID != nil {
			t.Errorf("type %s is not global (project_id set)", r.Key)
		}
		wantFilename, ok := want[r.Key]
		if !ok {
			t.Errorf("unexpected global type key %q", r.Key)
			continue
		}
		if r.DefaultFilename != wantFilename {
			t.Errorf("type %s default_filename = %q, want %q", r.Key, r.DefaultFilename, wantFilename)
		}
		delete(want, r.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing default keys: %v", want)
	}
}

// T7.2.3 — Re-seeding is idempotent: the row count stays at six and the
// full-row signature over the global rows is byte-identical before and
// after a second seed.
func TestP707ReSeedIdempotent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := service.NewStateFileService(dbrepo.NewStateFileTypeRepository(h.App.DB().Raw()), dbrepo.NewStateFileRepository(h.App.DB().Raw()), dbrepo.NewStateFileVersionRepository(h.App.DB().Raw()))
	if err := svc.SeedDefaultFileTypes(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sig1 := p707GlobalSignature(t, h)
	if sig1 == "" {
		t.Fatal("global signature empty after first seed")
	}
	if err := svc.SeedDefaultFileTypes(ctx); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	sig2 := p707GlobalSignature(t, h)
	if sig1 != sig2 {
		t.Fatalf("signature changed on re-seed:\nfirst:  %s\nsecond: %s", sig1, sig2)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_types WHERE project_id IS NULL", &n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("global type count after re-seed = %d, want 6", n)
	}
}

// T7.2.4 — A project-scoped type with the same key does not mutate the
// global default: the global rows stay byte-identical (signature) and the
// project list returns only the override row.
func TestP707ProjectOverrideKeepsGlobalDefault(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := parseUUID(t, createPhaseProject(t, h))
	repo := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw())
	svc := service.NewStateFileService(repo, dbrepo.NewStateFileRepository(h.App.DB().Raw()), dbrepo.NewStateFileVersionRepository(h.App.DB().Raw()))
	if err := svc.SeedDefaultFileTypes(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sig1 := p707GlobalSignature(t, h)
	// The project override: same key, project scope.
	if err := repo.Create(ctx, p705Type(t, &projectID, "ACTIVE_TASK", "WORK_UNIT")); err != nil {
		t.Fatalf("create override: %v", err)
	}
	// The override does not change the global rows, and a re-seed with the
	// override present still creates nothing.
	if err := svc.SeedDefaultFileTypes(ctx); err != nil {
		t.Fatalf("re-seed with override: %v", err)
	}
	sig2 := p707GlobalSignature(t, h)
	if sig1 != sig2 {
		t.Fatalf("global signature mutated by project override")
	}
	projList, err := repo.ListByScope(ctx, &projectID)
	if err != nil {
		t.Fatalf("list project: %v", err)
	}
	if len(projList) != 1 || projList[0].Key != "ACTIVE_TASK" {
		t.Fatalf("project list = %+v, want the single ACTIVE_TASK override", projList)
	}
}

// T7.2.4 placement half — Seed composition is verified end-to-end: the
// harness boots the real app (app.Start), whose startup sequence includes
// the default-type seed, so a fresh harness DB already carries the six
// global defaults without any explicit service call.
func TestP707SeedWiredIntoStartup(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	repo := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw())
	list, err := repo.ListByScope(ctx, nil)
	if err != nil {
		t.Fatalf("list global: %v", err)
	}
	if len(list) != 6 {
		t.Fatalf("global type count after app start = %d, want 6", len(list))
	}
	for _, r := range list {
		if r.ProjectID != nil || r.DefaultFilename != r.Key+".md" {
			t.Errorf("unexpected global type %+v", r)
		}
	}
}
