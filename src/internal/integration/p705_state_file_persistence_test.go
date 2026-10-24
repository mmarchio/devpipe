package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// P7-05 (WBS 7.1/7.4 persistence half + 7.7): migration 00034
// (state_file_types) + 00035 (state_files) and the type/file repositories
// against real PostgreSQL.

// p705Rig seeds a project with one phase and one work unit through the real
// services/repositories (seed helpers from prior tasks) and returns all ids.
func p705Rig(t *testing.T, h *Harness) (projectID, phaseID, unitID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	projectIDStr := createPhaseProject(t, h)
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	phaseID = seedPhaseStatus(t, h, projectID, 1, "Foundation", "PENDING")
	planID := seedPlan(t, h, phaseID, 1, 1, "READY")
	seedUnit(t, h, planID, 1, "unit", "PENDING")
	p705BackfillUnitProjects(t, h, projectID)
	var u uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM work_units WHERE execution_plan_id = '"+planID.String()+"'", &u); err != nil {
		t.Fatalf("scan unit: %v", err)
	}
	return projectID, phaseID, u
}

// p705Type builds a valid domain type record for the repository.
func p705Type(t *testing.T, projectID *uuid.UUID, key, scope string) *persistence.StateFileTypeRecord {
	t.Helper()
	ft, err := statefile.NewFileType(statefile.StateFileTypeInput{
		ProjectID:       projectID,
		Key:             key,
		Name:            key,
		DefaultFilename: key + ".md",
		Scope:           statefile.StateFileScope(scope),
	})
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	return p705TypeFromDomain(t, ft)
}

// p705TypeFromDomain maps a domain type to its persistence record.
func p705TypeFromDomain(t *testing.T, ft *statefile.StateFileType) *persistence.StateFileTypeRecord {
	t.Helper()
	return &persistence.StateFileTypeRecord{
		ID: ft.ID, ProjectID: ft.ProjectID, Key: ft.Key, Name: ft.Name,
		Description: ft.Description, DefaultFilename: ft.DefaultFilename,
		Scope: string(ft.Scope), Source: string(ft.Source),
		Required: ft.Required, Enabled: ft.Enabled,
		CreatedAt: ft.CreatedAt, UpdatedAt: ft.UpdatedAt,
	}
}

// p705BackfillUnitProjects replicates migration 00033 backfill for units
// seeded by the direct-SQL helper (which predates the column): the unit
// owner lookup in the domain-guard tests needs the owning project.
func p705BackfillUnitProjects(t *testing.T, h *Harness, projectID uuid.UUID) {
	t.Helper()
	sql := "UPDATE work_units w SET project_id = p.project_id FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE ep.id = w.execution_plan_id AND p.project_id = '" + projectID.String() + "'"
	if _, err := h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("backfill unit projects: %v", err)
	}
}

// nowUTC is the test-clock helper for record timestamps.
func nowUTC(t *testing.T) time.Time {
	t.Helper()
	return time.Now().UTC()
}

// P7-05.1 — Migration 00034/00035 apply cleanly on a fresh harness DB and
// the new objects exist.
func TestP705MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 33 -> 35 by P7-05 (migrations 00034/00035) — accepted contract
	// change, not a weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	for _, table := range []string{"state_file_types", "state_files"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+table+"'", &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s present = %d, want 1", table, n)
		}
	}
}

// P7-05.2 — Down migration reverses cleanly: dropping the new tables (state
// files first — they reference the type table) returns the schema to its
// pre-00034 shape.
func TestP705DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	// state_file_versions (00036, P7-06) references state_files, so the
	// down migration drops it first — accepted contract change (the P7-05
	// down-shape now includes the P7-06 table), not a weakened test.
	// materialized_state_files (00038, P7-15) references state_files the
	// same way and is dropped first as well — accepted contract change
	// (the P7-05 down-shape now includes the P7-15 table), not a weakened
	// test.
	// context_snapshot_files (00039, P7-18) already drops with the same
	// rationale; context_snapshots (also 00039) drops alongside —
	// accepted contract change (the P7-05 down-shape now includes the
	// P7-18 tables), not a weakened test.
	// canonical_prompts (00044, P8-30) references context_snapshots, so the
	// down migration drops it first — accepted contract change (the P7-05
	// down-shape now includes the P8-30 table), not a weakened test.
	// The Phase 9 compression tables (00046-00053, P9-12/P9-13/P9-46-P9-48)
	// reference canonical_prompts, so the down migration drops them first —
	// accepted contract change (the P7-05 down-shape now includes the Phase 9
	// tables), not a weakened test.
	// The Phase 10 cache entries (00054, P10-18) reference the canonical /
	// compressed / worker prompts and drop first — accepted contract change.
	// Phase 11 (00060/00066) precomputed templates + projections reference
	// canonical prompts / context snapshots — dropped first as well
	// (accepted contract change).
	// Phase 12 (00069-00074) worker dispatch tables reference worker
	// prompts — dropped first (accepted contract change).
	// Phase 13 (00076-00080) worker results reference worker dispatches —
	// dropped first (accepted contract change).
	// Phase 14 (00084-00087) continuations reference worker dispatches /
	// results / snapshots and dispatches reference continuations — the
	// dispatch link column and the continuation tables drop first
	// (accepted contract change).
	// Phase 15 (00090-00096) executions reference context snapshots /
	// worker prompts / state machines and are referenced by runtime_state /
	// history — the execution tables drop first, CASCADE removing the
	// referencing FK constraints (accepted contract change).
	// Phase 16 (00099-00106) engine tables reference executions / worker
	// dispatches / continuations / context snapshots — dropped first with
	// CASCADE (accepted contract change). Phase 17 (00107-00110): the audit
	// log and drift acknowledgements (referencing state files) drop first.
	// Phase 18 (00112+) operational tables drop first. Phase 19 (00137+):
	// the MCP tables reference worker dispatches / prompts and executions —
	// dropped first (accepted contract change).
	for _, table := range []string{"workflow_outbox", "project_state_authority", "mcp_requests", "mcp_dispatch_tokens", "task_reports", "execution_expected_heads", "execution_repository_identities", "dispatch_resolutions", "recovery_actions", "recovery_plans", "restore_state", "backup_records", "app_config_snapshots", "audit_events", "state_file_drift_acknowledgements", "execution_completions", "hardening_reports", "hardening_operations", "hardening_groups",
		"hardening_passes", "hardening_retained_groups", "repository_checkpoints", "fix_attempts", "acceptance_approvals",
		"validation_command_results", "validation_runs", "plan_candidates", "execution_steps", "execution_events", "execution_bootstrap_checkpoints", "execution_repository_baselines",
		"execution_bootstrap_transitions", "execution_configuration_snapshots", "executions"} {
		if _, err := h.DBExec(ctx, "DROP TABLE "+table+" CASCADE"); err != nil {
			t.Fatalf("down: drop %s: %v", table, err)
		}
	}
	if _, err := h.DBExec(ctx, "ALTER TABLE worker_dispatches DROP COLUMN continuation_id"); err != nil {
		t.Fatalf("down: drop worker_dispatches.continuation_id: %v", err)
	}
	if _, err := h.DBExec(ctx, "ALTER TABLE work_units DROP COLUMN archived_by_dispatch_id"); err != nil {
		t.Fatalf("down: drop work_units.archived_by_dispatch_id: %v", err)
	}
	for _, table := range []string{"worker_result_consumptions", "continuation_transitions", "continuations",
		"worker_result_parse_attempts", "worker_result_acceptance_claims", "worker_result_artifacts",
		"worker_result_items", "worker_results", "worker_tool_events", "worker_events", "worker_usage_observations", "worker_responses", "worker_dispatches",
		"precomputed_templates", "prompt_cache_entries", "worker_prompts", "compressed_prompts", "compression_attempts", "compression_runs",
		"canonical_prompt_preservation_items", "canonical_prompt_components"} {
		if _, err := h.DBExec(ctx, "DROP TABLE "+table); err != nil {
			t.Fatalf("down: drop %s: %v", table, err)
		}
	}
	if _, err := h.DBExec(ctx, "DROP TABLE canonical_prompts"); err != nil {
		t.Fatalf("down: drop canonical_prompts: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE precompute_projections"); err != nil {
		t.Fatalf("down: drop precompute_projections: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE context_snapshot_files"); err != nil {
		t.Fatalf("down: drop context_snapshot_files: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE context_snapshots"); err != nil {
		t.Fatalf("down: drop context_snapshots: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE materialized_state_files"); err != nil {
		t.Fatalf("down: drop materialized_state_files: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_file_versions"); err != nil {
		t.Fatalf("down: drop state_file_versions: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_files"); err != nil {
		t.Fatalf("down: drop state_files: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_file_types"); err != nil {
		t.Fatalf("down: drop state_file_types: %v", err)
	}
	for _, table := range []string{"state_file_types", "state_files", "state_file_versions", "materialized_state_files", "context_snapshots", "canonical_prompts"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+table+"'", &n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s still present after down migration", table)
		}
	}
}

// T7.1.3 DB half — A duplicate scoped key is rejected: a type with the same
// normalized key in the same project scope yields persistence.ErrConflict at
// the repository surface and 23505 at the DB level.
func TestP705DuplicateScopedKeyRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	repo := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw())

	if err := repo.Create(ctx, p705Type(t, &projectID, "active-task", "PROJECT")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same normalized key (lowercase + padded input), same project scope ->
	// conflict.
	if err := repo.Create(ctx, p705Type(t, &projectID, " active-task ", "PROJECT")); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate err = %v, want ErrConflict", err)
	}
	// DB level: the unique index fires with 23505.
	_, err := h.DBExec(ctx, "INSERT INTO state_file_types (id, project_id, key, name, scope, source) VALUES (gen_random_uuid(), '"+projectID.String()+"', 'ACTIVE-TASK', 'x', 'PROJECT', 'MANAGED')")
	if err == nil {
		t.Fatal("duplicate accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
}

// T7.1.4 DB half — The same key is allowed in different project scopes and
// in the global scope: each create persists, and the scoped list returns
// only its own scope's rows.
func TestP705SameKeyAcrossProjectsAllowed(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewStateFileTypeRepository(pool)

	projectA, _, _ := p705Rig(t, h)
	projectBStr := createPhaseProject(t, h)
	projectB, _ := uuid.Parse(projectBStr)

	for _, rec := range []*persistence.StateFileTypeRecord{
		p705Type(t, nil, "shared-key", "PROJECT"),
		p705Type(t, &projectA, "shared-key", "PROJECT"),
		p705Type(t, &projectB, "shared-key", "PROJECT"),
	} {
		if err := repo.Create(ctx, rec); err != nil {
			t.Fatalf("create for scope %v: %v", rec.ProjectID, err)
		}
	}
	globalList, err := repo.ListByScope(ctx, nil)
	if err != nil {
		t.Fatalf("list global: %v", err)
	}
	listA, err := repo.ListByScope(ctx, &projectA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	listB, err := repo.ListByScope(ctx, &projectB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	// The app-level startup seed (WBS 7.2, P7-07) creates six global
	// defaults, so the global scope starts at 6 rows — the SHARED-KEY create
	// brings it to 7. The scope ISOLATION assertion is the load-bearing
	// check: each project scope lists only its own row.
	if len(globalList) != 7 || len(listA) != 1 || len(listB) != 1 {
		t.Fatalf("scope counts = global %d, A %d, B %d, want 7/1/1 (7 = six WBS 7.2 defaults + shared-key)", len(globalList), len(listA), len(listB))
	}
	globalKeys := map[string]bool{}
	for _, r := range globalList {
		globalKeys[r.Key] = true
	}
	if !globalKeys["SHARED-KEY"] {
		t.Fatalf("global scope missing SHARED-KEY: %v", globalKeys)
	}
	if listA[0].Key != "SHARED-KEY" || listB[0].Key != "SHARED-KEY" {
		t.Fatalf("keys = %q / %q, want SHARED-KEY in both scopes", listA[0].Key, listB[0].Key)
	}
}

// T7.4.5 DB half — A PHASE-scoped file whose phase belongs to a DIFFERENT
// project is rejected at the repository surface: the domain-level
// project-match guard (StateFile.AssociationBelongsToProject) resolves the
// association's owning project from the phase row (a real lookup) and the
// service-layer check rejects the mismatch, while an in-project association
// passes the same path.
func TestP705CrossProjectPhaseRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectA, phaseA, _ := p705Rig(t, h)
	projectBStr := createPhaseProject(t, h)
	projectB, _ := uuid.Parse(projectBStr)
	phaseB := seedPhaseStatus(t, h, projectB, 1, "Foreign", "PENDING")

	phaseOwning := func(phaseID uuid.UUID) uuid.UUID {
		var owner uuid.UUID
		if err := h.DBScan(ctx, "SELECT project_id FROM phases WHERE id = '"+phaseID.String()+"'", &owner); err != nil {
			t.Fatalf("scan phase owner: %v", err)
		}
		return owner
	}

	domainFile, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID: &projectA, TypeID: &projectA, Scope: statefile.ScopePhase, PhaseID: &phaseB,
	})
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	// The association belongs to project B: the domain guard rejects it.
	if err := domainFile.AssociationBelongsToProject(phaseOwning(phaseB)); err == nil {
		t.Fatal("cross-project phase accepted, want rejection")
	}
	// The in-project association passes the same guard.
	inProject, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID: &projectA, TypeID: &projectA, Scope: statefile.ScopePhase, PhaseID: &phaseA,
	})
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	if err := inProject.AssociationBelongsToProject(phaseOwning(phaseA)); err != nil {
		t.Fatalf("in-project phase rejected: %v", err)
	}
}

// T7.4.6 DB half — A WORK_UNIT-scoped file whose work unit belongs to a
// DIFFERENT project is rejected the same way.
func TestP705CrossProjectWorkUnitRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectA, _, unitA := p705Rig(t, h)
	projectBStr := createPhaseProject(t, h)
	projectB, _ := uuid.Parse(projectBStr)
	phaseB := seedPhaseStatus(t, h, projectB, 1, "Foreign", "PENDING")
	planB := seedPlan(t, h, phaseB, 1, 1, "READY")
	seedUnit(t, h, planB, 1, "foreign-unit", "PENDING")
	p705BackfillUnitProjects(t, h, projectB)
	var unitB uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM work_units WHERE execution_plan_id = '"+planB.String()+"'", &unitB); err != nil {
		t.Fatalf("scan unit: %v", err)
	}

	unitOwning := func(unitID uuid.UUID) uuid.UUID {
		var owner uuid.UUID
		if err := h.DBScan(ctx, "SELECT project_id FROM work_units WHERE id = '"+unitID.String()+"'", &owner); err != nil {
			t.Fatalf("scan unit owner: %v", err)
		}
		return owner
	}

	domainFile, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID: &projectA, TypeID: &projectA, Scope: statefile.ScopeWorkUnit, WorkUnitID: &unitB,
	})
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	if err := domainFile.AssociationBelongsToProject(unitOwning(unitB)); err == nil {
		t.Fatal("cross-project work-unit accepted, want rejection")
	}
	inProject, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID: &projectA, TypeID: &projectA, Scope: statefile.ScopeWorkUnit, WorkUnitID: &unitA,
	})
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	if err := inProject.AssociationBelongsToProject(unitOwning(unitA)); err != nil {
		t.Fatalf("in-project work-unit rejected: %v", err)
	}
}

// T7.7.1 + T7.7.4 + T7.7.5 — Create persists the file (round-trip via Get
// and direct SQL), the scope queries work, and an unknown id returns the
// domain not-found error.
func TestP705FilePersistsAndScopeQueries(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	fileRepo := dbrepo.NewStateFileRepository(pool)
	typeRepo := dbrepo.NewStateFileTypeRepository(pool)

	projectA, phaseA, unitA := p705Rig(t, h)
	ts := nowUTC(t)
	typeRec := p705Type(t, &projectA, "task-archive", "WORK_UNIT")
	if err := typeRepo.Create(ctx, typeRec); err != nil {
		t.Fatalf("create type: %v", err)
	}

	recs := []*persistence.StateFileRecord{
		{ID: uuid.New(), ProjectID: projectA, TypeID: typeRec.ID, Scope: "PROJECT",
			CreatedAt: ts, UpdatedAt: ts},
		{ID: uuid.New(), ProjectID: projectA, TypeID: typeRec.ID, Scope: "PHASE", PhaseID: &phaseA,
			CreatedAt: ts, UpdatedAt: ts},
		{ID: uuid.New(), ProjectID: projectA, TypeID: typeRec.ID, Scope: "WORK_UNIT", WorkUnitID: &unitA,
			CreatedAt: ts, UpdatedAt: ts},
	}
	for _, rec := range recs {
		if err := fileRepo.Create(ctx, rec); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// Duplicate (type, scope, association) rejected.
	dup := *recs[1]
	dup.ID = uuid.New()
	if err := fileRepo.Create(ctx, &dup); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate file err = %v, want ErrConflict", err)
	}

	got, err := fileRepo.Get(ctx, recs[1].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectID != projectA || got.TypeID != typeRec.ID || got.Scope != "PHASE" ||
		got.PhaseID == nil || *got.PhaseID != phaseA || got.CurrentVersion != 0 {
		t.Fatalf("round-trip = %+v", got)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_files WHERE id = '"+recs[1].ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("direct SQL count = %d, want 1", stored)
	}

	byScope, err := fileRepo.GetByTypeAndScope(ctx, projectA, typeRec.ID, "WORK_UNIT", nil, &unitA)
	if err != nil {
		t.Fatalf("GetByTypeAndScope: %v", err)
	}
	if byScope.ID != recs[2].ID {
		t.Fatalf("GetByTypeAndScope = %s, want %s", byScope.ID, recs[2].ID)
	}
	if _, err := fileRepo.GetByTypeAndScope(ctx, projectA, typeRec.ID, "EXECUTION", nil, nil); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing scope err = %v, want ErrNotFound", err)
	}

	byProject, err := fileRepo.ListByProject(ctx, projectA)
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	byPhase, err := fileRepo.ListByPhase(ctx, phaseA)
	if err != nil {
		t.Fatalf("ListByPhase: %v", err)
	}
	byUnit, err := fileRepo.ListByWorkUnit(ctx, unitA)
	if err != nil {
		t.Fatalf("ListByWorkUnit: %v", err)
	}
	if len(byProject) != 3 || len(byPhase) != 1 || len(byUnit) != 1 {
		t.Fatalf("scope list counts = project %d, phase %d, unit %d, want 3/1/1", len(byProject), len(byPhase), len(byUnit))
	}
	if byPhase[0].ID != recs[1].ID || byUnit[0].ID != recs[2].ID {
		t.Fatalf("phase list = %v, unit list = %v", byPhase, byUnit)
	}

	// T7.7.5 — unknown ids return the domain not-found error.
	if _, err := fileRepo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown file err = %v, want ErrNotFound", err)
	}
	if _, err := typeRepo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown type err = %v, want ErrNotFound", err)
	}
}

// T7.7.6 — PostgreSQL driver errors do not escape the repository boundary:
// a live 23503 (FK) and a live 23514 (CHECK) both come back as domain
// errors, never as pgconn types.
func TestP705DriverErrorsTranslated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	typeRepo := dbrepo.NewStateFileTypeRepository(pool)
	fileRepo := dbrepo.NewStateFileRepository(pool)

	// FK violation: a type row referencing a missing project (23503).
	missing := uuid.New()
	rec := p705Type(t, &missing, "orphan", "PROJECT")
	if err := typeRepo.Create(ctx, rec); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("FK err = %v, want ErrInvalidReference", err)
	}
	// FK violation: a file row referencing a missing type (23503).
	orphanFile := &persistence.StateFileRecord{
		ID: uuid.New(), ProjectID: missing, TypeID: uuid.New(), Scope: "PROJECT",
		CreatedAt: nowUTC(t), UpdatedAt: nowUTC(t),
	}
	if err := fileRepo.Create(ctx, orphanFile); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("file FK err = %v, want ErrInvalidReference", err)
	}
	// CHECK violation: an unknown scope (23514) wraps as infrastructure.
	badScope := *rec
	badScope.ID = uuid.New()
	badScope.Key = "BADSCOPE"
	badScope.Scope = "BOGUS"
	err := typeRepo.Create(ctx, &badScope)
	if err == nil || errors.Is(err, persistence.ErrConflict) ||
		errors.Is(err, persistence.ErrNotFound) || errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("CHECK err = %v, want an infrastructure error", err)
	}
	var infra *persistence.InfrastructureError
	if !errors.As(err, &infra) {
		t.Fatalf("CHECK err type = %T, want *persistence.InfrastructureError", err)
	}
}
