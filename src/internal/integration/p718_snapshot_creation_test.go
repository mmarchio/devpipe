package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-18 (WBS 7.31/7.32): snapshot creation service + consistency.
//
// ExecutionSnapshotService.CreateExecutionSnapshot composes, against ONE
// consistent view of persisted state, the full creation pipeline (WBS 7.31):
// read runtime state -> resolve state -> resolve role snapshot -> resolve
// role + state state-file requirements -> resolve correctly scoped files ->
// verify required files -> capture exact versions -> persist. The whole
// read+persist is one dbrepo.Run transaction (nil-txPool sequential
// fallback), so every read sees a single consistent position.
//
// Consistency (WBS 7.32): the runtime row's version is read at the top of
// each attempt and re-read before persist; a concurrent transition in
// between (version moved) is detected and the attempt is RETRIED with a
// fresh view — the op either succeeds against a fresh position or fails,
// never persisting a mixed context (T7.32.2/T7.32.3); a bounded retry
// budget ends in ErrConflict (fail, not persist). Every failure path
// leaves zero rows (T7.32.4, asserted by direct SQL).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not
// fix them):
//   - ROLE RESOLUTION: the role ASSIGNED to the runtime's current state in
//     the SMALLEST ordinal position of state_role_assignments (the ordered
//     participating-role list, WBS 6.4). Roles are visited in ordinal
//     order; a disabled role is skipped; no assignment: the project's role
//     with the smallest key; no usable role at all: ErrNotFound.
//   - CONSISTENCY GUARD: the runtime row's optimistic-concurrency version
//     (WBS 6.24) — the value a transition bumps atomically. The op reads
//     it at the top of each attempt and re-reads before persist; a move in
//     between is detected (T7.32.2) and the attempt is re-run against the
//     fresh position (T7.32.3 retry); a bounded retry budget ends in
//     ErrConflict (fail, not persist).
//   - MISSING REQUIRED FILE: the P7-13 semantics (service.
//     ErrMissingStateFile) prevent the snapshot; nothing is persisted
//     (T7.31.5, T7.32.4).
//   - The snapshot's StateFiles entries carry the CURRENT version, hash
//     and exact content of each resolved file (WBS 7.30 pins) and are
//     ordered by type key (deterministic).
//
// Service-only surface (the read API is P7-23); the tests drive the op
// over the harness pool.

// p718Svc wires the snapshot-creation service over the harness pool.
func p718Svc(t *testing.T, h *Harness) *service.ExecutionSnapshotService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewExecutionSnapshotService(
		dbrepo.NewRuntimeStateRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateRoleAssignmentRepository(raw),
		dbrepo.NewRoleRepository(raw),
		p718RoleSvc(t, h),
		p713Svc(t, h),
	).WithSnapshots(dbrepo.NewContextSnapshotRepository(raw)).
		WithFileVersions(dbrepo.NewStateFileVersionRepository(raw)).
		WithTxPool(raw)
}

// p718RoleSvc wires a fully-populated RoleService (the snapshot role read
// joins the attempt's transaction through it).
func p718RoleSvc(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	return snapshotSvc(t, h)
}

// p718Rig seeds a complete snapshot-ready fixture and returns all ids:
// a project + phase + work unit (p705Rig), a project machine with one
// enabled state S, the project role P718-ROLE (required type A) assigned
// to S at ordinal 0, the state requiring type B, the resolved files A
// (PROJECT scope, content "A content") and B (PHASE scope, content "B
// content"), and the runtime state at S with the machine binding and the
// phase association.
func p718Rig(t *testing.T, h *Harness) (projectID, roleID uuid.UUID, stateID uuid.UUID, fileA, fileB uuid.UUID) {
	t.Helper()
	proj, _, _ := p705Rig(t, h)
	ctx := context.Background()
	raw := h.App.DB().Raw()

	// Machine v1 with one enabled state S.
	mid := p617SeedMachine(t, h, proj.String(), "p718-machine",
		[]service.MachineStateDef{{Key: "S", Kind: "ACTION"}},
		nil, "S")
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'S'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	// Project role requiring A; state S requiring B; the role is the
	// state's assigned role (ordinal 0).
	roleID = p712ProjectRole(t, h, proj, "P718-ROLE")
	p712RequireRoleFiles(t, h, roleID, []string{"A"})
	p712RequireStateFiles(t, h, stateID, []string{"B"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: roleID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Required types A (PROJECT scope) and B (PHASE scope) with files and
	// v1 content.
	typeA := p718Required(t, h, proj, "A", "PROJECT")
	typeB := p718Required(t, h, proj, "B", "PHASE")
	svc8 := p708Service(t, h)
	fileA = p713ProjectFile(t, h, proj, typeA)
	ph := p718PhaseID(t, h, proj)
	fileB = p713PhaseFile(t, h, proj, ph, typeB)
	if _, err := svc8.UpdateContent(ctx, fileA.String(), "A content", 0); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if _, err := svc8.UpdateContent(ctx, fileB.String(), "B content", 0); err != nil {
		t.Fatalf("write B: %v", err)
	}

	// Runtime state at S with the machine binding and phase association
	// (the P6-20 repository op).
	var machineRowID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM state_machines WHERE id = '"+mid+"'", &machineRowID); err != nil {
		t.Fatalf("scan machine: %v", err)
	}
	rt := dbrepo.NewRuntimeStateRepository(raw)
	if err := rt.Create(ctx, &persistence.RuntimeStateRecord{
		ID: uuid.New(), ProjectID: proj,
		StateMachineID: machineRowID, StateMachineVersion: 1,
		CurrentStateID: stateID, PhaseID: &ph, WorkUnitID: nil,
		EnteredAt: time.Now().UTC(), Version: 1,
	}); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	return proj, roleID, stateID, fileA, fileB
}

// p718PhaseID returns the project's phase id (the p705Rig's single phase).
func p718PhaseID(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var pid uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM phases WHERE project_id = '"+projectID.String()+"' LIMIT 1", &pid); err != nil {
		t.Fatalf("scan phase: %v", err)
	}
	return pid
}

// p718Required creates a project-scoped required type.
func p718Required(t *testing.T, h *Harness, projectID uuid.UUID, key, scope string) uuid.UUID {
	t.Helper()
	return p713Required(t, h, projectID, key, scope, true)
}

// p718RoleID looks up the fixture's project role by key.
func p718RoleID(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM roles WHERE project_id = '"+projectID.String()+"' AND key = 'P718-ROLE'", &id); err != nil {
		t.Fatalf("scan role: %v", err)
	}
	return id
}

// p718SnapshotCount returns the project's persisted snapshot rows by direct
// SQL — the no-partial-persist and persistence proofs never trust the
// service's own readback.
func p718SnapshotCount(t *testing.T, h *Harness, projectID uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM context_snapshots WHERE project_id = '"+projectID.String()+"'", &n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return n
}

// p718SnapRow reads the project's single snapshot row's scalar position
// fields by direct SQL.
func p718SnapRow(t *testing.T, h *Harness, projectID uuid.UUID) (runtimeVersion, machineVersion int, stateKey string) {
	t.Helper()
	ctx := context.Background()
	if err := h.DBScan(ctx,
		"SELECT runtime_state_version, state_machine_version, state_key FROM context_snapshots WHERE project_id = '"+projectID.String()+"'",
		&runtimeVersion, &machineVersion, &stateKey); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	return
}

// p718FileSnapshots returns the persisted (type_key, scope, version, hash)
// tuples of the project's snapshot files, ordered by type_key.
func p718FileSnapshots(t *testing.T, h *Harness, projectID uuid.UUID) [][4]string {
	t.Helper()
	ctx := context.Background()
	rows, err := h.App.DB().Raw().Query(ctx,
		"SELECT f.type_key, f.scope, f.version::text, f.content_hash FROM context_snapshot_files f JOIN context_snapshots s ON s.id = f.snapshot_id WHERE s.project_id = $1 ORDER BY f.type_key", projectID)
	if err != nil {
		t.Fatalf("query file snapshots: %v", err)
	}
	defer rows.Close()
	var out [][4]string
	for rows.Next() {
		var key, scope, version, hash string
		if err := rows.Scan(&key, &scope, &version, &hash); err != nil {
			t.Fatalf("scan file snapshot: %v", err)
		}
		out = append(out, [4]string{key, scope, version, hash})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// p718RuntimeVersion reads the current runtime row version by direct SQL.
func p718RuntimeVersion(t *testing.T, h *Harness, projectID uuid.UUID) int {
	t.Helper()
	var v int
	if err := h.DBScan(context.Background(), "SELECT version FROM runtime_state WHERE project_id = '"+projectID.String()+"'", &v); err != nil {
		t.Fatalf("scan version: %v", err)
	}
	return v
}

// T7.31.1 — A snapshot is created successfully and persisted.
func TestP718SnapshotCreated(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	svc := p718Svc(t, h)
	if p718SnapshotCount(t, h, proj) != 0 {
		t.Fatal("precondition: snapshots already present")
	}
	if _, err := svc.CreateExecutionSnapshot(context.Background(), proj.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	if n := p718SnapshotCount(t, h, proj); n != 1 {
		t.Fatalf("persisted snapshot count = %d, want 1", n)
	}
}

// T7.31.2 — The snapshot captures the CURRENT runtime version: the runtime
// position is advanced once BEFORE the op, so the captured version is
// provably the current one, not the initial one.
func TestP718CurrentRuntimeVersionCaptured(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	ctx := context.Background()
	rt := dbrepo.NewRuntimeStateRepository(h.App.DB().Raw())
	rec, err := rt.Get(ctx, proj)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if err := rt.UpdateWithVersion(ctx, rec, rec.Version); err != nil {
		t.Fatalf("advance runtime: %v", err)
	}
	want := p718RuntimeVersion(t, h, proj)
	if want != 2 {
		t.Fatalf("precondition: runtime version = %d, want 2", want)
	}
	svc := p718Svc(t, h)
	snap, err := svc.CreateExecutionSnapshot(ctx, proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	if snap.RuntimeStateVersion != want {
		t.Fatalf("captured runtime version = %d, want %d", snap.RuntimeStateVersion, want)
	}
	storedVer, _, _ := p718SnapRow(t, h, proj)
	if storedVer != want {
		t.Fatalf("persisted runtime version = %d, want %d", storedVer, want)
	}
}

// T7.31.3 — The correct role is captured: the state's assigned role at the
// smallest ordinal, with the state key pinned alongside.
func TestP718CorrectRoleCaptured(t *testing.T) {
	h := NewHarness(t)
	proj, roleID, _, _, _ := p718Rig(t, h)
	svc := p718Svc(t, h)
	snap, err := svc.CreateExecutionSnapshot(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	if snap.RoleSnapshot.RoleID != roleID {
		t.Fatalf("captured role = %s, want %s", snap.RoleSnapshot.RoleID, roleID)
	}
	if snap.RoleSnapshot.RoleKey != "P718-ROLE" {
		t.Fatalf("captured role key = %q, want P718-ROLE", snap.RoleSnapshot.RoleKey)
	}
	if snap.StateKey != "S" {
		t.Fatalf("captured state key = %q, want S", snap.StateKey)
	}
}

// T7.31.4 — The correct state files are captured: each required type's
// resolved file at its current version, with the content hash of the exact
// content, scope retained, ordered by type key — both in the returned
// snapshot and in the persisted rows.
func TestP718CorrectStateFilesCaptured(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, fileA, fileB := p718Rig(t, h)
	svc := p718Svc(t, h)
	snap, err := svc.CreateExecutionSnapshot(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	if len(snap.StateFiles) != 2 {
		t.Fatalf("captured %d files, want 2", len(snap.StateFiles))
	}
	wantA := statefile.ContentHash("A content")
	wantB := statefile.ContentHash("B content")
	if snap.StateFiles[0].StateFileID != fileA || snap.StateFiles[0].TypeKey != "A" ||
		snap.StateFiles[0].Scope != statefile.ScopeProject || snap.StateFiles[0].Version != 1 ||
		snap.StateFiles[0].Hash != wantA || snap.StateFiles[0].Content != "A content" {
		t.Fatalf("file A = %+v, want id %s v1 hash %s", snap.StateFiles[0], fileA, wantA)
	}
	if snap.StateFiles[1].StateFileID != fileB || snap.StateFiles[1].TypeKey != "B" ||
		snap.StateFiles[1].Scope != statefile.ScopePhase || snap.StateFiles[1].Version != 1 ||
		snap.StateFiles[1].Hash != wantB || snap.StateFiles[1].Content != "B content" {
		t.Fatalf("file B = %+v, want id %s v1 hash %s", snap.StateFiles[1], fileB, wantB)
	}
	rows := p718FileSnapshots(t, h, proj)
	if len(rows) != 2 {
		t.Fatalf("persisted %d file rows, want 2", len(rows))
	}
	if rows[0] != [4]string{"A", "PROJECT", "1", wantA} || rows[1] != [4]string{"B", "PHASE", "1", wantB} {
		t.Fatalf("persisted files = %v", rows)
	}
}

// T7.31.5 — A missing required file PREVENTS the snapshot: the op fails
// with the P7-13 missing-file error and nothing is persisted.
func TestP718MissingRequiredPreventsSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	// The role additionally requires type C, which has no file.
	p712RequireRoleFiles(t, h, p718RoleID(t, h, proj), []string{"A", "C"})
	if p718Required(t, h, proj, "C", "PROJECT") == uuid.Nil {
		t.Fatal("type C not created")
	}
	svc := p718Svc(t, h)
	if _, err := svc.CreateExecutionSnapshot(context.Background(), proj.String()); !errors.Is(err, service.ErrMissingStateFile) {
		t.Fatalf("err = %v, want ErrMissingStateFile", err)
	}
	if n := p718SnapshotCount(t, h, proj); n != 0 {
		t.Fatalf("snapshot persisted despite missing required file (%d rows)", n)
	}
}

// T7.32.1 — The snapshot reflects ONE consistent runtime position: every
// recorded position field (runtime id/version, machine version, state key)
// matches the single runtime row that existed at creation time.
func TestP718OneConsistentPosition(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	svc := p718Svc(t, h)
	snap, err := svc.CreateExecutionSnapshot(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	var rtID uuid.UUID
	var rtVer, rtMachineVer int
	var stateID uuid.UUID
	ctx := context.Background()
	if err := h.DBScan(ctx, "SELECT id, version, state_machine_version, current_state_id FROM runtime_state WHERE project_id = '"+proj.String()+"'",
		&rtID, &rtVer, &rtMachineVer, &stateID); err != nil {
		t.Fatalf("scan runtime: %v", err)
	}
	if snap.RuntimeStateID != rtID || snap.RuntimeStateVersion != rtVer {
		t.Fatalf("snapshot runtime = %s v%d, want %s v%d", snap.RuntimeStateID, snap.RuntimeStateVersion, rtID, rtVer)
	}
	if snap.StateMachineVersion != rtMachineVer {
		t.Fatalf("snapshot machine version = %d, want %d", snap.StateMachineVersion, rtMachineVer)
	}
	var stateKey string
	if err := h.DBScan(ctx, "SELECT key FROM workflow_states WHERE id = '"+stateID.String()+"'", &stateKey); err != nil {
		t.Fatalf("scan state key: %v", err)
	}
	if snap.StateKey != stateKey {
		t.Fatalf("snapshot state key = %q, want %q", snap.StateKey, stateKey)
	}
}

// T7.32.2 — A concurrent runtime transition is DETECTED: the in-flight
// probe seam (WithRuntimeProbe, used ONLY here) performs one genuine
// runtime transition between the read and the persist of the first
// attempt — the op's version guard observes the move and the attempt
// retries against the fresh position. The persisted snapshot must record
// the POST-transition version (a mixed context would record the old one).
func TestP718ConcurrentTransitionDetected(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	probe := &p718Probe{h: h, once: true}
	svc := p718Svc(t, h).WithRuntimeProbe(probe.run)
	snap, err := svc.CreateExecutionSnapshot(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	want := p718RuntimeVersion(t, h, proj)
	if snap.RuntimeStateVersion != want {
		t.Fatalf("captured runtime version = %d, want post-transition %d", snap.RuntimeStateVersion, want)
	}
	if probe.bump != 1 {
		t.Fatalf("probe fired %d times, want 1 (the transition the guard detected)", probe.bump)
	}
}

// T7.32.3 — A concurrent transition makes the op RETRY (or FAIL) rather
// than persist a mixed context: the probe fires on EVERY attempt (a
// perpetual concurrent writer), the bounded retry budget is exhausted, and
// the op fails with ErrConflict persisting nothing.
func TestP718RetriesOrFailNotPersistMixed(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _ := p718Rig(t, h)
	probe := &p718Probe{h: h}
	svc := p718Svc(t, h).WithRuntimeProbe(probe.run)
	if _, err := svc.CreateExecutionSnapshot(context.Background(), proj.String()); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict (retry budget exhausted)", err)
	}
	if n := p718SnapshotCount(t, h, proj); n != 0 {
		t.Fatalf("mixed-context snapshot persisted (%d rows)", n)
	}
	if probe.bump < 2 {
		t.Fatalf("probe fired %d times, want >= 2 (the retry must have re-attempted)", probe.bump)
	}
}

// T7.32.4 — No partial snapshot is persisted after ANY failure. Two
// failure classes are pinned: a missing required file, and a forced
// persistence failure (the WithPersistProbe seam, used ONLY here). Both
// must leave zero snapshot rows AND zero snapshot-file rows (the persist
// is one atomic unit).
func TestP718NoPartialSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, roleID, _, _, _ := p718Rig(t, h)
	// Case 1: missing required file (the T7.31.5 shape).
	p712RequireRoleFiles(t, h, roleID, []string{"A", "NOPE"})
	if p718Required(t, h, proj, "NOPE", "PROJECT") == uuid.Nil {
		t.Fatal("type NOPE not created")
	}
	svc := p718Svc(t, h)
	if _, err := svc.CreateExecutionSnapshot(context.Background(), proj.String()); !errors.Is(err, service.ErrMissingStateFile) {
		t.Fatalf("case 1 err = %v, want ErrMissingStateFile", err)
	}
	if n := p718SnapshotCount(t, h, proj); n != 0 {
		t.Fatalf("case 1: partial snapshot persisted (%d rows)", n)
	}
	// Case 2: a forced persistence failure — the role's requirements are
	// restored to the resolvable set (case 1's missing-file residue would
	// fail the op before the persist step), so the injected failure is
	// the op's actual outcome; the injected error must fire (non-vacuity)
	// and the snapshot + file rows must both be absent.
	p712RequireRoleFiles(t, h, roleID, []string{"A"})
	failing := &p718FailingPersist{}
	svc2 := p718Svc(t, h).WithPersistProbe(failing.persist)
	if _, err := svc2.CreateExecutionSnapshot(context.Background(), proj.String()); err == nil {
		t.Fatal("case 2: expected the injected persistence failure")
	}
	if !failing.fired {
		t.Fatal("case 2: the injected failure never fired — the test is vacuous")
	}
	if n := p718SnapshotCount(t, h, proj); n != 0 {
		t.Fatalf("case 2: partial snapshot persisted (%d rows)", n)
	}
	var fileRows int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM context_snapshot_files", &fileRows); err != nil {
		t.Fatalf("count file rows: %v", err)
	}
	if fileRows != 0 {
		t.Fatalf("case 2: %d snapshot file rows persisted", fileRows)
	}
}

// p718Probe is the T7.32.2/T7.32.3 in-flight transition probe: the
// WithRuntimeProbe seam runs it between the read and the persist of one
// attempt; it performs ONE genuine runtime transition (the real P6-20
// repository op) and counts how many attempts it fired in.
type p718Probe struct {
	h *Harness
	// once limits the probe to a single transition (T7.32.2: the op
	// succeeds after one detected move); without it the probe fires on
	// every attempt (T7.32.3: perpetual concurrent writer).
	once bool
	bump int
}

func (p *p718Probe) run(projectID string) bool {
	if p.once && p.bump >= 1 {
		return false
	}
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return false
	}
	rt := dbrepo.NewRuntimeStateRepository(p.h.App.DB().Raw())
	rec, err := rt.Get(context.Background(), proj)
	if err != nil {
		return false
	}
	if err := rt.UpdateWithVersion(context.Background(), rec, rec.Version); err == nil {
		p.bump++
		return true
	}
	return false
}

// p718FailingPersist is the forced persistence failure (the
// WithPersistProbe seam, used ONLY by T7.32.4): the op's persist step
// returns its error; fired proves the injection actually ran.
type p718FailingPersist struct {
	fired bool
}

func (f *p718FailingPersist) persist() error {
	f.fired = true
	return errors.New("injected persistence failure")
}
