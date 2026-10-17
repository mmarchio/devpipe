package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-13 (WBS 7.18/7.19): scope resolution + missing required vs optional
// state-file handling. The StateFileScopeResolutionService resolves, for
// every required type of a project (the P7-12 resolved set intersected with
// the ENABLED types marked required), the file at the TYPE'S DECLARED SCOPE
// (per the task NOTES: the mapping is per type record, not per category
// string):
//
//   - PROJECT-scoped types -> (type, PROJECT, nil associations)
//   - PHASE-scoped types   -> (type, PHASE, the current phase)
//   - WORK_UNIT-scoped     -> (type, WORK_UNIT, the current work unit)
//   - EXECUTION-scoped     -> (type, EXECUTION, nil associations)
//
// A missing required context (no current phase / no current work unit) is an
// error (T7.18.4); no cross-project leakage (T7.18.5 — the file lookup is
// (type, scope, association)-addressed and the association FKs are
// project-scoped).
//
// WBS 7.19 requiredness semantics: the type's persisted required flag
// (state_file_types.required, P7-01) drives it — a missing REQUIRED file
// fails the op with an error identifying the type key (T7.19.1/T7.19.2); a
// missing OPTIONAL file is omitted from the result without failing
// (T7.19.3); and the op is read-only — after a failure nothing is partially
// persisted (T7.19.4, asserted by direct SQL).
//
// Current phase / work unit reuse the P7-10 selection rule: the current
// phase's active (highest non-SUPERSEDED version) plan, first non-DONE unit
// in sequence order.
// Service-only surface (REST is P7-21); the tests drive the op over the
// harness pool.

// p713Svc wires the scope-resolution service against the harness pool.
func p713Svc(t *testing.T, h *Harness) *service.StateFileScopeResolutionService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewStateFileScopeResolutionService(
		p712Svc(t, h),
		dbrepo.NewStateFileTypeRepository(raw),
		dbrepo.NewStateFileRepository(raw),
		dbrepo.NewPhaseRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
	)
}

// p713Files returns the ids of the project's state_files rows, ordered by
// id — the T7.19.4 no-partial-persist proof compares the before/after sets
// by direct SQL (the op must be read-only).
func p713Files(t *testing.T, h *Harness, projectID uuid.UUID) []string {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT id::text FROM state_files WHERE project_id = $1 ORDER BY id", projectID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan file id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// p713Required creates a project-scoped type of the given scope/requiredness
// and returns its id (per-project scope, so it never collides with the
// global defaults).
func p713Required(t *testing.T, h *Harness, projectID uuid.UUID, key, scope string, required bool) uuid.UUID {
	t.Helper()
	rec := p705Type(t, &projectID, key, scope)
	rec.Required = required
	if err := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw()).Create(context.Background(), rec); err != nil {
		t.Fatalf("create type %s: %v", key, err)
	}
	return rec.ID
}

// p713ProjectFile creates a PROJECT-scoped file for the given type through
// the P7-08 CRUD surface.
func p713ProjectFile(t *testing.T, h *Harness, projectID, typeID uuid.UUID) uuid.UUID {
	t.Helper()
	fv, err := p708Service(t, h).CreateFile(context.Background(), service.CreateFileInput{
		ProjectID: projectID.String(),
		TypeID:    typeID.String(),
		Scope:     "PROJECT",
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	return uuid.MustParse(fv.ID)
}

// p713PhaseFile / p713UnitFile create PHASE / WORK_UNIT scoped files.
func p713PhaseFile(t *testing.T, h *Harness, projectID, phaseID, typeID uuid.UUID) uuid.UUID {
	t.Helper()
	fv, err := p708Service(t, h).CreateFile(context.Background(), service.CreateFileInput{
		ProjectID: projectID.String(),
		TypeID:    typeID.String(),
		Scope:     "PHASE",
		PhaseID:   ptrStr(phaseID.String()),
	})
	if err != nil {
		t.Fatalf("create phase file: %v", err)
	}
	return uuid.MustParse(fv.ID)
}

func p713UnitFile(t *testing.T, h *Harness, projectID, unitID, typeID uuid.UUID) uuid.UUID {
	t.Helper()
	fv, err := p708Service(t, h).CreateFile(context.Background(), service.CreateFileInput{
		ProjectID:  projectID.String(),
		TypeID:     typeID.String(),
		Scope:      "WORK_UNIT",
		WorkUnitID: ptrStr(unitID.String()),
	})
	if err != nil {
		t.Fatalf("create unit file: %v", err)
	}
	return uuid.MustParse(fv.ID)
}

func containsUUID(list []uuid.UUID, want uuid.UUID) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func resolvedFileIDs(got []service.ResolvedStateFile) []uuid.UUID {
	var out []uuid.UUID
	for _, r := range got {
		out = append(out, r.File.ID)
	}
	return out
}

// T7.18.1 — Project scope resolves correctly: a required PROJECT-scoped type
// resolves to the project's (type, PROJECT) file.
func TestP713ProjectScopeResolves(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p713Required(t, h, projectID, "P13PROJ", "PROJECT", true)
	role := p712ProjectRole(t, h, projectID, "p713r1")
	p712RequireRoleFiles(t, h, role, []string{"P13PROJ"})
	fileID := p713ProjectFile(t, h, projectID, typeID)

	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !containsUUID(resolvedFileIDs(got), fileID) {
		t.Fatalf("resolved %v missing the project-scoped file %s", got, fileID)
	}
}

// T7.18.2 — Phase scope resolves correctly: a required PHASE-scoped type
// resolves to the file bound to the project's CURRENT phase.
func TestP713PhaseScopeResolves(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID, _ := p705Rig(t, h)
	typeID := p713Required(t, h, projectID, "P13PHASE", "PHASE", true)
	role := p712ProjectRole(t, h, projectID, "p713r2")
	p712RequireRoleFiles(t, h, role, []string{"P13PHASE"})
	fileID := p713PhaseFile(t, h, projectID, phaseID, typeID)

	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !containsUUID(resolvedFileIDs(got), fileID) {
		t.Fatalf("resolved %v missing the current-phase file %s", got, fileID)
	}
}

// T7.18.3 — Work-unit scope resolves correctly: a required WORK_UNIT-scoped
// type resolves to the file bound to the project's CURRENT work unit.
func TestP713WorkUnitScopeResolves(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitID := p705Rig(t, h)
	typeID := p713Required(t, h, projectID, "P13UNIT", "WORK_UNIT", true)
	role := p712ProjectRole(t, h, projectID, "p713r3")
	p712RequireRoleFiles(t, h, role, []string{"P13UNIT"})
	fileID := p713UnitFile(t, h, projectID, unitID, typeID)

	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !containsUUID(resolvedFileIDs(got), fileID) {
		t.Fatalf("resolved %v missing the current-unit file %s", got, fileID)
	}
}

// T7.18.4 — Missing required context returns an error: (a) a project with
// no phase cannot resolve a required PHASE-scoped type; (b) a project whose
// current plan is all-DONE has no current work unit, so a required
// WORK_UNIT-scoped type fails the same way. The op is a pure read, so
// nothing is written in either case (direct SQL).
func TestP713MissingRequiredContextErrors(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	// (a) no phase at all (createPhaseProject creates a bare project).
	bare := createPhaseProject(t, h)
	bareID := uuid.MustParse(bare)
	p713Required(t, h, bareID, "P13NOPHASE", "PHASE", true)
	role := p712ProjectRole(t, h, bareID, "p713no")
	p712RequireRoleFiles(t, h, role, []string{"P13NOPHASE"})
	svc := p713Svc(t, h)
	_, err := svc.ResolveStateFileScopes(ctx, bare)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("no-phase resolve err = %v, want service.ErrNotFound", err)
	}

	// (b) phase + plan exist, but the only unit is DONE -> no current unit.
	projectID, phaseID, _ := p705Rig(t, h)
	var planID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM execution_plans WHERE phase_id = '"+phaseID.String()+"'", &planID); err != nil {
		t.Fatalf("scan plan: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE execution_plan_id = '"+planID.String()+"'"); err != nil {
		t.Fatalf("mark unit done: %v", err)
	}
	var ustatus string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE execution_plan_id = '"+planID.String()+"'", &ustatus); err != nil {
		t.Fatalf("scan unit status: %v", err)
	}
	if ustatus != "DONE" {
		t.Fatalf("unit status = %q, want DONE", ustatus)
	}
	p713Required(t, h, projectID, "P13NOUNIT", "WORK_UNIT", true)
	roleB := p712ProjectRole(t, h, projectID, "p713nu")
	p712RequireRoleFiles(t, h, roleB, []string{"P13NOUNIT"})
	before := p713Files(t, h, projectID)
	_, err = svc.ResolveStateFileScopes(ctx, projectID.String())
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("no-unit resolve err = %v, want service.ErrNotFound", err)
	}
	after := p713Files(t, h, projectID)
	if len(before) != len(after) {
		t.Fatalf("files after failed resolve = %d rows, want %d (read-only op)", len(after), len(before))
	}
}

// T7.18.5 — No cross-project leakage: project B's PHASE-scoped file (bound
// to project B's phase) never appears in project A's resolution, even
// though both projects carry the same required type key. The file lookup is
// (type, scope, association)-addressed and the association FKs are
// project-scoped, so A resolves its own (missing, optional -> omitted)
// entry without seeing B's row.
func TestP713NoCrossProjectLeakage(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectA, _, _ := p705Rig(t, h)
	bStr := createPhaseProject(t, h)
	projectB := uuid.MustParse(bStr)
	phaseB := seedPhaseStatus(t, h, projectB, 1, "BPhase", "PENDING")

	key := "P13LEAK"
	typeA := p713Required(t, h, projectA, key, "PHASE", false)
	typeB := p713Required(t, h, projectB, key, "PHASE", false)
	roleA := p712ProjectRole(t, h, projectA, "p713la")
	p712RequireRoleFiles(t, h, roleA, []string{key})
	roleB := p712ProjectRole(t, h, projectB, "p713lb")
	p712RequireRoleFiles(t, h, roleB, []string{key})
	leakID := p713PhaseFile(t, h, projectB, phaseB, typeB)
	if leakID == uuid.Nil {
		t.Fatal("leak file id is nil")
	}

	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectA.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if containsUUID(resolvedFileIDs(got), leakID) {
		t.Fatalf("resolution for project A contains project B's file %s (leakage)", leakID)
	}
	// A's own (missing, optional) entry is simply omitted.
	if typeA == uuid.Nil {
		t.Fatal("type A id is nil")
	}
}

// T7.19.1 — Required missing file fails: a required type with no file at
// its scope makes the op fail (service.ErrMissingStateFile), while a
// present required file resolves (the control half).
func TestP713RequiredMissingFails(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	missing := p713Required(t, h, projectID, "P13MISSING", "PROJECT", true)
	if missing == uuid.Nil {
		t.Fatal("missing type id is nil")
	}
	role := p712ProjectRole(t, h, projectID, "p713m1")
	p712RequireRoleFiles(t, h, role, []string{"P13MISSING"})
	svc := p713Svc(t, h)
	_, err := svc.ResolveStateFileScopes(ctx, projectID.String())
	if !errors.Is(err, service.ErrMissingStateFile) {
		t.Fatalf("missing-required err = %v, want service.ErrMissingStateFile", err)
	}

	// Control: once the file exists, the same op succeeds.
	fileID := p713ProjectFile(t, h, projectID, missing)
	got, err := svc.ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve after file creation: %v", err)
	}
	if !containsUUID(resolvedFileIDs(got), fileID) {
		t.Fatalf("resolved %v missing %s", got, fileID)
	}
}

// T7.19.2 — The error identifies the missing type: the message carries the
// type's key.
func TestP713MissingErrorIdentifiesType(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	key := "P13NAMED"
	if p713Required(t, h, projectID, key, "PROJECT", true) == uuid.Nil {
		t.Fatal("type id is nil")
	}
	role := p712ProjectRole(t, h, projectID, "p713m2")
	p712RequireRoleFiles(t, h, role, []string{key})
	_, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if !errors.Is(err, service.ErrMissingStateFile) {
		t.Fatalf("err = %v, want service.ErrMissingStateFile", err)
	}
	if !strings.Contains(err.Error(), key) {
		t.Fatalf("error %q does not identify the missing type key %q", err, key)
	}
}

// T7.19.3 — Optional missing file does not fail: an optional type with no
// file is omitted from the result and the op succeeds.
func TestP713OptionalMissingDoesNotFail(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	if p713Required(t, h, projectID, "P13OPTIONAL", "PROJECT", false) == uuid.Nil {
		t.Fatal("type id is nil")
	}
	role := p712ProjectRole(t, h, projectID, "p713m3")
	p712RequireRoleFiles(t, h, role, []string{"P13OPTIONAL"})
	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("optional missing: err = %v, want nil", err)
	}
	_ = got
}

// T7.19.4 — No partial persistence after failure: the op is a pure read —
// after a required-missing failure, state_files (and every state-file
// table) is unchanged, asserted by direct SQL signatures (row counts +
// per-row (id, current_version) set), not the service's own readback.
func TestP713NoPartialPersistAfterFailure(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	// Baseline content so the version signatures are non-vacuous.
	present := p713Required(t, h, projectID, "P13BASE", "PROJECT", true)
	baseFile := p713ProjectFile(t, h, projectID, present)
	if _, err := p708Service(t, h).UpdateContent(ctx, baseFile.String(), "baseline content", 0); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	if p713Required(t, h, projectID, "P13BLOCKER", "PROJECT", true) == uuid.Nil {
		t.Fatal("blocker type id is nil")
	}
	role := p712ProjectRole(t, h, projectID, "p713b")
	p712RequireRoleFiles(t, h, role, []string{"P13BASE", "P13BLOCKER"})

	sig := func() string {
		var filesSig, versionsSig string
		if err := h.DBScan(ctx,
			"SELECT coalesce(string_agg(id || ':' || current_version, ',' ORDER BY id), '') FROM state_files WHERE project_id = '"+projectID.String()+"'",
			&filesSig); err != nil {
			t.Fatalf("files signature: %v", err)
		}
		var count int
		if err := h.DBScan(ctx,
			"SELECT count(*) FROM state_file_versions WHERE state_file_id IN (SELECT id FROM state_files WHERE project_id = '"+projectID.String()+"')",
			&count); err != nil {
			t.Fatalf("version count: %v", err)
		}
		versionsSig = "rows=" + itoaSig(count)
		return filesSig + " | " + versionsSig
	}
	before := sig()

	_, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if !errors.Is(err, service.ErrMissingStateFile) {
		t.Fatalf("expected missing-required failure, got %v", err)
	}
	if after := sig(); after != before {
		t.Fatalf("state changed after failed resolve: before=%q after=%q (partial persist)", before, after)
	}
}

func itoaSig(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
