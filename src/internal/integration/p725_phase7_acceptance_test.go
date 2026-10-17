package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-25 (WBS 7.44/7.45/7.46): the Phase 7 full-flow acceptance areas over
// the existing state-file + snapshot pipeline. Test-only: no production
// code — the behavior already exists (the P7-13 scope resolution, the
// P7-10 per-unit ACTIVE_TASK derivation, the P7-18 snapshot creation, and
// the P7-15/16 materialization + drift surface), so RED is likely
// unreachable; if the whole file passes on its first run that is
// recorded per the task constraint. The tests are load-bearing: they pin
// file ids, exact versions, exact content, and the DERIVED aggregate
// snapshot hash (recomputed from the persisted row, WBS 7.34) — never
// row counts alone.
//
// Fixture policy (stated so TESTER/PM can verify):
//   - Unit statuses (EXECUTING/DONE) and phase statuses (PENDING/DONE)
//     are direct-SQL test bookkeeping — the work-unit/phase lifecycle is
//     not under test (the same pattern as the P6-39 full-flow, the P7-10
//     sync tests, and the P7-24 flows).
//   - The 7.44 fixture (p725Rig) carries two phases: phase A (DONE, its
//     unit DONE) and phase B (current — its plan's first non-DONE unit B
//     is EXECUTING). The runtime is initialized on the P724-style
//     machine and bound to phase B + unit B by a direct-SQL UPDATE (the
//     P6-32 fixture pattern; the machine never writes phase/unit rows).
//   - The 7.45 restart uses the P6-37/P7-19 reconstruction pattern:
//     CREATE DATABASE, two NewHarnessOn instances on the same persisted
//     database, and byte-for-byte comparison of every persisted position
//     field, resolution output, version/hash signature, and the DERIVED
//     aggregate snapshot hash (recomputed from the persisted snapshot
//     row on each side — the hash is not stored, WBS 7.34).
//   - The 7.46 isolation fixture gives both projects IDENTICAL logical
//     type keys (the global P7-07 defaults — i.e. identical filenames)
//     and distinct content per project.

// p725Rig seeds the phase-advancement fixture and returns the ids the
// scenarios need.
func p725Rig(t *testing.T, h *Harness) (projectID, phaseA, phaseB, unitA, unitB uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	projectID, _, _ = p705Rig(t, h) // phaseA + plan + unitA (PENDING)
	planA := p710PlanOfPhase(t, h, projectID)
	unitA = p710UnitWithSpec(t, h, planA, 1, "p725-a", "DONE",
		[]string{"A criteria"}, []string{"T7.44"}, []string{"A constraints"})
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE', completed_at = now() WHERE id = '"+unitA.String()+"'"); err != nil {
		t.Fatalf("mark unit A done: %v", err)
	}
	phaseA = p718PhaseID(t, h, projectID)
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE id = '"+phaseA.String()+"'"); err != nil {
		t.Fatalf("mark phase A done: %v", err)
	}

	phaseB = seedPhaseStatus(t, h, projectID, 2, "Expansion", "PENDING")
	planB := seedPlan(t, h, phaseB, 2, 1, "READY")
	seedUnit(t, h, planB, 1, "p725-b", "EXECUTING")
	if err := h.DBScan(ctx, "SELECT id FROM work_units WHERE execution_plan_id = '"+planB.String()+"'", &unitB); err != nil {
		t.Fatalf("scan unit B: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE work_units SET project_id = '"+projectID.String()+"' WHERE id = '"+unitB.String()+"'"); err != nil {
		t.Fatalf("backfill unit B project: %v", err)
	}

	machine := p617SeedMachine(t, h, projectID.String(), "p725-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}},
		nil, "IMPLEMENT")
	implement := p620StateIDs(t, h, machine, "IMPLEMENT")
	roleID := p712ProjectRole(t, h, projectID, "P725-ROLE")
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "ACTIVE_TASK", "PHASE_STATE"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(h.App.DB().Raw())
	now := time.Now().UTC()
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(implement), RoleID: roleID, Ordinal: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Project-level file: the single current PROJECT_STATE (advanced to
	// the new phase by a content write — the P7-09 update path).
	svc8 := p708Service(t, h)
	ps := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "PROJECT_STATE"))
	if _, err := svc8.UpdateContent(ctx, ps.String(), "project: in phase B", 0); err != nil {
		t.Fatalf("write project state: %v", err)
	}
	// Phase A's state (retained after the advancement).
	phA := p713PhaseFile(t, h, projectID, phaseA, p709DefaultTypeID(t, h, "PHASE_STATE"))
	if _, err := svc8.UpdateContent(ctx, phA.String(), "phase A: completed", 0); err != nil {
		t.Fatalf("write phase A state: %v", err)
	}
	// Phase B's state (the new current phase).
	phB := p713PhaseFile(t, h, projectID, phaseB, p709DefaultTypeID(t, h, "PHASE_STATE"))
	if _, err := svc8.UpdateContent(ctx, phB.String(), "phase B: in progress", 0); err != nil {
		t.Fatalf("write phase B state: %v", err)
	}
	// The ACTIVE_TASK bound to the current (phase B) unit, created the
	// way the P7-10 derivation creates it.
	at := p710ActiveTaskFile(t, h, svc8, projectID, unitB)
	if _, err := svc8.UpdateContent(ctx, at.String(), "active task: unit B "+unitB.String(), 0); err != nil {
		t.Fatalf("write active task: %v", err)
	}

	if _, err := p622Service(h, true).Initialize(ctx, projectID.String(), machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseB.String()+"', work_unit_id = '"+unitB.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime to phase B: %v", err)
	}
	return
}

// p725FilePhase returns the file's (type key, phase association).
func p725FilePhase(t *testing.T, h *Harness, fileID uuid.UUID) (key string, phase uuid.UUID) {
	t.Helper()
	var keyNull *string
	var phaseNull *uuid.UUID
	q := "SELECT ft.key, sf.phase_id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE sf.id = '" + fileID.String() + "'"
	if err := h.DBScan(context.Background(), q, &keyNull, &phaseNull); err != nil {
		t.Fatalf("scan file meta: %v", err)
	}
	if keyNull != nil {
		key = *keyNull
	}
	if phaseNull != nil {
		phase = *phaseNull
	}
	return
}

// p725FileSig is a byte-signature of the project's state_files rows:
// (id, type key, scope, associations, current version) — any change to
// any persisted state-file byte is visible.
func p725FileSig(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	var sig string
	q := "SELECT coalesce(string_agg(sf.id::text || '|' || ft.key || '|' || sf.scope || '|' || coalesce(sf.phase_id::text, '-') || '|' || coalesce(sf.work_unit_id::text, '-') || '|' || sf.current_version::text, ',' ORDER BY sf.id), '') FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE sf.project_id = '" + projectID.String() + "'"
	if err := h.DBScan(context.Background(), q, &sig); err != nil {
		t.Fatalf("file signature: %v", err)
	}
	return sig
}

// p725VersionSig is a byte-signature of the project's state-file version
// rows (the versions + hashes the 7.45 list must restore).
func p725VersionSig(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	var sig string
	q := "SELECT coalesce(string_agg(v.state_file_id::text || ':' || v.version::text || ':' || v.content_hash, ',' ORDER BY v.state_file_id, v.version), '') FROM state_file_versions v WHERE v.state_file_id IN (SELECT id FROM state_files WHERE project_id = '" + projectID.String() + "')"
	if err := h.DBScan(context.Background(), q, &sig); err != nil {
		t.Fatalf("version signature: %v", err)
	}
	return sig
}

// p725RuntimeSig is a byte-signature of the project's persisted runtime
// position (the 7.45 runtime-position-restored proof).
func p725RuntimeSig(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	var sig string
	q := "SELECT current_state_id::text || '|' || version::text || '|' || coalesce(phase_id::text, '-') || '|' || coalesce(work_unit_id::text, '-') || '|' || state_machine_id::text || '|' || state_machine_version::text FROM runtime_state WHERE project_id = '" + projectID.String() + "'"
	if err := h.DBScan(context.Background(), q, &sig); err != nil {
		t.Fatalf("runtime signature: %v", err)
	}
	return sig
}

// p725Reconstruct reassembles the domain snapshot from the persisted
// snapshot row + file rows (the WBS 7.34 inputs: everything except the
// row id and created_at, which are excluded from the hash by design) and
// returns it with its derived aggregate hash.
func p725Reconstruct(t *testing.T, h *Harness, rec *persistence.ContextSnapshotRecord, snapID uuid.UUID) statefile.ExecutionContextSnapshot {
	t.Helper()
	var rs role.RoleSnapshot
	if err := json.Unmarshal(rec.RoleSnapshotJSON, &rs); err != nil {
		t.Fatalf("unmarshal role snapshot: %v", err)
	}
	var files []statefile.StateFileSnapshot
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT type_key, scope, version, content_hash, content FROM context_snapshot_files WHERE snapshot_id = $1 ORDER BY type_key", snapID)
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f statefile.StateFileSnapshot
		if err := rows.Scan(&f.TypeKey, &f.Scope, &f.Version, &f.Hash, &f.Content); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return statefile.ExecutionContextSnapshot{
		ProjectID:           rec.ProjectID,
		PhaseID:             rec.PhaseID,
		WorkUnitID:          rec.WorkUnitID,
		RuntimeStateID:      rec.RuntimeStateID,
		RuntimeStateVersion: rec.RuntimeStateVersion,
		StateMachineID:      rec.StateMachineID,
		StateMachineVersion: rec.StateMachineVersion,
		StateKey:            rec.StateKey,
		RoleSnapshot:        rs,
		StateFiles:          files,
	}
}

// WBS 7.44 — Phase advancement state files (T7.44.1-T7.44.5).
func TestP725PhaseAdvancement(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseA, phaseB, unitA, unitB := p725Rig(t, h)
	svc8 := p708Service(t, h)

	// T7.44.1 — The new phase's state is selected: PHASE_STATE resolves
	// to phase B's file at the type's declared scope.
	got, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var gotPhaseFile uuid.UUID
	for _, r := range got {
		if key, _ := p725FilePhase(t, h, r.File.ID); key == "PHASE_STATE" {
			gotPhaseFile = r.File.ID
		}
	}
	var phaseBState uuid.UUID
	if err := h.DBScan(ctx, "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'PHASE_STATE' AND sf.scope = 'PHASE' AND sf.phase_id = '"+phaseB.String()+"' AND sf.project_id = '"+projectID.String()+"'", &phaseBState); err != nil {
		t.Fatalf("scan phase B state: %v", err)
	}
	if gotPhaseFile == uuid.Nil || gotPhaseFile != phaseBState {
		t.Fatalf("resolved PHASE_STATE = %s, want phase B's file %s (T7.44.1)", gotPhaseFile, phaseBState)
	}

	// T7.44.2 — The old phase's state is retained at its exact version:
	// phase A's file + content read through the versioned read path.
	var phaseAState uuid.UUID
	if err := h.DBScan(ctx, "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'PHASE_STATE' AND sf.scope = 'PHASE' AND sf.phase_id = '"+phaseA.String()+"'", &phaseAState); err != nil {
		t.Fatalf("scan phase A state: %v", err)
	}
	if phaseAState == uuid.Nil {
		t.Fatal("phase A's PHASE_STATE file is gone (T7.44.2)")
	}
	cur, err := svc8.GetCurrentVersion(ctx, phaseAState.String())
	if err != nil {
		t.Fatalf("current version: %v", err)
	}
	if cur.Version != 1 || cur.Content != "phase A: completed" {
		t.Fatalf("phase A state = v%d %q, want v1 %q (T7.44.2)", cur.Version, cur.Content, "phase A: completed")
	}
	hist, err := svc8.GetVersion(ctx, phaseAState.String(), 1)
	if err != nil {
		t.Fatalf("read phase A v1: %v", err)
	}
	if hist.Content != "phase A: completed" {
		t.Fatalf("phase A v1 = %q (T7.44.2)", hist.Content)
	}

	// T7.44.3 — The project state reflects the new phase: exactly ONE
	// project-scoped PROJECT_STATE file, carrying the advanced content.
	var projStateCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_files WHERE project_id = '"+projectID.String()+"' AND scope = 'PROJECT' AND type_id = '"+p709DefaultTypeID(t, h, "PROJECT_STATE").String()+"'", &projStateCount); err != nil {
		t.Fatalf("count project state: %v", err)
	}
	if projStateCount != 1 {
		t.Fatalf("project-scoped PROJECT_STATE rows = %d, want exactly 1 (T7.44.3)", projStateCount)
	}
	var projState uuid.UUID
	if err := h.DBScan(ctx, "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'PROJECT_STATE' AND sf.scope = 'PROJECT' AND sf.project_id = '"+projectID.String()+"'", &projState); err != nil {
		t.Fatalf("scan project state: %v", err)
	}
	psv, err := svc8.GetCurrentVersion(ctx, projState.String())
	if err != nil {
		t.Fatalf("current project state: %v", err)
	}
	if psv.Content != "project: in phase B" {
		t.Fatalf("PROJECT_STATE = %q, want the advanced %q (T7.44.3)", psv.Content, "project: in phase B")
	}

	// T7.44.4 — The active task does not leak the previous phase: the
	// derived ACTIVE_TASK names unit B (phase B) only.
	derived, err := p710Service(t, h).DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if !strings.Contains(derived.Content, unitB.String()) {
		t.Fatalf("derived ACTIVE_TASK does not reference the current unit B:\n%s", derived.Content)
	}
	if strings.Contains(derived.Content, unitA.String()) {
		t.Fatalf("derived ACTIVE_TASK leaks previous-phase unit A:\n%s", derived.Content)
	}
	if got := p724ActiveTaskFileID(t, h, unitB); uuid.MustParse(derived.StateFileID) != got {
		t.Fatalf("derivation wrote %s, want the current unit's file %s", derived.StateFileID, got)
	}

	// T7.44.5 — A snapshot pins no wrong-phase scoped file.
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	pins := p724SnapFiles(t, h, snap.ID)
	ph, ok := pins["PHASE_STATE"]
	if !ok {
		t.Fatalf("snapshot pins no PHASE_STATE: %v", pins)
	}
	if ph.fileID != phaseBState {
		t.Fatalf("snapshot PHASE_STATE pin = file %s, want phase B's %s (T7.44.5)", ph.fileID, phaseBState)
	}
	if ph.content != "phase B: in progress" {
		t.Fatalf("snapshot PHASE_STATE content = %q (T7.44.5)", ph.content)
	}
	for key, pin := range pins {
		if pin.fileID == phaseAState {
			t.Fatalf("snapshot pins phase A's file under key %s (T7.44.5)", key)
		}
	}
	// The snapshot's position is the advanced one.
	rec, err := p723Svc(t, h).GetSnapshot(ctx, projectID.String(), snap.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if rec.PhaseID == nil || *rec.PhaseID != phaseB || rec.WorkUnitID == nil || *rec.WorkUnitID != unitB {
		t.Fatalf("snapshot position = phase %v unit %v, want (%s %s)", rec.PhaseID, rec.WorkUnitID, phaseB, unitB)
	}
}

// WBS 7.45 — Restart reconstruction: with a project mid-flight, the
// runtime position, required-file resolution, versions, hashes, snapshot
// hash, and drift detection all restore after a service restart — no
// in-memory cache required for correctness (every value is re-read from
// the persisted database).
func TestP725RestartReconstruction(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName)
	ctx = context.Background()
	projectID, _, _, _, unitB := p725Rig(t, h1)

	// Mid-flight: the project is RUNNING (direct-SQL bookkeeping, the
	// P6-39 pattern) and the role requires the full context set.
	if _, err := h1.DBExec(ctx, "UPDATE projects SET status = 'RUNNING' WHERE id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	var roleID uuid.UUID
	if err := h1.DBScan(ctx, "SELECT id FROM roles WHERE project_id = '"+projectID.String()+"'", &roleID); err != nil {
		t.Fatalf("scan role: %v", err)
	}
	p712RequireRoleFiles(t, h1, roleID, []string{"PROJECT_STATE", "ACTIVE_TASK", "PHASE_STATE", "ARCHITECTURE"})
	arch := p713ProjectFile(t, h1, projectID, p709DefaultTypeID(t, h1, "ARCHITECTURE"))
	if _, err := p708Service(t, h1).UpdateContent(ctx, arch.String(), "architecture: service + persistence", 0); err != nil {
		t.Fatalf("write architecture: %v", err)
	}

	// Pre-restart captures (all read through the services / direct SQL
	// over h1's pool — the values the restart must restore).
	res1, err := p713Svc(t, h1).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	var res1Sig strings.Builder
	for _, r := range res1 {
		ver, verr := p708Service(t, h1).GetCurrentVersion(ctx, r.File.ID.String())
		if verr != nil {
			t.Fatalf("current version: %v", verr)
		}
		fmt.Fprintf(&res1Sig, "%s|v%d|%s\n", r.File.ID, ver.Version, ver.ContentHash)
	}
	before := map[string]string{
		"runtime":    p725RuntimeSig(t, h1, projectID),
		"resolution": res1Sig.String(),
		"versions":   p725VersionSig(t, h1, projectID),
		"files":      p725FileSig(t, h1, projectID),
	}
	beforeSnapSig := "" // captured after the snapshot exists, before h1 closes
	snap1, err := p718Svc(t, h1).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	beforeSnapSig = p719Sig(t, h1, projectID)
	rec1, err := p723Svc(t, h1).GetSnapshot(ctx, projectID.String(), snap1.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot 1: %v", err)
	}
	pins1 := p718FileSnapshots(t, h1, projectID)
	snap1dom := p725Reconstruct(t, h1, rec1, snap1.ID)
	snapHash1 := snap1dom.SnapshotHash()
	// Drift is detectable PRE-restart (baseline; the same check after the
	// restart proves the post-restart half).
	root := t.TempDir()
	if _, err := p714Svc(t, h1).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize 1: %v", err)
	}
	psFile := p725FileIDByPhaseKey(t, h1, projectID, "PROJECT_STATE")
	target := filepath.Join(root, ".overseer", "state", "PROJECT_STATE.md")
	if err := os.WriteFile(target, []byte("externally edited after materialization"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	if res, derr := p715Svc(t, h1).DetectDrift(ctx, root, projectID.String(), psFile.String()); derr != nil || res.Status != service.DriftDrifted {
		t.Fatalf("pre-restart drift = %v/%v, want DRIFTED (baseline)", res, derr)
	}

	// RESTART: h1 goes away; h2 boots on the SAME persisted database.
	h1.Close()
	h2 := NewHarnessOn(t, base, dbName)
	defer h2.Close()

	// Runtime position restored.
	if got := p725RuntimeSig(t, h2, projectID); got != before["runtime"] {
		t.Fatalf("runtime position differs after restart:\nbefore: %s\nafter:  %s", before["runtime"], got)
	}
	// Required-file resolution identical (file id, version, hash).
	res2, err := p713Svc(t, h2).ResolveStateFileScopes(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	var res2Sig strings.Builder
	for _, r := range res2 {
		ver, verr := p708Service(t, h2).GetCurrentVersion(ctx, r.File.ID.String())
		if verr != nil {
			t.Fatalf("current version: %v", verr)
		}
		fmt.Fprintf(&res2Sig, "%s|v%d|%s\n", r.File.ID, ver.Version, ver.ContentHash)
	}
	if res2Sig.String() != before["resolution"] {
		t.Fatalf("required-file resolution differs after restart:\nbefore:\n%s\nafter:\n%s", before["resolution"], res2Sig.String())
	}
	// Versions + hashes identical.
	if got := p725VersionSig(t, h2, projectID); got != before["versions"] {
		t.Fatalf("file version/hash signature differs after restart:\nbefore: %s\nafter:  %s", before["versions"], got)
	}
	if got := p725FileSig(t, h2, projectID); got != before["files"] {
		t.Fatalf("state-file signature differs after restart:\nbefore: %s\nafter:  %s", before["files"], got)
	}
	// The persisted snapshot row is byte-identical, and the DERIVED
	// aggregate snapshot hash recomputes identically.
	if got := p719Sig(t, h2, projectID); got != beforeSnapSig {
		t.Fatalf("snapshot rows differ after restart:\nbefore:\n%s\nafter:\n%s", beforeSnapSig, got)
	}
	rec2, err := p723Svc(t, h2).GetSnapshot(ctx, projectID.String(), snap1.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot 2: %v", err)
	}
	if string(rec2.RoleSnapshotJSON) != string(rec1.RoleSnapshotJSON) {
		t.Fatal("snapshot role snapshot bytes differ after restart")
	}
	snap2dom := p725Reconstruct(t, h2, rec2, snap1.ID)
	snapHash2 := snap2dom.SnapshotHash()
	if snapHash2 != snapHash1 {
		t.Fatalf("aggregate snapshot hash differs after restart: %s vs %s", snapHash1, snapHash2)
	}
	// A NEW snapshot after the restart pins the identical tuple set (the
	// live state is unchanged, so the pins are the same (key, scope,
	// version, hash) rows — appended after the pre-restart pins).
	snap3, err := p718Svc(t, h2).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot 3: %v", err)
	}
	all := p725PinsOf(t, h2, snap3.ID)
	if len(all) != len(pins1) {
		t.Fatalf("post-restart snapshot pins = %d rows, want %d", len(all), len(pins1))
	}
	for i, want := range pins1 {
		if all[i] != want {
			t.Fatalf("post-restart snapshot pin %d = %v, want %v", i, all[i], want)
		}
	}
	// Materialization drift stays detectable after the restart: the
	// externally edited PROJECT_STATE copy is DRIFTED on h2's pool.
	res, err := p715Svc(t, h2).DetectDrift(ctx, root, projectID.String(), psFile.String())
	if err != nil {
		t.Fatalf("detect after restart: %v", err)
	}
	if res.Status != service.DriftDrifted {
		t.Fatalf("post-restart drift = %s, want DRIFTED", res.Status)
	}
	// Non-vacuity: the position is mid-flight (unit B bound, phase B).
	var boundUnit uuid.UUID
	if err := h2.DBScan(ctx, "SELECT work_unit_id FROM runtime_state WHERE project_id = '"+projectID.String()+"'", &boundUnit); err != nil {
		t.Fatalf("scan binding: %v", err)
	}
	if boundUnit != unitB {
		t.Fatalf("runtime bound unit = %s, want %s", boundUnit, unitB)
	}
}

// p725PinsOf returns one snapshot's persisted (type_key, scope, version,
// hash) tuples ordered by type_key (the p718FileSnapshots shape scoped to a
// single snapshot, so the two snapshots' rows never interleave).
func p725PinsOf(t *testing.T, h *Harness, snapID uuid.UUID) [][4]string {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT type_key, scope, version::text, content_hash FROM context_snapshot_files WHERE snapshot_id = $1 ORDER BY type_key", snapID)
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	var out [][4]string
	for rows.Next() {
		var key, scope, version, hash string
		if err := rows.Scan(&key, &scope, &version, &hash); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		out = append(out, [4]string{key, scope, version, hash})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// p725FileIDByPhaseKey returns the project's file of the given global type
// key (scope per the key's default declaration).
func p725FileIDByPhaseKey(t *testing.T, h *Harness, projectID uuid.UUID, key string) uuid.UUID {
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

// WBS 7.46 — State-file isolation: two simultaneous projects with
// identical logical type keys and filenames never cross boundaries
// (T7.46.1-T7.46.4).
func TestP725StateFileIsolation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	setup := func(name string) (projectID, phaseID, unitID uuid.UUID) {
		projectID, phaseID, _ = p705Rig(t, h)
		plan := p710PlanOfPhase(t, h, projectID)
		unitID = p710UnitWithSpec(t, h, plan, 1, "p725-"+name+"-unit", "EXECUTING",
			[]string{name + " criteria"}, []string{"T7.46"}, []string{name + " constraints"})
		roleID := p712ProjectRole(t, h, projectID, "P725-"+name)
		p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "ACTIVE_TASK", "PHASE_STATE"})
		svc8 := p708Service(t, h)
		ps := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "PROJECT_STATE"))
		if _, err := svc8.UpdateContent(ctx, ps.String(), name+": project state", 0); err != nil {
			t.Fatalf("write %s project state: %v", name, err)
		}
		ph := p713PhaseFile(t, h, projectID, phaseID, p709DefaultTypeID(t, h, "PHASE_STATE"))
		if _, err := svc8.UpdateContent(ctx, ph.String(), name+": phase state", 0); err != nil {
			t.Fatalf("write %s phase state: %v", name, err)
		}
		at := p710ActiveTaskFile(t, h, svc8, projectID, unitID)
		if _, err := svc8.UpdateContent(ctx, at.String(), name+": active task "+unitID.String(), 0); err != nil {
			t.Fatalf("write %s active task: %v", name, err)
		}
		return
	}
	projA, _, _ := setup("A")
	projB, _, _ := setup("B")

	// T7.46.1 — Project A resolves only A files.
	resA, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projA.String())
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	// T7.46.2 — Project B resolves only B files.
	resB, err := p713Svc(t, h).ResolveStateFileScopes(ctx, projB.String())
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	checkOwn := func(t *testing.T, own uuid.UUID, got []service.ResolvedStateFile, other string) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("resolution is empty")
		}
		for _, r := range got {
			var owner uuid.UUID
			if err := h.DBScan(ctx, "SELECT project_id FROM state_files WHERE id = '"+r.File.ID.String()+"'", &owner); err != nil {
				t.Fatalf("scan owner: %v", err)
			}
			if owner != own {
				t.Fatalf("resolution for project %s contains project %s's file %s (cross-project leak)", own, other, r.File.ID)
			}
		}
	}
	checkOwn(t, projA, resA, "B")
	checkOwn(t, projB, resB, "A")
	// Content isolation: none of A's resolved files carries B's content.
	for _, r := range resA {
		ver, verr := p708Service(t, h).GetCurrentVersion(ctx, r.File.ID.String())
		if verr != nil {
			t.Fatalf("current version: %v", verr)
		}
		if strings.Contains(ver.Content, "B:") {
			t.Fatalf("project A's resolved file carries project B's content: %q", ver.Content)
		}
	}

	// T7.46.3 — A cross-project file id is rejected at the read surface:
	// a file of project B addressed through project A's scope is
	// ErrNotFound (and vice versa).
	svc8 := p708Service(t, h)
	var aProjState, bProjState uuid.UUID
	if err := h.DBScan(ctx, "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'PROJECT_STATE' AND sf.project_id = '"+projA.String()+"'", &aProjState); err != nil {
		t.Fatalf("scan A file: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'PROJECT_STATE' AND sf.project_id = '"+projB.String()+"'", &bProjState); err != nil {
		t.Fatalf("scan B file: %v", err)
	}
	if _, _, _, err := svc8.GetFile(ctx, projA.String(), bProjState.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project GetFile(A, B-file) err = %v, want ErrNotFound", err)
	}
	if _, _, _, err := svc8.GetFile(ctx, projB.String(), aProjState.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project GetFile(B, A-file) err = %v, want ErrNotFound", err)
	}
	// A version read of the foreign file is rejected the same way.
	if _, err := svc8.GetFileVersion(ctx, projA.String(), bProjState.String(), 1); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project GetFileVersion err = %v, want ErrNotFound", err)
	}

	// T7.46.4 — Materialization paths are isolated: each project
	// materializes into its OWN root; the filename is identical for both
	// (the same global type key) and the contents never mix.
	rootA, rootB := t.TempDir(), t.TempDir()
	if _, err := p714Svc(t, h).Materialize(ctx, rootA, projA.String()); err != nil {
		t.Fatalf("materialize A: %v", err)
	}
	if _, err := p714Svc(t, h).Materialize(ctx, rootB, projB.String()); err != nil {
		t.Fatalf("materialize B: %v", err)
	}
	aState, err := os.ReadFile(filepath.Join(rootA, ".overseer", "state", "PROJECT_STATE.md"))
	if err != nil {
		t.Fatalf("read A materialization: %v", err)
	}
	if string(aState) != "A: project state" {
		t.Fatalf("A materialized PROJECT_STATE = %q", aState)
	}
	bState, err := os.ReadFile(filepath.Join(rootB, ".overseer", "state", "PROJECT_STATE.md"))
	if err != nil {
		t.Fatalf("read B materialization: %v", err)
	}
	if string(bState) != "B: project state" {
		t.Fatalf("B materialized PROJECT_STATE = %q", bState)
	}
	if got, statErr := os.ReadFile(filepath.Join(rootA, ".overseer", "state", "ARCHITECTURE.md")); !os.IsNotExist(statErr) {
		t.Fatalf("A's state dir carries a file it never wrote: %q", got)
	}
}
