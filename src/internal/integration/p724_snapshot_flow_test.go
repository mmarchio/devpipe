package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/state"
)

// P7-24 (WBS 7.41/7.42/7.43): full-flow snapshot acceptance over the
// existing pipeline — the P7-18 creation op, the P7-23 read surface
// (ListSnapshots/GetSnapshot), the P7-10 derived ACTIVE_TASK (per-unit
// files), and the P6-13/P6-21 transition machinery. Test-only: no
// production code — the behavior already exists, so RED was unreachable
// (recorded per the task constraint).
//
// Fixture policy (stated so TESTER/PM can verify):
//   - The fixture role (P724-ROLE) requires PROJECT_STATE (PROJECT
//     scope) + ACTIVE_TASK (WORK_UNIT scope); the 7.43 test extends the
//     same role with PHASE_STATE + ARCHITECTURE (set-replacement).
//   - The machine is a single IMPLEMENT state + the two context fork
//     destinations (CONTINUE_CURRENT_SESSION / RESET_SESSION) with the
//     outcome-token transitions; the runtime is initialized via the
//     P6-22 service and the phase/unit binding is a direct-SQL UPDATE
//     (the P6-32 fixture pattern — the runtime row stays at v1).
//   - Unit statuses (EXECUTING/DONE) are direct-SQL bookkeeping, the
//     same pattern the P6-39 full-flow and P7-10 sync tests use.
//   - Snapshot pins are asserted by direct read through the P7-23
//     GetSnapshot record (pinned file id + version + exact content),
//     never row counts alone.

// p724Rig seeds the P7-24 snapshot fixture: a project with a READY plan
// carrying units A (seq 1, EXECUTING) and B (seq 2, PENDING), the
// P724 machine + role (assigned to IMPLEMENT at ordinal 0), the
// PROJECT_STATE + ACTIVE_TASK context files (the ACTIVE_TASK file bound
// to the current unit A, content referencing unit A), and the runtime at
// IMPLEMENT with the phase + unit A binding. It returns the ids the
// scenarios need.
func p724Rig(t *testing.T, h *Harness, keySuffix string) (projectID, phaseID, unitA, unitB uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	projectID, phaseID, _ = p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, projectID)
	unitA = p710UnitWithSpec(t, h, planID, 1, "p724-a"+keySuffix, "EXECUTING",
		[]string{"A criteria"}, []string{"T7.41"}, []string{"A constraints"})
	unitB = p710UnitWithSpec(t, h, planID, 2, "p724-b"+keySuffix, "PENDING",
		[]string{"B criteria"}, []string{"T7.42"}, []string{"B constraints"})

	machine := p617SeedMachine(t, h, projectID.String(), "p724-machine"+keySuffix,
		[]service.MachineStateDef{
			{Key: "IMPLEMENT", Kind: "ACTION"},
			{Key: "CONTINUE_CURRENT_SESSION", Kind: "CONTROL"},
			{Key: "RESET_SESSION", Kind: "CONTROL"},
		},
		[]service.MachineTransitionDef{
			{From: "IMPLEMENT", Outcome: state.OutcomeContinueCurrentSession, To: "CONTINUE_CURRENT_SESSION"},
			{From: "IMPLEMENT", Outcome: state.OutcomeResetSession, To: "RESET_SESSION"},
		},
		"IMPLEMENT")
	implement := p620StateIDs(t, h, machine, "IMPLEMENT")

	roleID := p712ProjectRole(t, h, projectID, "P724-ROLE"+keySuffix)
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "ACTIVE_TASK"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(h.App.DB().Raw())
	now := time.Now().UTC()
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(implement), RoleID: roleID, Ordinal: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	svc8 := p708Service(t, h)
	ps := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "PROJECT_STATE"))
	if _, err := svc8.UpdateContent(ctx, ps.String(), "project state: in flight", 0); err != nil {
		t.Fatalf("write project state: %v", err)
	}
	// The ACTIVE_TASK file bound to the CURRENT unit is created the way
	// the P7-10 rig does, so the first snapshot pins a real unit-A
	// version.
	at := p710ActiveTaskFile(t, h, svc8, projectID, unitA)
	if _, err := svc8.UpdateContent(ctx, at.String(), "active task: unit A "+unitA.String(), 0); err != nil {
		t.Fatalf("write active task: %v", err)
	}

	if _, err := p622Service(h, true).Initialize(ctx, projectID.String(), machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"', work_unit_id = '"+unitA.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p724Pin is one persisted snapshot file pin (the load-bearing tuple:
// the file it pins, at which version, with which exact content).
type p724Pin struct {
	fileID  uuid.UUID
	version int
	content string
}

// p724SnapFiles maps a persisted snapshot's file pins to their type key.
func p724SnapFiles(t *testing.T, h *Harness, snapID uuid.UUID) map[string]p724Pin {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT type_key, state_file_id, version, content FROM context_snapshot_files WHERE snapshot_id = $1", snapID)
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	out := map[string]p724Pin{}
	for rows.Next() {
		var key, scope string
		var id uuid.UUID
		var ver int
		var content string
		if err := rows.Scan(&key, &id, &ver, &content); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		out[key] = p724Pin{fileID: id, version: ver, content: content}
		_ = scope
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// p724ActiveTaskFileID returns the ACTIVE_TASK file currently bound to the
// given unit (the P7-10 per-unit-file pattern).
func p724ActiveTaskFileID(t *testing.T, h *Harness, unitID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	q := "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'ACTIVE_TASK' AND sf.scope = 'WORK_UNIT' AND sf.work_unit_id = '" + unitID.String() + "'"
	if err := h.DBScan(context.Background(), q, &id); err != nil {
		t.Fatalf("scan active-task file: %v", err)
	}
	return id
}

// p724MachineID returns the project's single state machine id.
func p724MachineID(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(), "SELECT id::text FROM state_machines WHERE project_id = '"+projectID.String()+"'", &id); err != nil {
		t.Fatalf("scan machine: %v", err)
	}
	return id
}

// p724RuntimeVersion reads the persisted runtime version (the value the
// next transition's ExpectedVersion must carry).
func p724RuntimeVersion(t *testing.T, h *Harness, projectID uuid.UUID) int {
	t.Helper()
	var v int
	if err := h.DBScan(context.Background(), "SELECT version FROM runtime_state WHERE project_id = '"+projectID.String()+"'", &v); err != nil {
		t.Fatalf("scan runtime version: %v", err)
	}
	return v
}

// p724SnapshotIDs lists the project's persisted snapshot ids in creation
// order.
func p724SnapshotIDs(t *testing.T, h *Harness, projectID uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT id FROM context_snapshots WHERE project_id = $1 ORDER BY created_at, id", projectID)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan snapshot id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// WBS 7.41 — Work-unit change integration: with unit A EXECUTING and a
// snapshot pinning the ACTIVE_TASK derived for A, completing A and
// selecting B must bind the NEW unit's ACTIVE_TASK (the per-unit-file
// pattern), keep v1 available, make current resolution follow the new
// unit, pin the new version in the next snapshot, and keep the
// historical snapshot on its old pin.
func TestP724WorkUnitChange(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitA, unitB := p724Rig(t, h, "a")
	readSvc := p723Svc(t, h)

	// The historical snapshot pins the ACTIVE_TASK version for unit A.
	snap1, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	fileA := p724ActiveTaskFileID(t, h, unitA)
	pins1 := p724SnapFiles(t, h, snap1.ID)
	at1, ok := pins1["ACTIVE_TASK"]
	if !ok || at1.fileID != fileA {
		t.Fatalf("snapshot 1 ACTIVE_TASK pin = %+v, want file %s", at1, fileA)
	}
	if at1.version != 1 {
		t.Fatalf("snapshot 1 ACTIVE_TASK version = %d, want 1", at1.version)
	}

	// Complete unit A and select unit B (direct-SQL bookkeeping — the
	// P6-39/P7-10 pattern; the work-unit lifecycle is not under test).
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at=now() WHERE id='"+unitA.String()+"'"); err != nil {
		t.Fatalf("mark A done: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status='EXECUTING' WHERE id='"+unitB.String()+"'"); err != nil {
		t.Fatalf("select B: %v", err)
	}

	// 7.41.3 — current resolution follows the new unit: the derived
	// ACTIVE_TASK now references B, in B's own file (a new per-unit
	// version series, not a write into A's file).
	derived, err := p710Service(t, h).DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if !strings.Contains(derived.Content, unitB.String()) || strings.Contains(derived.Content, unitA.String()) {
		t.Fatalf("derived ACTIVE_TASK does not follow unit B:\\n%s", derived.Content)
	}
	fileB := p724ActiveTaskFileID(t, h, unitB)
	if fileB == fileA {
		t.Fatal("unit B's ACTIVE_TASK file is unit A's file — per-unit binding violated")
	}
	if uuid.MustParse(derived.StateFileID) != fileB {
		t.Fatalf("derivation wrote %s, want unit B's file %s", derived.StateFileID, fileB)
	}

	// 7.41.1 — v1 remains available: unit A's file keeps its derived
	// history (read through the P7-08 versioned read path).
	hist, err := p708Service(t, h).GetVersion(ctx, fileA.String(), at1.version)
	if err != nil {
		t.Fatalf("read unit A ACTIVE_TASK v%d: %v", at1.version, err)
	}
	if !strings.Contains(hist.Content, unitA.String()) {
		t.Fatalf("unit A's retained ACTIVE_TASK history lost the unit-A identity:\\n%s", hist.Content)
	}

	// 7.41.4 — the NEW snapshot pins the current ACTIVE_TASK (unit B's
	// file at the version the derivation just appended), read back
	// through the P7-23 read API.
	snap2, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	if _, err := readSvc.GetSnapshot(ctx, projectID.String(), snap2.ID.String()); err != nil {
		t.Fatalf("GetSnapshot 2: %v", err)
	}
	pins2 := p724SnapFiles(t, h, snap2.ID)
	at2, ok := pins2["ACTIVE_TASK"]
	if !ok || at2.fileID != fileB {
		t.Fatalf("snapshot 2 ACTIVE_TASK pin = %+v, want file %s", at2, fileB)
	}
	if !strings.Contains(at2.content, unitB.String()) || strings.Contains(at2.content, unitA.String()) {
		t.Fatalf("snapshot 2 ACTIVE_TASK content does not reference unit B:\\n%s", at2.content)
	}

	// 7.41.5 — the historical snapshot STILL pins the old version (unit
	// A's file at v1, byte-identical content) after the live file and
	// the current resolution both moved on.
	pins1b := p724SnapFiles(t, h, snap1.ID)
	at1b, ok := pins1b["ACTIVE_TASK"]
	if !ok || at1b.fileID != fileA || at1b.version != at1.version || at1b.content != at1.content {
		t.Fatalf("historical snapshot pin = %+v, want file %s v%d unchanged", at1b, fileA, at1.version)
	}

	// Both snapshots coexist in creation order.
	if ids := p724SnapshotIDs(t, h, projectID); len(ids) != 2 || ids[0] != snap1.ID || ids[1] != snap2.ID {
		t.Fatalf("persisted snapshots = %v, want [%s %s] in creation order", ids, snap1.ID, snap2.ID)
	}
}

// WBS 7.42 — Continuation snapshot: an INCOMPLETE outcome routed by the
// P6-13 resolver to same-session continuation keeps the next snapshot
// tied to the SAME project/phase/work unit (T7.42.1), the ACTIVE_TASK
// remains correct (T7.42.2), the project/phase context is unchanged
// (T7.42.3), and the snapshot identity/version is DISTINCT where the
// runtime moved (T7.42.4).
func TestP724ContinuationSnapshot(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID, unitA, _ := p724Rig(t, h, "c")

	// The P6-13 resolver: below threshold -> same-session continuation.
	token, err := state.ResolveContextThreshold(50, 80)
	if err != nil {
		t.Fatalf("resolve threshold: %v", err)
	}
	if token != state.OutcomeContinueCurrentSession {
		t.Fatalf("resolver(50,80) = %q, want %q", token, state.OutcomeContinueCurrentSession)
	}

	snap1, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}

	// The worker reports INCOMPLETE; the state machine selects
	// same-session continuation (the P6-21 apply moves the runtime).
	mid := p724MachineID(t, h, projectID)
	implement := p620StateIDs(t, h, mid, "IMPLEMENT")
	contID := p620StateIDs(t, h, mid, "CONTINUE_CURRENT_SESSION")
	base := p724RuntimeVersion(t, h, projectID)
	if _, err := p621Service(h, true).ApplyTransition(ctx, projectID.String(), &service.ApplyTransitionInput{
		FromStateID: implement, ToStateID: contID, Outcome: token, ExpectedVersion: base,
	}); err != nil {
		t.Fatalf("apply continuation: %v", err)
	}

	snap2, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	// Read back through the P7-23 read API (position fields live on the
	// record; the pins are read by direct SQL).
	rec2, err := p723Svc(t, h).GetSnapshot(ctx, projectID.String(), snap2.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}

	// T7.42.1 — work unit unchanged.
	if rec2.WorkUnitID == nil || *rec2.WorkUnitID != unitA {
		t.Fatalf("snapshot 2 work unit = %v, want %s", rec2.WorkUnitID, unitA)
	}
	// T7.42.3 — project/phase context unchanged.
	if rec2.ProjectID != snap1.ProjectID || rec2.PhaseID == nil || *rec2.PhaseID != phaseID {
		t.Fatalf("snapshot 2 project/phase = (%s %v), want (%s %s)", rec2.ProjectID, rec2.PhaseID, snap1.ProjectID, phaseID)
	}
	// T7.42.2 — the ACTIVE_TASK remains correct: unit A's file, pinned
	// content naming unit A.
	pins := p724SnapFiles(t, h, snap2.ID)
	fileA := p724ActiveTaskFileID(t, h, unitA)
	at, ok := pins["ACTIVE_TASK"]
	if !ok || at.fileID != fileA {
		t.Fatalf("snapshot 2 ACTIVE_TASK pin = %+v, want file %s", at, fileA)
	}
	if !strings.Contains(at.content, unitA.String()) {
		t.Fatalf("snapshot 2 ACTIVE_TASK content lost the unit-A identity:\\n%s", at.content)
	}
	// T7.42.4 — distinct identity, and the recorded runtime version moved
	// with the transition (the apply bumped the runtime row).
	if snap2.ID == snap1.ID {
		t.Fatal("continuation snapshot reused the historical snapshot id")
	}
	if rec2.RuntimeStateVersion != base+1 {
		t.Fatalf("snapshot 2 runtime version = %d, want %d (the transition bumped it)", rec2.RuntimeStateVersion, base+1)
	}
}

// WBS 7.43 — Context reset snapshot: the high-context path (INCOMPLETE ->
// context >= threshold -> RESET_SESSION) lets the NEW snapshot
// reconstruct the same logical task — same phase, same work unit, same
// ACTIVE_TASK — plus the CURRENT project/phase/architecture state, with
// no reliance on old conversation history (the snapshot content is the
// source of truth for the fresh session).
func TestP724ContextResetSnapshot(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID, unitA, _ := p724Rig(t, h, "r")
	readSvc := p723Svc(t, h)

	// The post-reset context: the role additionally requires
	// PHASE_STATE + ARCHITECTURE (the 7.43 verification set), seeded at
	// their declared scopes. SetStateFiles REPLACES the whole set, so
	// all four requirements are passed at once.
	svc8 := p708Service(t, h)
	phaseState := p713PhaseFile(t, h, projectID, phaseID, p709DefaultTypeID(t, h, "PHASE_STATE"))
	if _, err := svc8.UpdateContent(ctx, phaseState.String(), "phase: foundation, in progress", 0); err != nil {
		t.Fatalf("write phase state: %v", err)
	}
	arch := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "ARCHITECTURE"))
	if _, err := svc8.UpdateContent(ctx, arch.String(), "architecture: service + persistence", 0); err != nil {
		t.Fatalf("write architecture: %v", err)
	}
	var roleID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM roles WHERE project_id = '"+projectID.String()+"'", &roleID); err != nil {
		t.Fatalf("scan role: %v", err)
	}
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "ACTIVE_TASK", "PHASE_STATE", "ARCHITECTURE"})

	// The P6-13 resolver: at/above threshold -> RESET_SESSION.
	token, err := state.ResolveContextThreshold(90, 80)
	if err != nil {
		t.Fatalf("resolve threshold: %v", err)
	}
	if token != state.OutcomeResetSession {
		t.Fatalf("resolver(90,80) = %q, want %q", token, state.OutcomeResetSession)
	}

	// Walk the path through the real apply op: the INCOMPLETE report
	// reaches the CONTINUE_CURRENT_SESSION fork, the reset route moves to
	// RESET_SESSION.
	mid := p724MachineID(t, h, projectID)
	implement := p620StateIDs(t, h, mid, "IMPLEMENT")
	contID := p620StateIDs(t, h, mid, "CONTINUE_CURRENT_SESSION")
	rstID := p620StateIDs(t, h, mid, "RESET_SESSION")
	ver := p724RuntimeVersion(t, h, projectID)
	if _, err := p621Service(h, true).ApplyTransition(ctx, projectID.String(), &service.ApplyTransitionInput{
		FromStateID: implement, ToStateID: contID, Outcome: "INCOMPLETE", ExpectedVersion: ver,
	}); err != nil {
		t.Fatalf("apply INCOMPLETE: %v", err)
	}
	if _, err := p621Service(h, true).ApplyTransition(ctx, projectID.String(), &service.ApplyTransitionInput{
		FromStateID: contID, ToStateID: rstID, Outcome: token, ExpectedVersion: ver + 1,
	}); err != nil {
		t.Fatalf("apply reset: %v", err)
	}

	// The post-reset snapshot reconstructs the logical task with no old
	// conversation in play — its pinned content is the entire source of
	// truth.
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("post-reset snapshot: %v", err)
	}
	rec, err := readSvc.GetSnapshot(ctx, projectID.String(), snap.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	pins := p724SnapFiles(t, h, snap.ID)

	// Same phase + same work unit.
	if rec.PhaseID == nil || *rec.PhaseID != phaseID {
		t.Fatalf("post-reset phase = %v, want %s", rec.PhaseID, phaseID)
	}
	if rec.WorkUnitID == nil || *rec.WorkUnitID != unitA {
		t.Fatalf("post-reset work unit = %v, want %s", rec.WorkUnitID, unitA)
	}
	// Same ACTIVE_TASK: unit A's file, pinned content naming unit A (the
	// derivation output, not a session artifact).
	fileA := p724ActiveTaskFileID(t, h, unitA)
	at, ok := pins["ACTIVE_TASK"]
	if !ok || at.fileID != fileA {
		t.Fatalf("post-reset ACTIVE_TASK pin = %+v, want file %s", at, fileA)
	}
	if !strings.Contains(at.content, unitA.String()) {
		t.Fatalf("post-reset ACTIVE_TASK content lost the unit-A identity:\\n%s", at.content)
	}
	// CURRENT project/phase/architecture state, byte-exact against the
	// persisted current versions — these pins are exactly what a fresh
	// session reconstructs from.
	wantProjectState := p724CurrentFile(t, h, "PROJECT_STATE", projectID)
	if p, ok := pins["PROJECT_STATE"]; !ok || p.fileID != wantProjectState || p.content != "project state: in flight" {
		t.Fatalf("post-reset PROJECT_STATE pin = %+v, want file %s content %q", p, wantProjectState, "project state: in flight")
	}
	if p, ok := pins["PHASE_STATE"]; !ok || p.fileID != phaseState || p.content != "phase: foundation, in progress" {
		t.Fatalf("post-reset PHASE_STATE pin = %+v, want file %s", p, phaseState)
	}
	if p, ok := pins["ARCHITECTURE"]; !ok || p.fileID != arch || p.content != "architecture: service + persistence" {
		t.Fatalf("post-reset ARCHITECTURE pin = %+v, want file %s", p, arch)
	}
	// The snapshot is self-contained: every required file is pinned with
	// exact content — nothing is left to a prior session.
	if len(pins) != 4 {
		t.Fatalf("post-reset snapshot pins %d files, want 4 (PROJECT_STATE/ACTIVE_TASK/PHASE_STATE/ARCHITECTURE)", len(pins))
	}
}

// p724CurrentFile returns the project's file of the given global type key
// at its declared scope.
func p724CurrentFile(t *testing.T, h *Harness, key string, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	scope := "PROJECT"
	if key == "PHASE_STATE" || key == "TEST_STATE" {
		scope = "PHASE"
	}
	var id uuid.UUID
	q := "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = '" + key + "' AND sf.scope = '" + scope + "' AND sf.project_id = '" + projectID.String() + "'"
	if err := h.DBScan(context.Background(), q, &id); err != nil {
		t.Fatalf("scan %s file: %v", key, err)
	}
	return id
}
