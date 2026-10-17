package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
	"overseer/internal/service"
)

// P7-26 (WBS 7.47/7.48 + Phase 7 DoD): the Phase 7 close-out.
//
// 7.47 — concurrent snapshot consistency: CreateExecutionSnapshot raced
// against real concurrent state changes (ARCHITECTURE content updates,
// ACTIVE_TASK re-derivations, runtime transitions); AFTER the race EVERY
// persisted snapshot (all rows, not a sample) is validated against its
// recorded logical context — runtime version, phase, work unit, role
// version, state-machine version, and every pinned state-file version +
// hash (never a random mixture). Real goroutines over the harness pool +
// real PG; the -race gate covers the data-race half.
//
// 7.48 — the exact WBS 11-step end-to-end scenario: pinned versions
// (PROJECT_STATE v5, ARCHITECTURE v3, PHASE_STATE v2, ACTIVE_TASK v1 for
// Unit 7.1), the drift cycle (external edit -> detect -> DB authoritative
// -> restore), the 7.1 -> 7.2 rebind (ACTIVE_TASK v2), Snapshot A's
// continued reconstruction of Unit 7.1, and the restart comparison
// (A and B byte-identical to their pre-restart representations).
//
// Test-only: the 7.47/7.48 behaviors compose existing ops (P7-18/19
// snapshot creation + immutability, P7-10 derivation, P7-14/15/16
// materialize/drift, P6-21 transitions). If the whole file passes on its
// first run, RED was unreachable (recorded per the task constraint).
//
// Fixture policy (stated so TESTER/PM can verify):
//   - Unit statuses (EXECUTING/DONE) are direct-SQL bookkeeping, the
//     same pattern the P6-39 / P7-24 / P7-25 flows use (the work-unit
//     lifecycle is not under test).
//   - Versioned content writes pin their expected version by RE-READING
//     the current version first (the version-chaining rule).
//   - The 7.47 machine has two states (IMPLEMENT/VALIDATE) so the
//     concurrent transitions have a legal destination; both states
//     assign the same role (ordinal 0) and carry the same state-file
//     requirements, so every recorded position resolves the same four
//     files and stays fully validatable.
//   - The 7.48 restart uses the P6-37 / P7-19 / P7-25 reconstruction
//     pattern: CREATE DATABASE, two NewHarnessOn instances on the same
//     persisted database, byte-for-byte comparison of the persisted
//     snapshot rows + pins plus the DERIVED aggregate snapshot hash
//     (recomputed from the persisted rows on each side, WBS 7.34).

// p726FileID returns the project's state file of the given global type key
// at its declared scope.
func p726FileID(t *testing.T, h *Harness, projectID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	scope := "PROJECT"
	if key == "PHASE_STATE" || key == "TEST_STATE" {
		scope = "PHASE"
	}
	if key == "ACTIVE_TASK" {
		scope = "WORK_UNIT"
	}
	var id uuid.UUID
	q := "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = '" + key + "' AND sf.scope = '" + scope + "' AND sf.project_id = '" + projectID.String() + "' LIMIT 1"
	if err := h.DBScan(context.Background(), q, &id); err != nil {
		t.Fatalf("scan %s file: %v", key, err)
	}
	return id
}

// p726ActiveTaskFileIDFor returns the ACTIVE_TASK file bound to the given
// unit.
func p726ActiveTaskFileIDFor(t *testing.T, h *Harness, unitID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	q := "SELECT sf.id FROM state_files sf JOIN state_file_types ft ON ft.id = sf.type_id WHERE ft.key = 'ACTIVE_TASK' AND sf.scope = 'WORK_UNIT' AND sf.work_unit_id = '" + unitID.String() + "'"
	if err := h.DBScan(context.Background(), q, &id); err != nil {
		t.Fatalf("scan unit ACTIVE_TASK file: %v", err)
	}
	return id
}

// p726FileVersion reads a file's persisted current version.
func p726FileVersion(t *testing.T, h *Harness, fileID uuid.UUID) int {
	t.Helper()
	var v int
	if err := h.DBScan(context.Background(), "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &v); err != nil {
		t.Fatalf("scan file version: %v", err)
	}
	return v
}

// p726Sig returns the full-row byte signature of ONE persisted snapshot
// (all columns) plus its file pins (all columns, ordered by type_key) —
// any change to any persisted snapshot byte is visible.
func p726Sig(t *testing.T, h *Harness, snapID uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var row string
	if err := h.DBScan(ctx, "SELECT id::text||'|'||project_id::text||'|'||coalesce(phase_id::text,'-')||'|'||coalesce(work_unit_id::text,'-')||'|'||runtime_state_id::text||'|'||runtime_state_version::text||'|'||state_machine_id::text||'|'||state_machine_version::text||'|'||state_key||'|'||role_snapshot::text||'|'||state_file_tokens::text||'|'||role_tokens::text||'|'||estimated_total::text||'|'||created_at::text FROM context_snapshots WHERE id = '"+snapID.String()+"'", &row); err != nil {
		t.Fatalf("scan snapshot row: %v", err)
	}
	rows, err := h.App.DB().Raw().Query(ctx, "SELECT id::text||'|'||snapshot_id::text||'|'||state_file_id::text||'|'||type_key||'|'||scope||'|'||version::text||'|'||content_hash||'|'||content||'|'||state_file_token_estimate::text||'|'||classification||'|'||created_at::text FROM context_snapshot_files WHERE snapshot_id = $1 ORDER BY type_key", snapID)
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	var pins []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		pins = append(pins, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return row + "\n" + fmt.Sprint(pins)
}

// p726SnapRoleJSON returns the persisted role snapshot bytes of a snapshot
// row.
func p726SnapRoleJSON(t *testing.T, h *Harness, snapID uuid.UUID) string {
	t.Helper()
	var b string
	if err := h.DBScan(context.Background(), "SELECT role_snapshot::text FROM context_snapshots WHERE id = '"+snapID.String()+"'", &b); err != nil {
		t.Fatalf("scan role snapshot: %v", err)
	}
	return b
}

// p726ValidateSnapshot checks that ONE persisted snapshot is a valid
// single logical context (WBS 7.47): its pinned state-file versions all
// exist with the EXACT recorded hash + content (a random mixture would
// show up here), and its recorded role / state-machine / state / phase /
// unit are coherent with the persisted tables. The recorded runtime
// version is a HISTORICAL value (the live row's version moves forward
// under the concurrent transitions), so it is not compared to the live
// row — the pins are the load-bearing no-mixture proof.
func p726ValidateSnapshot(t *testing.T, h *Harness, projectID uuid.UUID, snapID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	var phaseID, unitID, machineID *uuid.UUID
	var stateKey string
	var roleJSON []byte
	if err := conn.QueryRow(ctx,
		"SELECT phase_id, work_unit_id, state_machine_id, state_key, role_snapshot FROM context_snapshots WHERE id = '"+snapID.String()+"'",
	).Scan(&phaseID, &unitID, &machineID, &stateKey, &roleJSON); err != nil {
		t.Fatalf("scan snapshot %s: %v", snapID, err)
	}
	// The recorded phase and unit belong to the project.
	if phaseID != nil {
		var owner uuid.UUID
		if err := conn.QueryRow(ctx, "SELECT project_id FROM phases WHERE id = '"+phaseID.String()+"'").Scan(&owner); err != nil || owner != projectID {
			t.Fatalf("snapshot %s phase %s is not the project's phase", snapID, phaseID)
		}
	}
	if unitID != nil {
		var owner uuid.UUID
		if err := conn.QueryRow(ctx, "SELECT project_id FROM work_units WHERE id = '"+unitID.String()+"'").Scan(&owner); err != nil || owner != projectID {
			t.Fatalf("snapshot %s unit %s is not the project's unit", snapID, unitID)
		}
	}
	// The recorded state key is a state of the captured machine family at
	// the recorded version (a fresh id per edit, WBS 6.20).
	var stateCount int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM workflow_states ws JOIN state_machines m ON m.id = ws.machine_id WHERE (m.id = $1 OR m.family_id = $1) AND ws.key = $2",
		machineID, stateKey).Scan(&stateCount); err != nil {
		t.Fatalf("scan state: %v", err)
	}
	if stateCount == 0 {
		t.Fatalf("snapshot %s state key %q is not a state of machine %s", snapID, stateKey, machineID)
	}
	// The recorded role version exists.
	var rs role.RoleSnapshot
	if err := json.Unmarshal(roleJSON, &rs); err != nil {
		t.Fatalf("unmarshal role snapshot: %v", err)
	}
	var roleCount int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM roles WHERE id = $1 AND version = $2", rs.RoleID, rs.RoleVersion).Scan(&roleCount); err != nil {
		t.Fatalf("scan role version: %v", err)
	}
	if roleCount == 0 {
		t.Fatalf("snapshot %s records role %s v%d which no longer exists", snapID, rs.RoleID, rs.RoleVersion)
	}
	// Collect the pins first (no open rows while running the per-pin
	// checks — a single pooled connection).
	type pinRow struct {
		key  string
		fid  uuid.UUID
		ver  int
		hash string
		cont string
	}
	var pins []pinRow
	prs, err := conn.Query(ctx,
		"SELECT type_key, state_file_id, version, content_hash, content FROM context_snapshot_files WHERE snapshot_id = '"+snapID.String()+"' ORDER BY type_key")
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	for prs.Next() {
		var p pinRow
		if err := prs.Scan(&p.key, &p.fid, &p.ver, &p.hash, &p.cont); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		pins = append(pins, p)
	}
	prs.Close()
	if err := prs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(pins) == 0 {
		t.Fatalf("snapshot %s has no file pins", snapID)
	}
	for _, p := range pins {
		var owner *uuid.UUID
		if err := conn.QueryRow(ctx, "SELECT project_id FROM state_files WHERE id = $1", p.fid).Scan(&owner); err != nil {
			t.Fatalf("snapshot %s pin %s references a missing file %s", snapID, p.key, p.fid)
		}
		var vhash, vcontent string
		if err := conn.QueryRow(ctx,
			"SELECT content_hash, content FROM state_file_versions WHERE state_file_id = $1 AND version = $2",
			p.fid, p.ver).Scan(&vhash, &vcontent); err != nil {
			t.Fatalf("snapshot %s pin %s v%d: version row missing (%v) — mixed context", snapID, p.key, p.ver, err)
		}
		if vhash != p.hash || vcontent != p.cont {
			t.Fatalf("snapshot %s pin %s v%d: recorded hash/content does not match the version row", snapID, p.key, p.ver)
		}
	}
}

// WBS 7.47 — Snapshot consistency under concurrent updates (the stress
// half of the Phase 7 close-out): repeatedly run snapshot creation
// CONCURRENTLY with state changes (ARCHITECTURE updates, ACTIVE_TASK
// re-derivations, runtime transitions), then validate EVERY persisted
// snapshot against its recorded logical context.
func TestP726ConcurrentSnapshotConsistency(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unit := p705Rig(t, h)
	plan := p710PlanOfPhase(t, h, projectID)
	unit = p710UnitWithSpec(t, h, plan, 1, "p726-unit", "EXECUTING",
		[]string{"criteria"}, []string{"T7.47"}, []string{"constraints"})

	machine := p617SeedMachine(t, h, projectID.String(), "p726-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}, {Key: "VALIDATE", Kind: "ACTION"}},
		[]service.MachineTransitionDef{
			{From: "IMPLEMENT", Outcome: "CONTINUE_CURRENT_SESSION", To: "VALIDATE"},
			{From: "VALIDATE", Outcome: "RESET_SESSION", To: "IMPLEMENT"},
		},
		"IMPLEMENT")
	implID := p620StateIDs(t, h, machine, "IMPLEMENT")
	validID := p620StateIDs(t, h, machine, "VALIDATE")

	roleID := p712ProjectRole(t, h, projectID, "P726-ROLE")
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})
	// Both states carry the same requirements, so every recorded
	// position resolves the same four files (fixture policy above).
	p712RequireStateFiles(t, h, uuid.MustParse(implID), []string{"PHASE_STATE", "ACTIVE_TASK"})
	p712RequireStateFiles(t, h, uuid.MustParse(validID), []string{"PHASE_STATE", "ACTIVE_TASK"})
	raw := h.App.DB().Raw()
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	now := time.Now().UTC()
	for _, sid := range []string{implID, validID} {
		if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
			ID: uuid.New(), StateID: uuid.MustParse(sid), RoleID: roleID, Ordinal: 0,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}

	svc8 := p708Service(t, h)
	ps := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "PROJECT_STATE"))
	if _, err := svc8.UpdateContent(ctx, ps.String(), "p726: project state", 0); err != nil {
		t.Fatalf("write project state: %v", err)
	}
	arch := p713ProjectFile(t, h, projectID, p709DefaultTypeID(t, h, "ARCHITECTURE"))
	if _, err := svc8.UpdateContent(ctx, arch.String(), "p726: architecture v1", 0); err != nil {
		t.Fatalf("write architecture: %v", err)
	}
	ph := p713PhaseFile(t, h, projectID, p718PhaseID(t, h, projectID), p709DefaultTypeID(t, h, "PHASE_STATE"))
	if _, err := svc8.UpdateContent(ctx, ph.String(), "p726: phase state", 0); err != nil {
		t.Fatalf("write phase state: %v", err)
	}
	if _, err := p710Service(t, h).DeriveActiveTask(ctx, projectID.String()); err != nil {
		t.Fatalf("derive active task: %v", err)
	}

	if _, err := p622Service(h, true).Initialize(ctx, projectID.String(), machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+p718PhaseID(t, h, projectID).String()+"', work_unit_id = '"+unit.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime: %v", err)
	}

	svc := p718Svc(t, h)
	transSvc := p621Service(h, true)
	atSvc := p710Service(t, h)
	base := p718RuntimeVersion(t, h, projectID)

	var wg sync.WaitGroup
	for round := 0; round < 8; round++ {
		wg.Add(4)
		// Snapshot creation.
		go func(r int) {
			defer wg.Done()
			if _, err := svc.CreateExecutionSnapshot(ctx, projectID.String()); err != nil {
				t.Errorf("round %d snapshot: %v", r, err)
			}
		}(round)
		// ARCHITECTURE content update (re-reads the current version,
		// retries on conflict).
		go func(r int) {
			defer wg.Done()
			for attempt := 0; attempt < 50; attempt++ {
				cur, err := svc8.GetCurrentVersion(ctx, arch.String())
				if err != nil {
					t.Errorf("round %d arch current: %v", r, err)
					return
				}
				if _, err := svc8.UpdateContent(ctx, arch.String(), fmt.Sprintf("p726: architecture r%d", r), cur.Version); err == nil {
					return
				}
			}
			t.Errorf("round %d architecture update never landed", r)
		}(round)
		// ACTIVE_TASK re-derivation (the op carries its own retry).
		go func(r int) {
			defer wg.Done()
			if _, err := atSvc.DeriveActiveTask(ctx, projectID.String()); err != nil {
				t.Errorf("round %d derive: %v", r, err)
			}
		}(round)
		// Runtime transition (the P6-21 apply op; the machine's two
		// transitions round-trip IMPLEMENT/VALIDATE so every round has a
		// legal destination for the runtime's current state).
		go func(r int) {
			defer wg.Done()
			for attempt := 0; attempt < 50; attempt++ {
				var fromKey string
				if err := h.DBScan(ctx, "SELECT ws.key FROM runtime_state rs JOIN workflow_states ws ON ws.id = rs.current_state_id WHERE rs.project_id = '"+projectID.String()+"'", &fromKey); err != nil {
					return
				}
				from, to, outcome := implID, validID, "CONTINUE_CURRENT_SESSION"
				if fromKey == "VALIDATE" {
					from, to, outcome = validID, implID, "RESET_SESSION"
				}
				ver := p718RuntimeVersion(t, h, projectID)
				if _, err := transSvc.ApplyTransition(ctx, projectID.String(), &service.ApplyTransitionInput{
					FromStateID: from, ToStateID: to, Outcome: outcome, ExpectedVersion: ver,
				}); err == nil {
					return
				}
			}
			t.Errorf("round %d transition never landed", r)
		}(round)
		wg.Wait()
	}

	// EVERY persisted snapshot is a valid single logical context.
	var ids []uuid.UUID
	rows, err := raw.Query(ctx, "SELECT id FROM context_snapshots WHERE project_id = $1 ORDER BY created_at, id", projectID)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan snapshot id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("persisted snapshots = %d, want >= 2 (the race must have produced snapshots)", len(ids))
	}
	for _, id := range ids {
		p726ValidateSnapshot(t, h, projectID, id)
	}
	// Non-vacuity: the state genuinely moved under the snapshots.
	if got := p718RuntimeVersion(t, h, projectID); got <= base {
		t.Fatalf("runtime version = %d, want > %d (the transitions must have landed)", got, base)
	}
	if got := p726FileVersion(t, h, arch); got < 2 {
		t.Fatalf("architecture at v%d, want >= 2 (the updates must have landed)", got)
	}
}

// WBS 7.48 — Phase 7 end-to-end acceptance: the exact 11-step scenario
// (T7.48.1-T7.48.11).
func TestP726Phase7EndToEnd(t *testing.T) {
	base := baseDSN()
	ctx0, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx0, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx0, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx0)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx0)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName)
	ctx := context.Background()

	// Create: Project A / Phase 7 / units 7.1 + 7.2.
	projectID, _, _ := p705Rig(t, h1)
	if _, err := h1.DBExec(ctx, "UPDATE phases SET name = 'Phase 7' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("rename phase: %v", err)
	}
	plan := p710PlanOfPhase(t, h1, projectID)
	unit1 := p710UnitWithSpec(t, h1, plan, 1, "7.1", "EXECUTING",
		[]string{"7.1 criteria"}, []string{"T7.48"}, []string{"7.1 constraints"})
	unit2 := p710UnitWithSpec(t, h1, plan, 2, "7.2", "PENDING",
		[]string{"7.2 criteria"}, []string{"T7.48"}, []string{"7.2 constraints"})

	// Configure: the ENGINEER role (PROJECT_STATE + ARCHITECTURE) and the
	// IMPLEMENT state (PHASE_STATE + ACTIVE_TASK); the role is assigned
	// to the state at ordinal 0.
	machine := p617SeedMachine(t, h1, projectID.String(), "p726-e2e-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}, {Key: "VALIDATE", Kind: "ACTION"}},
		[]service.MachineTransitionDef{{From: "IMPLEMENT", Outcome: "CONTINUE_CURRENT_SESSION", To: "VALIDATE"}},
		"IMPLEMENT")
	implID := p620StateIDs(t, h1, machine, "IMPLEMENT")
	roleID := p712ProjectRole(t, h1, projectID, "ENGINEER")
	p712RequireRoleFiles(t, h1, roleID, []string{"PROJECT_STATE", "ARCHITECTURE"})
	p712RequireStateFiles(t, h1, uuid.MustParse(implID), []string{"PHASE_STATE", "ACTIVE_TASK"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(h1.App.DB().Raw())
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(implID), RoleID: roleID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Persist: PROJECT_STATE v5, ARCHITECTURE v3, PHASE_STATE v2,
	// ACTIVE_TASK v1 (derived for Unit 7.1) — the exact pinned versions.
	svc8 := p708Service(t, h1)
	writeN := func(fileID uuid.UUID, label string, want int) {
		t.Helper()
		for i := 1; i <= want; i++ {
			cur, err := svc8.GetCurrentVersion(ctx, fileID.String())
			if err != nil {
				t.Fatalf("current %s: %v", label, err)
			}
			expected := 0
			if cur != nil {
				expected = cur.Version
			}
			if _, err := svc8.UpdateContent(ctx, fileID.String(), fmt.Sprintf("%s v%d", label, i), expected); err != nil {
				t.Fatalf("write %s v%d: %v", label, i, err)
			}
		}
		if got := p726FileVersion(t, h1, fileID); got != want {
			t.Fatalf("%s at v%d, want v%d (pinned)", label, got, want)
		}
	}
	ps := p713ProjectFile(t, h1, projectID, p709DefaultTypeID(t, h1, "PROJECT_STATE"))
	arch := p713ProjectFile(t, h1, projectID, p709DefaultTypeID(t, h1, "ARCHITECTURE"))
	ph := p713PhaseFile(t, h1, projectID, p718PhaseID(t, h1, projectID), p709DefaultTypeID(t, h1, "PHASE_STATE"))
	writeN(ps, "PROJECT_STATE", 5)
	writeN(arch, "ARCHITECTURE", 3)
	writeN(ph, "PHASE_STATE", 2)

	if _, err := p622Service(h1, true).Initialize(ctx, projectID.String(), machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h1.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+p718PhaseID(t, h1, projectID).String()+"', work_unit_id = '"+unit1.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime: %v", err)
	}
	d1, err := p710Service(t, h1).DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive ACTIVE_TASK: %v", err)
	}
	if d1.Version != 1 {
		t.Fatalf("ACTIVE_TASK v%d, want v1 (T7.48 pin)", d1.Version)
	}
	atFile1 := p726ActiveTaskFileIDFor(t, h1, unit1)

	// Step: create Snapshot A (the WBS 7.48 expected context: project,
	// phase, Unit 7.1, IMPLEMENT, ENGINEER, and the four pinned files at
	// the pinned versions).
	snapA, err := p718Svc(t, h1).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	if *snapA.PhaseID != p718PhaseID(t, h1, projectID) || *snapA.WorkUnitID != unit1 {
		t.Fatalf("snapshot A position = (%v %v), want (phase, unit 7.1)", snapA.PhaseID, snapA.WorkUnitID)
	}
	if snapA.StateKey != "IMPLEMENT" || snapA.RoleSnapshot.RoleID != roleID || snapA.RoleSnapshot.RoleKey != "ENGINEER" {
		t.Fatalf("snapshot A state/role = %s/%s, want IMPLEMENT/ENGINEER", snapA.StateKey, snapA.RoleSnapshot.RoleKey)
	}
	pinsA := p724SnapFiles(t, h1, snapA.ID)
	if p := pinsA["PROJECT_STATE"]; p.version != 5 {
		t.Fatalf("snapshot A PROJECT_STATE v%d, want v5", p.version)
	}
	if p := pinsA["ARCHITECTURE"]; p.version != 3 {
		t.Fatalf("snapshot A ARCHITECTURE v%d, want v3", p.version)
	}
	if p := pinsA["PHASE_STATE"]; p.version != 2 {
		t.Fatalf("snapshot A PHASE_STATE v%d, want v2", p.version)
	}
	if p := pinsA["ACTIVE_TASK"]; p.version != 1 || p.fileID != atFile1 {
		t.Fatalf("snapshot A ACTIVE_TASK pin = %+v, want unit 7.1's file at v1", p)
	}
	if !strings.Contains(pinsA["ACTIVE_TASK"].content, unit1.String()) {
		t.Fatalf("snapshot A ACTIVE_TASK does not reference unit 7.1")
	}

	// Materialize the files.
	root := t.TempDir()
	if _, err := p714Svc(t, h1).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// 1. Externally modify ACTIVE_TASK.md.
	atPath := filepath.Join(root, ".overseer", "state", "ACTIVE_TASK.md")
	if err := os.WriteFile(atPath, []byte("externally edited ACTIVE_TASK"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	// 2. Detect drift.
	res, err := p715Svc(t, h1).DetectDrift(ctx, root, projectID.String(), atFile1.String())
	if err != nil || res.Status != service.DriftDrifted {
		t.Fatalf("drift = %v/%v, want DRIFTED", res, err)
	}
	// 3. The database state remains authoritative: the persisted current
	// version is untouched by the external edit.
	cur, err := svc8.GetCurrentVersion(ctx, atFile1.String())
	if err != nil {
		t.Fatalf("current active task: %v", err)
	}
	if cur.Version != 1 || !strings.Contains(cur.Content, unit1.String()) {
		t.Fatalf("DB active task = v%d %q — the external edit leaked into the DB", cur.Version, cur.Content)
	}
	// 4. Restore the derived file: the materialization write of the
	// persisted CURRENT content to the managed path (the P7-16
	// fixture policy — the Materialize op selects PROJECT-scoped files
	// only, so the WORK_UNIT-scoped file is restored by mirroring the
	// derivation's own write). Drift returns to SYNCHRONIZED.
	cur1, err := svc8.GetCurrentVersion(ctx, atFile1.String())
	if err != nil {
		t.Fatalf("current active task 7.1: %v", err)
	}
	if err := os.WriteFile(atPath, []byte(cur1.Content), 0o644); err != nil {
		t.Fatalf("restore write: %v", err)
	}
	res, err = p715Svc(t, h1).DetectDrift(ctx, root, projectID.String(), atFile1.String())
	if err != nil || res.Status != service.DriftSynchronized {
		t.Fatalf("post-restore drift = %v/%v, want SYNCHRONIZED", res, err)
	}

	// 5. Complete Unit 7.1. 6. Select Unit 7.2.
	if _, err := h1.DBExec(ctx, "UPDATE work_units SET status='DONE', completed_at=now() WHERE id='"+unit1.String()+"'"); err != nil {
		t.Fatalf("complete 7.1: %v", err)
	}
	if _, err := h1.DBExec(ctx, "UPDATE work_units SET status='EXECUTING' WHERE id='"+unit2.String()+"'"); err != nil {
		t.Fatalf("select 7.2: %v", err)
	}
	// 7. Generate ACTIVE_TASK v2 — the derivation binds unit 7.2's NEW
	// file (the per-unit-file pattern; unit 7.1's file and its history
	// stay intact).
	d2, err := p710Service(t, h1).DeriveActiveTask(ctx, projectID.String())
	if err != nil {
		t.Fatalf("derive v2: %v", err)
	}
	if d2.Version != 1 {
		t.Fatalf("unit 7.2's ACTIVE_TASK v%d, want the fresh file's v1", d2.Version)
	}
	atFile2 := p726ActiveTaskFileIDFor(t, h1, unit2)
	if uuid.MustParse(d2.StateFileID) != atFile2 {
		t.Fatalf("derivation wrote %s, want unit 7.2's file %s", d2.StateFileID, atFile2)
	}
	if got := p726FileVersion(t, h1, atFile1); got != 1 {
		t.Fatalf("unit 7.1's ACTIVE_TASK at v%d, want v1 (the restore wrote the disk, not the DB)", got)
	}
	// 8. Create Snapshot B.
	snapB, err := p718Svc(t, h1).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	pinsB := p724SnapFiles(t, h1, snapB.ID)
	if p := pinsB["ACTIVE_TASK"]; p.fileID != atFile2 || !strings.Contains(p.content, unit2.String()) || strings.Contains(p.content, unit1.String()) {
		t.Fatalf("snapshot B ACTIVE_TASK pin = %+v, want unit 7.2's file referencing 7.2", p)
	}
	// 9. Snapshot A still reconstructs Unit 7.1: its pins are unchanged
	// (re-read through the P7-23 read API + direct SQL).
	recA, err := p723Svc(t, h1).GetSnapshot(ctx, projectID.String(), snapA.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot A: %v", err)
	}
	if recA.WorkUnitID == nil || *recA.WorkUnitID != unit1 {
		t.Fatalf("snapshot A now records unit %v, want 7.1", recA.WorkUnitID)
	}
	pinsA2 := p724SnapFiles(t, h1, snapA.ID)
	if pinsA2["ACTIVE_TASK"] != pinsA["ACTIVE_TASK"] {
		t.Fatalf("snapshot A ACTIVE_TASK pin moved: %+v -> %+v", pinsA["ACTIVE_TASK"], pinsA2["ACTIVE_TASK"])
	}
	for key, pin := range pinsA {
		if pinsA2[key] != pin {
			t.Fatalf("snapshot A pin %s moved: %+v -> %+v", key, pin, pinsA2[key])
		}
	}

	// 10. Restart the service: h1 goes away, h2 boots on the SAME
	// persisted database.
	sigA := p726Sig(t, h1, snapA.ID)
	sigB := p726Sig(t, h1, snapB.ID)
	domA := p725Reconstruct(t, h1, recA, snapA.ID)
	hashA := domA.SnapshotHash()
	recB, err := p723Svc(t, h1).GetSnapshot(ctx, projectID.String(), snapB.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot B: %v", err)
	}
	domB := p725Reconstruct(t, h1, recB, snapB.ID)
	hashB := domB.SnapshotHash()
	roleJSONA, roleJSONB := p726SnapRoleJSON(t, h1, snapA.ID), p726SnapRoleJSON(t, h1, snapB.ID)

	h1.Close()
	h2 := NewHarnessOn(t, base, dbName)
	defer h2.Close()

	// 11. Snapshots A and B remain identical to their pre-restart
	// representations (persisted rows byte-identical, the derived
	// aggregate hash recomputes identically).
	if got := p726Sig(t, h2, snapA.ID); got != sigA {
		t.Fatalf("snapshot A differs after restart:\nbefore:\n%s\nafter:\n%s", sigA, got)
	}
	if got := p726Sig(t, h2, snapB.ID); got != sigB {
		t.Fatalf("snapshot B differs after restart:\nbefore:\n%s\nafter:\n%s", sigB, got)
	}
	if got := p726SnapRoleJSON(t, h2, snapA.ID); got != roleJSONA {
		t.Fatal("snapshot A role snapshot bytes differ after restart")
	}
	if got := p726SnapRoleJSON(t, h2, snapB.ID); got != roleJSONB {
		t.Fatal("snapshot B role snapshot bytes differ after restart")
	}
	recA2, err := p723Svc(t, h2).GetSnapshot(ctx, projectID.String(), snapA.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot A after restart: %v", err)
	}
	domA2 := p725Reconstruct(t, h2, recA2, snapA.ID)
	if domA2.SnapshotHash() != hashA {
		t.Fatal("snapshot A aggregate hash differs after restart")
	}
	recB2, err := p723Svc(t, h2).GetSnapshot(ctx, projectID.String(), snapB.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot B after restart: %v", err)
	}
	domB2 := p725Reconstruct(t, h2, recB2, snapB.ID)
	if domB2.SnapshotHash() != hashB {
		t.Fatal("snapshot B aggregate hash differs after restart")
	}
	// Post-restart sanity: both snapshots are still valid single logical
	// contexts on the new pool.
	p726ValidateSnapshot(t, h2, projectID, snapA.ID)
	p726ValidateSnapshot(t, h2, projectID, snapB.ID)
	_ = ctx0
}
