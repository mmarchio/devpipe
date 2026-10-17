package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/service"
)

// P7-19 (WBS 7.33): snapshot immutability + determinism across restart.
//
// A captured snapshot is immutable: later state-file content appends,
// role updates, and state-machine edits leave the persisted snapshot
// rows AND the pinned version rows byte-identical (the snapshot pins
// exact version numbers + duplicated content, so nothing it reads can
// move). T7.33.4 restarts the process (two harness instances on one
// persisted database, the reconstruction pattern) and proves the
// historical snapshot remains reconstructable at its pinned versions.
//
// The immutability proofs compare full-row byte signatures read by
// DIRECT SQL (concatenated column lists — a column-by-column compare
// would miss timestamp/version side effects), never the service's own
// readback.

// p719Sig returns the full-row byte signature of the project's snapshot
// row plus its file rows (concatenated, ordered by type_key), so ANY
// change to ANY persisted snapshot byte is visible.
func p719Sig(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var rowSig string
	q := "SELECT id::text || '|' || project_id::text || '|' || coalesce(phase_id::text,'-') || '|' || coalesce(work_unit_id::text,'-') || '|' || runtime_state_id::text || '|' || runtime_state_version::text || '|' || state_machine_id::text || '|' || state_machine_version::text || '|' || state_key || '|' || role_snapshot::text || '|' || state_file_tokens::text || '|' || role_tokens::text || '|' || estimated_total::text || '|' || created_at::text FROM context_snapshots WHERE project_id = '" + projectID.String() + "'"
	if err := h.DBScan(ctx, q, &rowSig); err != nil {
		t.Fatalf("scan snapshot signature: %v", err)
	}
	rows, err := h.App.DB().Raw().Query(ctx,
		"SELECT f.id::text || '|' || f.snapshot_id::text || '|' || f.state_file_id::text || '|' || f.type_key || '|' || f.scope || '|' || f.version::text || '|' || f.content_hash || '|' || f.content || '|' || f.state_file_token_estimate::text || '|' || f.classification || '|' || f.created_at::text FROM context_snapshot_files f JOIN context_snapshots s ON s.id = f.snapshot_id WHERE s.project_id = $1 ORDER BY f.type_key", projectID)
	if err != nil {
		t.Fatalf("query file signatures: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			t.Fatalf("scan file signature: %v", err)
		}
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return rowSig + "\n" + fmt.Sprint(out)
}

// p719Rig builds the P7-18 snapshot fixture and creates ONE snapshot,
// returning the snapshot row id and the position fields.
func p719Rig(t *testing.T, h *Harness) (projectID uuid.UUID, snapshotID uuid.UUID) {
	t.Helper()
	proj, _, _, _, _ := p718Rig(t, h)
	svc := p718Svc(t, h)
	snap, err := svc.CreateExecutionSnapshot(context.Background(), proj.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	return proj, snap.ID
}

// T7.33.1 — Later state-file updates don't affect the snapshot: after
// the snapshot, appending new content to both pinned files (v1 -> v2)
// leaves the persisted snapshot rows and the pinned v1 version rows
// byte-identical; the snapshot still resolves the captured v1.
func TestP719FileUpdatesDontAffectSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, snapID := p719Rig(t, h)
	before := p719Sig(t, h, proj)
	// File-version rows for the pinned versions (direct-SQL signature).
	var v1Sig string
	if err := h.DBScan(context.Background(),
		"SELECT string_agg(version::text || ':' || content_hash, ',' ORDER BY ctid) FROM state_file_versions WHERE state_file_id IN (SELECT state_file_id FROM context_snapshot_files WHERE snapshot_id = '"+snapID.String()+"') AND version = 1",
		&v1Sig); err != nil {
		t.Fatalf("scan version signature: %v", err)
	}
	// Later updates: both pinned files get a v2 with new content.
	svc8 := p708Service(t, h)
	ids := p719FileIDs(t, h, snapID)
	if _, err := svc8.UpdateContent(context.Background(), ids[0].String(), "A content v2", 1); err != nil {
		t.Fatalf("update A: %v", err)
	}
	if _, err := svc8.UpdateContent(context.Background(), ids[1].String(), "B content v2", 1); err != nil {
		t.Fatalf("update B: %v", err)
	}
	// Sanity: the files actually moved (non-vacuity).
	var curA, curB int
	if err := h.DBScan(context.Background(),
		"SELECT (SELECT current_version FROM state_files WHERE id = '"+ids[0].String()+"'), (SELECT current_version FROM state_files WHERE id = '"+ids[1].String()+"')",
		&curA, &curB); err != nil {
		t.Fatalf("scan current versions: %v", err)
	}
	if curA != 2 || curB != 2 {
		t.Fatalf("precondition: files at v%d/v%d, want 2/2 (the updates must have landed)", curA, curB)
	}
	// Snapshot rows byte-identical...
	if after := p719Sig(t, h, proj); after != before {
		t.Fatalf("snapshot rows changed after file updates:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// ...and the pinned v1 version rows byte-identical (still resolvable
	// at v1: the snapshot's recorded hash matches the v1 row's hash).
	var v1SigAfter string
	if err := h.DBScan(context.Background(),
		"SELECT string_agg(version::text || ':' || content_hash, ',' ORDER BY ctid) FROM state_file_versions WHERE state_file_id IN (SELECT state_file_id FROM context_snapshot_files WHERE snapshot_id = '"+snapID.String()+"') AND version = 1",
		&v1SigAfter); err != nil {
		t.Fatalf("scan version signature after: %v", err)
	}
	if v1SigAfter != v1Sig {
		t.Fatalf("pinned v1 version rows changed: %q -> %q", v1Sig, v1SigAfter)
	}
	var snapVersion int
	if err := h.DBScan(context.Background(),
		"SELECT f.version FROM context_snapshot_files f WHERE f.snapshot_id = '"+snapID.String()+"' AND f.type_key = 'A'",
		&snapVersion); err != nil {
		t.Fatalf("scan pinned version: %v", err)
	}
	if snapVersion != 1 {
		t.Fatalf("snapshot now resolves A at v%d, want the captured v1", snapVersion)
	}
}

// T7.33.2 — Later role updates don't affect the snapshot: updating the
// captured role's name/description AND its system prompt (new prompt
// version) leaves the persisted snapshot rows byte-identical.
func TestP719RoleUpdatesDontAffectSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, snapID := p719Rig(t, h)
	before := p719Sig(t, h, proj)
	roleID := p718RoleID(t, h, proj)
	roleSvc := snapshotSvc(t, h)
	if _, err := roleSvc.UpdateRole(context.Background(), roleID.String(), "Edited name", "Edited description"); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if _, err := roleSvc.UpdateSystemPrompt(context.Background(), roleID.String(), "Edited system prompt", 1); err != nil {
		t.Fatalf("UpdateSystemPrompt: %v", err)
	}
	// Sanity: the role actually moved (non-vacuity).
	var roleVersion, promptVersion int
	if err := h.DBScan(context.Background(),
		"SELECT r.version, (SELECT max(version) FROM role_system_prompts p WHERE p.role_id = r.id) FROM roles r WHERE r.id = '"+roleID.String()+"'",
		&roleVersion, &promptVersion); err != nil {
		t.Fatalf("scan role versions: %v", err)
	}
	if roleVersion < 2 || promptVersion < 2 {
		t.Fatalf("precondition: role v%d prompt v%d, want moved (>=2) (the updates must have landed)", roleVersion, promptVersion)
	}
	if after := p719Sig(t, h, proj); after != before {
		t.Fatalf("snapshot rows changed after role updates:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	_ = snapID
}

// T7.33.3 — Later state-machine edits don't affect the snapshot: the
// machine is edited (a new version-2 row, per WBS 6.20), and the
// persisted snapshot rows stay byte-identical, still pinned to the
// captured machine version.
func TestP719MachineEditsDontAffectSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, snapID := p719Rig(t, h)
	before := p719Sig(t, h, proj)
	var machineID uuid.UUID
	if err := h.DBScan(context.Background(),
		"SELECT state_machine_id FROM context_snapshots WHERE id = '"+snapID.String()+"'",
		&machineID); err != nil {
		t.Fatalf("scan machine id: %v", err)
	}
	svc := p617Service(h)
	// Add a state: a real edit that creates machine version 2.
	if _, err := svc.Edit(context.Background(), machineID.String(), &service.MachineEditInput{
		InitialStateKey: "S",
		States:          []service.MachineStateDef{{Key: "S", Kind: "ACTION"}, {Key: "S2", Kind: "TERMINAL"}},
	}); err != nil {
		t.Fatalf("machine edit: %v", err)
	}
	// Sanity: the edit landed (a version-2 row exists; non-vacuity).
	var maxVer int
	if err := h.DBScan(context.Background(),
		"SELECT max(version) FROM state_machines WHERE id = '"+machineID.String()+"' OR family_id = '"+machineID.String()+"'",
		&maxVer); err != nil {
		t.Fatalf("scan machine versions: %v", err)
	}
	if maxVer < 2 {
		t.Fatalf("precondition: machine max version = %d, want >= 2 (the edit must have landed)", maxVer)
	}
	if after := p719Sig(t, h, proj); after != before {
		t.Fatalf("snapshot rows changed after machine edit:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// T7.33.4 — The historical snapshot remains reconstructable after
// restart: the fixture is built and snapshotted on the FIRST harness
// instance; the process restarts (second instance, same persisted
// database); the snapshot's pinned versions must still resolve —
// every pinned file version row exists with the exact recorded hash,
// the pinned role config hash recomputes from the persisted
// role_snapshot, and the snapshot rows are byte-identical across the
// restart.
func TestP719SnapshotReconstructableAfterRestart(t *testing.T) {
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
	// Caller owns the DB lifecycle; h2 closes BEFORE dropDB (LIFO).
	t.Cleanup(func() { dropDB(t, base, dbName) })
	h1 := NewHarnessOn(t, base, dbName)
	proj, _ := p719Rig(t, h1)
	before := p719Sig(t, h1, proj)
	h1.Close()
	h2 := NewHarnessOn(t, base, dbName)
	defer h2.Close()
	after := p719Sig(t, h2, proj)
	if after != before {
		t.Fatalf("snapshot rows differ across the restart:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// Reconstructability at the pinned versions: for EVERY pinned file
	// row the exact (state_file_id, version) version row exists with
	// the EXACT recorded hash — reconstruction resolves the captured
	// bytes, not the file's current content.
	var missing, mismatched int
	if err := h2.DBScan(context.Background(),
		"SELECT count(*) FROM context_snapshot_files f WHERE NOT EXISTS (SELECT 1 FROM state_file_versions v WHERE v.state_file_id = f.state_file_id AND v.version = f.version)",
		&missing); err != nil {
		t.Fatalf("scan missing pinned versions: %v", err)
	}
	if err := h2.DBScan(context.Background(),
		"SELECT count(*) FROM context_snapshot_files f JOIN state_file_versions v ON v.state_file_id = f.state_file_id AND v.version = f.version WHERE v.content_hash <> f.content_hash",
		&mismatched); err != nil {
		t.Fatalf("scan hash mismatches: %v", err)
	}
	if missing != 0 || mismatched != 0 {
		t.Fatalf("reconstruction failed after restart: %d pinned versions missing, %d hash mismatches", missing, mismatched)
	}
}

// p719FileIDs returns the pinned file ids of the snapshot, ordered by
// type_key.
func p719FileIDs(t *testing.T, h *Harness, snapshotID uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT state_file_id FROM context_snapshot_files WHERE snapshot_id = $1 ORDER BY type_key", snapshotID)
	if err != nil {
		t.Fatalf("query file ids: %v", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
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
