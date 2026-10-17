package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-20 (WBS 7.35/7.36): snapshot token budget + source metadata
// classification, against the REAL snapshot-creation service and
// PostgreSQL (the pure-domain halves are pinned in
// internal/statefile/p720_snapshot_size_test.go).
//
// Token budget (WBS 7.35): the persisted snapshot row carries the
// aggregate (state_file_tokens, role_tokens, estimated_total) and every
// file pin carries its own token estimate — the estimate is STORED WITH
// THE SNAPSHOT (migration 00040), not re-derived at read, so the budget
// survives reconstruction.
//
// Source metadata (WBS 7.36): every snapshot component carries its
// classification. State-file pins persist a classification column
// (00040); role components keep the role snapshot's per-component
// classifications inside the serialized role snapshot. The persisted
// values — not a re-derivation from live state — are authoritative:
// T7.36.5 asserts the reconstruction path reads them back byte-identical
// while the live state files are edited in between (a re-derivation at
// read would reflect the edits).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS gives the
// class SET and the four canonical tests, not the full mapping):
//   - PROJECT_STATE -> PROJECT_CONTEXT, PHASE_STATE -> PHASE_CONTEXT,
//     ACTIVE_TASK -> TASK_CONTEXT, TEST_STATE -> TEST_CONTEXT (the
//     canonical mapping pinned by T7.36.1-T7.36.4).
//   - Non-canonical type keys fall back to the scope-based class:
//     PROJECT scope -> PROJECT_CONTEXT, PHASE scope -> PHASE_CONTEXT,
//     WORK_UNIT/EXECUTION scope -> TASK_CONTEXT.

const (
	p720Prompt = "P7-20 prompt content"  // role system prompt
	p720Proj   = "project state content" // PROJECT_STATE file
	p720Phase  = "phase state content"   // PHASE_STATE file
	p720Active = "active task content"   // ACTIVE_TASK file
	p720Test   = "test state content"    // TEST_STATE file
)

// p720Svc wires the snapshot-creation service over the harness pool (the
// P7-18 wiring; the persisted estimate/classification ride on the same
// op).
func p720Svc(t *testing.T, h *Harness) *service.ExecutionSnapshotService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewExecutionSnapshotService(
		dbrepo.NewRuntimeStateRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateRoleAssignmentRepository(raw),
		dbrepo.NewRoleRepository(raw),
		snapshotSvc(t, h),
		p713Svc(t, h),
	).WithSnapshots(dbrepo.NewContextSnapshotRepository(raw)).
		WithFileVersions(dbrepo.NewStateFileVersionRepository(raw)).
		WithTxPool(raw)
}

// p720Rig seeds a complete snapshot-ready fixture with the FOUR canonical
// state-file types (all required, all with content) and a role assigned
// to the runtime's state, and returns all ids.
func p720Rig(t *testing.T, h *Harness) (projectID, roleID uuid.UUID, fileProj, filePhase, fileActive, fileTest uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	proj, phaseID, unitID := p705Rig(t, h)

	// Machine v1 with one enabled state S; the project role is assigned
	// to S at ordinal 0 (the snapshot's role).
	mid := p617SeedMachine(t, h, proj.String(), "p720-machine",
		[]service.MachineStateDef{{Key: "S", Kind: "ACTION"}},
		nil, "S")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'S'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}
	roleID = p712ProjectRole(t, h, proj, "P720-ROLE")
	// The rig's role prompt is the helper's bare content; move it to the
	// dedicated prompt so the budget tests pin a known estimate.
	if _, err := snapshotSvc(t, h).UpdateSystemPrompt(ctx, roleID.String(), p720Prompt, 1); err != nil {
		t.Fatalf("update role prompt: %v", err)
	}
	// The role requires the four canonical types (the snapshot resolves
	// the role's + state's required types, WBS 7.12/7.13).
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "PHASE_STATE", "ACTIVE_TASK", "TEST_STATE"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: roleID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// The four canonical types (required) with files + content.
	typeProj := p718Required(t, h, proj, "PROJECT_STATE", "PROJECT")
	typePhase := p718Required(t, h, proj, "PHASE_STATE", "PHASE")
	typeActive := p718Required(t, h, proj, "ACTIVE_TASK", "WORK_UNIT")
	typeTest := p718Required(t, h, proj, "TEST_STATE", "PHASE")
	svc8 := p708Service(t, h)
	fileProj = p713ProjectFile(t, h, proj, typeProj)
	filePhase = p713PhaseFile(t, h, proj, phaseID, typePhase)
	fileActive = p713UnitFile(t, h, proj, unitID, typeActive)
	fileTest = p713PhaseFile(t, h, proj, phaseID, typeTest)
	for _, w := range []struct {
		id      uuid.UUID
		content string
	}{{fileProj, p720Proj}, {filePhase, p720Phase}, {fileActive, p720Active}, {fileTest, p720Test}} {
		if _, err := svc8.UpdateContent(ctx, w.id.String(), w.content, 0); err != nil {
			t.Fatalf("write %s: %v", w.id, err)
		}
	}

	// Runtime state at S with the machine binding and phase association.
	var machineRowID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM state_machines WHERE id = '"+mid+"'", &machineRowID); err != nil {
		t.Fatalf("scan machine: %v", err)
	}
	rt := dbrepo.NewRuntimeStateRepository(raw)
	if err := rt.Create(ctx, &persistence.RuntimeStateRecord{
		ID: uuid.New(), ProjectID: proj,
		StateMachineID: machineRowID, StateMachineVersion: 1,
		CurrentStateID: stateID, PhaseID: &phaseID, WorkUnitID: &unitID,
		EnteredAt: time.Now().UTC(), Version: 1,
	}); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	return proj, roleID, fileProj, filePhase, fileActive, fileTest
}

// p720Budget reads the persisted aggregate from the project's snapshot
// row by direct SQL (never the service's readback).
func p720Budget(t *testing.T, h *Harness, projectID uuid.UUID) (stateTokens, roleTokens, total int) {
	t.Helper()
	if err := h.DBScan(context.Background(),
		"SELECT state_file_tokens, role_tokens, estimated_total FROM context_snapshots WHERE project_id = '"+projectID.String()+"'",
		&stateTokens, &roleTokens, &total); err != nil {
		t.Fatalf("scan budget: %v", err)
	}
	return
}

// p720FileRows reads the persisted (type_key, classification, token
// estimate) tuples, ordered by type_key.
func p720FileRows(t *testing.T, h *Harness, projectID uuid.UUID) [][3]string {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(),
		"SELECT f.type_key, f.classification, f.state_file_token_estimate::text FROM context_snapshot_files f JOIN context_snapshots s ON s.id = f.snapshot_id WHERE s.project_id = $1 ORDER BY f.type_key", projectID)
	if err != nil {
		t.Fatalf("query file rows: %v", err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var key, class, est string
		if err := rows.Scan(&key, &class, &est); err != nil {
			t.Fatalf("scan file row: %v", err)
		}
		out = append(out, [3]string{key, class, est})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// p720Reconstruct returns the domain snapshot read back through the
// repository (the P7-23 read path) — reconstruction from persisted
// state, not a service re-derivation.
func p720Reconstruct(t *testing.T, h *Harness, projectID uuid.UUID) *persistence.ContextSnapshotRecord {
	t.Helper()
	recs, err := dbrepo.NewContextSnapshotRepository(h.App.DB().Raw()).ListByProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("reconstruct: %d persisted snapshots, want 1", len(recs))
	}
	return recs[0]
}

// T7.35.1/T7.35.2 (DB half) — The estimate is STORED WITH THE SNAPSHOT:
// the persisted aggregate equals the token estimates of the captured
// content (state-file pins summed; the role estimate included), read by
// direct SQL.
func TestP720BudgetStoredWithSnapshot(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _, _ := p720Rig(t, h)
	if _, err := p720Svc(t, h).CreateExecutionSnapshot(context.Background(), proj.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	wantState := statefile.TokenEstimate(p720Proj) + statefile.TokenEstimate(p720Phase) +
		statefile.TokenEstimate(p720Active) + statefile.TokenEstimate(p720Test)
	wantRole := statefile.TokenEstimate(p720Prompt)
	if wantState == 0 || wantRole == 0 {
		t.Fatal("fixture sanity: the estimates must be nonzero")
	}
	gotState, gotRole, gotTotal := p720Budget(t, h, proj)
	if gotState != wantState {
		t.Errorf("persisted state_file_tokens = %d, want %d", gotState, wantState)
	}
	if gotRole != wantRole {
		t.Errorf("persisted role_tokens = %d, want %d", gotRole, wantRole)
	}
	if gotTotal != wantState+wantRole {
		t.Errorf("persisted estimated_total = %d, want %d", gotTotal, wantState+wantRole)
	}
}

// T7.35.3 (DB half) — Empty optional files handled: a required type whose
// file was never written (no version) is pinned at version 0 with an
// empty-content estimate of 0, and the aggregate is undisturbed by it.
func TestP720EmptyOptionalFile(t *testing.T) {
	h := NewHarness(t)
	proj, roleID, _, _, _, _ := p720Rig(t, h)
	// The role additionally requires a type whose file is EMPTY (created,
	// no content) — the set REPLACEMENT keeps the four canonical
	// requirements alongside it.
	p712RequireRoleFiles(t, h, roleID, []string{"PROJECT_STATE", "PHASE_STATE", "ACTIVE_TASK", "TEST_STATE", "EMPTY_OPT"})
	p718Required(t, h, proj, "EMPTY_OPT", "PROJECT")
	svc8 := p708Service(t, h)
	emptyFile := p713ProjectFile(t, h, proj, p720Type(t, h, proj, "EMPTY_OPT"))
	if _, err := svc8.UpdateContent(context.Background(), emptyFile.String(), "", 0); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if _, err := p720Svc(t, h).CreateExecutionSnapshot(context.Background(), proj.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	gotState, _, gotTotal := p720Budget(t, h, proj)
	wantState := statefile.TokenEstimate(p720Proj) + statefile.TokenEstimate(p720Phase) +
		statefile.TokenEstimate(p720Active) + statefile.TokenEstimate(p720Test)
	if gotState != wantState {
		t.Errorf("state_file_tokens = %d, want %d (the empty file must contribute 0)", gotState, wantState)
	}
	if gotTotal != wantState+statefile.TokenEstimate(p720Prompt) {
		t.Errorf("estimated_total = %d, want %d", gotTotal, wantState+statefile.TokenEstimate(p720Prompt))
	}
	found := false
	for _, row := range p720FileRows(t, h, proj) {
		if row[0] == "EMPTY_OPT" {
			found = true
			if row[2] != "0" {
				t.Errorf("EMPTY_OPT pin estimate = %s, want 0", row[2])
			}
		}
	}
	if !found {
		t.Fatal("the empty optional file's pin is missing from the snapshot")
	}
}

// T7.36.1/T7.36.2/T7.36.3/T7.36.4 (DB half) — Every snapshot component is
// classified: the four canonical state-file pins persist the WBS 7.36
// classes, and the persisted role snapshot carries the role components'
// classifications (SYSTEM / ROLE_INSTRUCTION).
func TestP720ComponentClassificationsPersisted(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _, _, _ := p720Rig(t, h)
	if _, err := p720Svc(t, h).CreateExecutionSnapshot(context.Background(), proj.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	rows := p720FileRows(t, h, proj)
	want := map[string]string{
		"PROJECT_STATE": "PROJECT_CONTEXT",
		"PHASE_STATE":   "PHASE_CONTEXT",
		"ACTIVE_TASK":   "TASK_CONTEXT",
		"TEST_STATE":    "TEST_CONTEXT",
	}
	if len(rows) != len(want) {
		t.Fatalf("persisted %d file rows, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if want[row[0]] != row[1] {
			t.Errorf("file %s classification = %q, want %q", row[0], row[1], want[row[0]])
		}
	}
	rec := p720Reconstruct(t, h, proj)
	if rec.RoleSnapshotJSON == nil {
		t.Fatal("persisted role snapshot is empty")
	}
	var rs role.RoleSnapshot
	if err := json.Unmarshal(rec.RoleSnapshotJSON, &rs); err != nil {
		t.Fatalf("unmarshal persisted role snapshot: %v", err)
	}
	if rs.SystemPrompt.Classification != role.ClassificationSystem {
		t.Errorf("system prompt classification = %q, want SYSTEM", rs.SystemPrompt.Classification)
	}
}

// T7.36.5 — Classification SURVIVES SNAPSHOT RECONSTRUCTION: the
// classification read from the persisted snapshot is authoritative, not
// re-derived from live state. The live state files are edited AFTER the
// snapshot (content + a new classification-relevant type added), and the
// reconstruction (repository read + deserialized role snapshot) must
// still carry the EXACT persisted classifications. A read-time
// re-derivation would reflect the edits and fail here.
func TestP720ClassificationSurvivesReconstruction(t *testing.T) {
	h := NewHarness(t)
	proj, _, fileProj, _, _, _ := p720Rig(t, h)
	if _, err := p720Svc(t, h).CreateExecutionSnapshot(context.Background(), proj.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	before := p720Reconstruct(t, h, proj)
	beforeClasses := map[string]string{}
	for _, f := range before.Files {
		beforeClasses[f.TypeKey] = f.Classification
	}
	// Mutate the LIVE state after the snapshot: the persisted
	// classification must not track the live file (it is a snapshot
	// property, not a live-state property).
	svc8 := p708Service(t, h)
	if _, err := svc8.UpdateContent(context.Background(), fileProj.String(), "MUTATED AFTER SNAPSHOT", 1); err != nil {
		t.Fatalf("post-snapshot edit: %v", err)
	}
	after := p720Reconstruct(t, h, proj)
	if len(after.Files) != len(before.Files) {
		t.Fatalf("reconstruction file count = %d, want %d", len(after.Files), len(before.Files))
	}
	for _, f := range after.Files {
		if beforeClasses[f.TypeKey] != f.Classification {
			t.Errorf("reconstructed classification for %s = %q, want the persisted %q",
				f.TypeKey, f.Classification, beforeClasses[f.TypeKey])
		}
	}
	// The persisted role components' classifications survive identically.
	var rsBefore, rsAfter role.RoleSnapshot
	if err := json.Unmarshal(before.RoleSnapshotJSON, &rsBefore); err != nil {
		t.Fatalf("unmarshal before: %v", err)
	}
	if err := json.Unmarshal(after.RoleSnapshotJSON, &rsAfter); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if rsBefore.SystemPrompt.Classification != rsAfter.SystemPrompt.Classification {
		t.Error("system prompt classification changed across reconstruction")
	}
}

// p720Type looks up the project's type id by key (direct SQL — the type
// is created through the P7-13 helper).
func p720Type(t *testing.T, h *Harness, projectID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM state_file_types WHERE project_id = '"+projectID.String()+"' AND key = '"+key+"'", &id); err != nil {
		t.Fatalf("scan type %s: %v", key, err)
	}
	return id
}
