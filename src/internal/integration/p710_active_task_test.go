package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// P7-10 (WBS 7.11/7.12): active-task derivation + synchronization. The
// ActiveTaskService derives the ACTIVE_TASK file content from the project's
// current work unit (selection rule: the FIRST unit, in ascending sequence
// order, of the project's current phase's active (non-SUPERSEDED,
// highest-version) plan whose status is not DONE — the WBS 2.19
// CurrentPhase/ActivePlan composition) and writes it through the P7-08
// versioned content path, so the work unit row stays byte-identical
// (T7.11.7), a unit change binds a new (type, WORK_UNIT, unit) file and
// appends a new version (T7.12.1/T7.12.3), and the previous active-task
// content is retained in the previous unit's file history (T7.12.2).
// Service-only surface (REST is P7-21); the tests drive the op over the
// harness pool.
//
// Per-unit file semantics (stated here per the task NOTES and the WBS
// 7.3/7.11 derivation model): the ACTIVE_TASK file binds to the work unit
// it derives from, so a unit change creates the new unit's file and leaves
// the previous unit's file — with its full version history — intact.

// p710Service wires the ActiveTaskService against the harness pool: the
// P7-08 CRUD service (the derivation's write path) plus the phase/plan/unit
// repositories the current-work-unit resolution reads.
func p710Service(t *testing.T, h *Harness) *service.ActiveTaskService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewActiveTaskService(
		p708Service(t, h),
		dbrepo.NewPhaseRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
	)
}

// p710UnitWithSpec seeds a work unit through the repository so its
// specification (objective / acceptance / tests / constraints) is persisted
// as real rows (the shared direct-SQL seedUnit helper carries empty spec
// columns, which would render empty sections).
func p710UnitWithSpec(t *testing.T, h *Harness, planID uuid.UUID, seq int, name, status string, acc, tests, cons []string) uuid.UUID {
	t.Helper()
	st, err := wbs.ParseWorkUnitStatus(status)
	if err != nil {
		t.Fatalf("ParseWorkUnitStatus: %v", err)
	}
	u, err := wbs.NewWorkUnit(planID, seq, name, wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("NewWorkUnit: %v", err)
	}
	u.Status = st
	u.Specification = &wbs.WorkUnitSpecification{
		Objective:          "Objective " + name,
		AcceptanceCriteria: acc,
		RequiredTests:      tests,
		Constraints:        cons,
	}
	if err := dbrepo.NewWorkUnitRepository(h.App.DB().Raw()).Create(context.Background(), u); err != nil {
		t.Fatalf("create unit: %v", err)
	}
	return u.ID
}

// p710UnitSignature is the full work_units row rendered as a stable string
// (every column, timestamps included) — the T7.11.7 non-mutation proof
// compares before/after signatures byte-for-byte.
func p710UnitSignature(t *testing.T, h *Harness, unitID uuid.UUID) string {
	t.Helper()
	var sig string
	q := "SELECT string_agg(col || '=' || coalesce(val, '<null>'), ',' ORDER BY col) FROM (SELECT col, val FROM (VALUES"
	for _, col := range []string{
		"status", "name", "description", "sequence", "objective",
		"implementation_instructions", "acceptance_criteria", "required_tests",
		"constraints", "created_at", "updated_at", "started_at", "completed_at",
	} {
		q += "('" + col + "', (SELECT " + col + "::text FROM work_units WHERE id = '" + unitID.String() + "')),"
	}
	q = q[:len(q)-1] + ") v(col, val)) s"
	if err := h.DBScan(context.Background(), q, &sig); err != nil {
		t.Fatalf("unit signature: %v", err)
	}
	return sig
}

// p710ActiveTaskFile creates the project's ACTIVE_TASK file bound to the
// given unit through the P7-08 service (the derivation op would create the
// same file on first use — the global ACTIVE_TASK type is P7-07 seeded).
func p710ActiveTaskFile(t *testing.T, h *Harness, crud *service.StateFileService, projectID, unitID uuid.UUID) uuid.UUID {
	t.Helper()
	typeID := p709DefaultTypeID(t, h, "ACTIVE_TASK")
	fv, err := crud.CreateFile(context.Background(), service.CreateFileInput{
		ProjectID:  projectID.String(),
		TypeID:     typeID.String(),
		Scope:      "WORK_UNIT",
		WorkUnitID: ptrStr(unitID.String()),
	})
	if err != nil {
		t.Fatalf("create active-task file: %v", err)
	}
	return uuid.MustParse(fv.ID)
}

// p710PlanOfPhase returns the id of the READY plan of the project's first
// phase (the seedPlan helper needs the phase id).
func p710PlanOfPhase(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	// The p705Rig fixture already seeds a READY v1 plan (with its own seq-1
	// unit); seed a fresh v2 plan instead — the active-plan rule (WBS
	// 2.19) picks the highest non-SUPERSEDED version, so v2 is the plan
	// the derivation resolves, with this test's own units.
	var phaseID uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM phases WHERE project_id = '"+projectID.String()+"' ORDER BY phase_order LIMIT 1", &phaseID); err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	return seedPlan(t, h, phaseID, 1, 2, "READY")
}

// T7.11.1-T7.11.7 — derivation for the current unit: the generated content
// carries the unit id, objective, acceptance criteria, required tests, and
// constraints; generation is non-mutating (the unit row is byte-identical
// before/after).
func TestP710ActiveTaskDerivation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	crud := p708Service(t, h)
	svc := p710Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	unit := p710UnitWithSpec(t, h, planID, 1, "p710-current", "EXECUTING",
		[]string{"derive active task"}, []string{"T7.11.1"}, []string{"non-mutating"})
	fileID := p710ActiveTaskFile(t, h, crud, projectID, unit)

	before := p710UnitSignature(t, h, unit)

	v, err := svc.DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if v.StateFileID != fileID.String() {
		t.Fatalf("derived file %s, want %s", v.StateFileID, fileID)
	}
	if v.Version != 1 {
		t.Fatalf("first derivation version = %d, want 1", v.Version)
	}
	for _, want := range []string{
		unit.String(),
		"Objective p710-current",
		"- derive active task",
		"- T7.11.1",
		"- non-mutating",
	} {
		if !strings.Contains(v.Content, want) {
			t.Fatalf("derived content missing %q:\n%s", want, v.Content)
		}
	}

	// T7.11.7: the work-unit row is byte-identical after generation.
	after := p710UnitSignature(t, h, unit)
	if before != after {
		t.Fatalf("derivation altered the work-unit row:\nbefore: %s\nafter:  %s", before, after)
	}
}

// T7.12.1-T7.12.4 — synchronization on unit change: unit A DONE -> unit B
// selected -> the derivation now writes unit B's file with content matching
// B (T7.12.1/T7.12.3); the previous active-task content is retained in
// unit A's file history (T7.12.2); a hand-edit of the materialized content
// moves only the file version — the DB work unit stays authoritative
// (T7.12.4).
func TestP710ActiveTaskSynchronization(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	crud := p708Service(t, h)
	svc := p710Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	unitA := p710UnitWithSpec(t, h, planID, 1, "p710-a", "EXECUTING",
		[]string{"A criteria"}, []string{"T7.12.1"}, []string{"A constraints"})
	unitB := p710UnitWithSpec(t, h, planID, 2, "p710-b", "PENDING",
		[]string{"B criteria"}, []string{"T7.12.2"}, []string{"B constraints"})
	fileA := p710ActiveTaskFile(t, h, crud, projectID, unitA)

	// First derivation: unit A is current; the content matches A.
	v1, err := svc.DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive A: %v", err)
	}
	if v1.StateFileID != fileA.String() || v1.Version != 1 {
		t.Fatalf("first derivation = (file %s, v%d), want (file %s, v1)", v1.StateFileID, v1.Version, fileA)
	}
	if !strings.Contains(v1.Content, unitA.String()) || !strings.Contains(v1.Content, "A criteria") {
		t.Fatalf("v1 content does not match unit A:\n%s", v1.Content)
	}

	// Unit A DONE, unit B selected (direct-SQL status bookkeeping — the
	// work-unit lifecycle is not the surface under test).
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at=now() WHERE id='"+unitA.String()+"'"); err != nil {
		t.Fatalf("mark A done: %v", err)
	}

	// T7.12.1/T7.12.3: the new current unit is derived — unit B's file,
	// version 1, content matching B.
	v2, err := svc.DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive B: %v", err)
	}
	if v2.StateFileID == fileA.String() {
		t.Fatalf("derivation for unit B wrote unit A's file %s", fileA)
	}
	if !strings.Contains(v2.Content, unitB.String()) || !strings.Contains(v2.Content, "B criteria") {
		t.Fatalf("v2 content does not match unit B:\n%s", v2.Content)
	}

	// T7.12.2: the previous active task is retained historically — unit
	// A's file still carries the unit-A derivation at version 1.
	hist, err := crud.GetVersion(ctx, fileA.String(), 1)
	if err != nil {
		t.Fatalf("read unit A history: %v", err)
	}
	if hist.Content != v1.Content {
		t.Fatalf("historical content = %q, want the unit-A derivation", hist.Content)
	}
	if !strings.Contains(hist.Content, unitA.String()) {
		t.Fatalf("retained history lost the unit-A identity:\n%s", hist.Content)
	}

	// T7.12.4: modifying the materialized content cannot silently change the
	// current work unit. The global ACTIVE_TASK default is MANAGED (the
	// domain default source class), so the controlled-update path exists;
	// the hand edit appends a version to unit B's file, and the
	// derivation's next re-run overwrites it with the unit-derived content.
	// The unit row's status is asserted by direct SQL before and after.
	var statusBefore string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id='"+unitB.String()+"'", &statusBefore); err != nil {
		t.Fatalf("scan unit B status: %v", err)
	}
	fileB := uuid.MustParse(v2.StateFileID)
	if _, err := crud.UpdateContent(ctx, fileB.String(), "hand-edited materialized content", 1); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	redrawn, err := svc.DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("re-derive after edit: %v", err)
	}
	if !strings.Contains(redrawn.Content, unitB.String()) || strings.Contains(redrawn.Content, "hand-edited") {
		t.Fatalf("re-derivation did not restore the unit-derived content:\n%s", redrawn.Content)
	}
	var statusAfter string
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id='"+unitB.String()+"'", &statusAfter); err != nil {
		t.Fatalf("scan unit B status after: %v", err)
	}
	if statusBefore != statusAfter || statusAfter != "PENDING" {
		t.Fatalf("unit status changed by materialized edit: before=%q after=%q", statusBefore, statusAfter)
	}
}

// A project with no current work unit (no phase / no plan / all-DONE plan)
// is a clean no-current-unit: service.ErrNotFound and nothing written.
func TestP710DeriveNoCurrentUnit(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p710Service(t, h)
	projectID := uuid.MustParse(createPhaseProject(t, h))

	_, err := svc.DeriveActiveTask(ctx, projectID.String())
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("no phase: err = %v, want service.ErrNotFound", err)
	}
	var files int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_files", &files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 0 {
		t.Fatalf("state files written on a no-unit derivation = %d, want 0", files)
	}

	// An all-DONE plan is equally a no-current-unit.
	phaseID := seedPhaseStatus(t, h, projectID, 1, "Only", "PENDING")
	planID := seedPlan(t, h, phaseID, 1, 1, "READY")
	p710UnitWithSpec(t, h, planID, 1, "p710-done", "DONE", nil, nil, nil)
	_, err = svc.DeriveActiveTask(ctx, projectID.String())
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("all-DONE plan: err = %v, want service.ErrNotFound", err)
	}
}

// Concurrent derivations for the same current unit: the advisory-locked
// version append (P7-06) keeps the version sequence contiguous with no
// duplicates — the op-level companion to the -race gate.
func TestP710DeriveConcurrentContiguousVersions(t *testing.T) {
	h := NewHarness(t)
	svc := p710Service(t, h)
	crud := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	unit := p710UnitWithSpec(t, h, planID, 1, "p710-conc", "PENDING", nil, nil, nil)
	fileID := p710ActiveTaskFile(t, h, crud, projectID, unit)

	const racers = 5
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			_, err := svc.DeriveActiveTask(context.Background(), projectID.String())
			errs <- err
		}()
	}
	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent derivation: %v", err)
		}
	}
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != racers {
		t.Fatalf("version rows = %d, want %d", n, racers)
	}
	var maxV, minV int
	if err := h.DBScan(context.Background(),
		"SELECT max(version), min(version) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &maxV, &minV); err != nil {
		t.Fatalf("max/min version: %v", err)
	}
	if minV != 1 || maxV != racers {
		t.Fatalf("version range = [%d, %d], want [1, %d]", minV, maxV, racers)
	}
}
