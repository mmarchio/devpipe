package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-23 (WBS 7.37/7.40): the context-preview API + the snapshot-read API.
//
// The context preview (WBS 7.37) reports WHAT context WOULD be captured for
// the project's current runtime position — the current state, the resolved
// role, the required state files at their CURRENT versions, the missing
// required files, and the estimated token count — WITHOUT persisting a
// snapshot and WITHOUT altering the runtime. It reuses the P7-18 creation
// pipeline (role resolution, P7-13 scope resolution, version capture); the
// distinguishing property over the creation op is that a missing required
// file is REPORTED (in MissingFiles) instead of failing.
//
// The snapshot read API (WBS 7.40) lists the project's persisted snapshots
// and retrieves one by id (404 when unknown); read-only (no mutation).
//
// The fixture reuses p718Rig (a complete snapshot-ready project: machine +
// state S, role P718-ROLE requiring A (PROJECT), state S requiring B (PHASE),
// files A/B with content, runtime at S). Service-level tests drive the op
// over the harness pool for the no-persist / no-mutation proofs; the REST
// tests drive the live HTTP surface for the 200/404/405 contract.

// p723Svc wires the context-preview + snapshot-read service over the harness
// pool (the same composition the app wires in production).
func p723Svc(t *testing.T, h *Harness) *service.ContextPreviewService {
	t.Helper()
	raw := h.App.DB().Raw()
	scopeRes := service.NewStateFileScopeResolutionService(
		p712Svc(t, h),
		dbrepo.NewStateFileTypeRepository(raw),
		dbrepo.NewStateFileRepository(raw),
		dbrepo.NewPhaseRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
	)
	return service.NewContextPreviewService(
		dbrepo.NewRuntimeStateRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateRoleAssignmentRepository(raw),
		dbrepo.NewRoleRepository(raw),
		p718RoleSvc(t, h),
		scopeRes,
	).WithFileVersions(dbrepo.NewStateFileVersionRepository(raw)).
		WithSnapshots(dbrepo.NewContextSnapshotRepository(raw)).
		WithTxPool(raw)
}

// p723PreviewKeys maps the preview's required files to a set of type keys.
func p723PreviewKeys(t *testing.T, v *service.ScopePreview) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, f := range v.Files {
		out[f.TypeKey] = 1
	}
	return out
}

// T7.37.1 — Correct requirements displayed: the preview reports exactly the
// required state-file types (A from the role, B from the state) with their
// scopes, and the resolved state + role keys.
func TestP723PreviewRequirementsDisplayed(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	svc := p723Svc(t, h)
	v, err := svc.Preview(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if v.StateKey != "S" {
		t.Fatalf("state key = %q, want S", v.StateKey)
	}
	if v.RoleKey != "P718-ROLE" {
		t.Fatalf("role key = %q, want P718-ROLE", v.RoleKey)
	}
	keys := p723PreviewKeys(t, v)
	if len(keys) != 2 || keys["A"] != 1 || keys["B"] != 1 {
		t.Fatalf("required files = %v, want exactly {A, B}", keys)
	}
}

// T7.37.2 — Versions displayed: each present required file is reported at its
// CURRENT persisted version, with the content hash and a non-zero token
// estimate (both files are at v1 with content).
func TestP723PreviewVersionsDisplayed(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, fileA, _ := p718Rig(t, h)
	svc := p723Svc(t, h)
	v, err := svc.Preview(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	var gotA, gotB *service.ScopePreviewFile
	for i := range v.Files {
		switch v.Files[i].TypeKey {
		case "A":
			gotA = &v.Files[i]
		case "B":
			gotB = &v.Files[i]
		}
	}
	if gotA == nil || gotB == nil {
		t.Fatalf("preview files missing A/B: %+v", v.Files)
	}
	// Cross-check A's version + hash against the persisted version row
	// (the version the file is actually at), not an assumed literal.
	var storedVer int
	var storedHash string
	if err := h.DBScan(context.Background(),
		"SELECT version, content_hash FROM state_file_versions WHERE state_file_id = '"+fileA.String()+"' AND version = (SELECT current_version FROM state_files WHERE id = '"+fileA.String()+"')",
		&storedVer, &storedHash); err != nil {
		t.Fatalf("scan stored version: %v", err)
	}
	if !gotA.Present || gotA.Version != storedVer || gotA.Hash != storedHash {
		t.Fatalf("file A = (present=%v, v%d, %s), want (true, %d, %s)",
			gotA.Present, gotA.Version, gotA.Hash, storedVer, storedHash)
	}
	if !gotB.Present || gotB.Version != 1 {
		t.Fatalf("file B = (present=%v, v%d), want present v1", gotB.Present, gotB.Version)
	}
	// The estimated token count is non-zero (both files + role prompt carry
	// content) — a vacuous 0 would mean the estimate never computed.
	if v.EstimatedTokens <= 0 {
		t.Fatalf("estimated tokens = %d, want > 0", v.EstimatedTokens)
	}
}

// T7.37.3 — Missing files identified: a required type with no file at its
// scope is reported in MissingFiles (present=false, version 0) and the
// preview SUCCEEDS (the distinguishing property over the creation op, which
// the P7-13 semantics make fail with ErrMissingStateFile).
func TestP723PreviewMissingFilesIdentified(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, _, _, _ := p718Rig(t, h)
	// The role additionally requires type C, which has no file.
	p712RequireRoleFiles(t, h, roleID, []string{"A", "C"})
	if p718Required(t, h, projectID, "C", "PROJECT") == uuid.Nil {
		t.Fatal("type C not created")
	}
	svc := p723Svc(t, h)
	v, err := svc.Preview(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("Preview (missing file) = %v, want success (missing is reported, not fatal)", err)
	}
	found := false
	for _, m := range v.MissingFiles {
		if m == "C" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MissingFiles = %v, want to include C", v.MissingFiles)
	}
	// The present required file (A) is still reported present.
	var aPresent bool
	for _, f := range v.Files {
		if f.TypeKey == "A" && f.Present {
			aPresent = true
		}
	}
	if !aPresent {
		t.Fatalf("file A not reported present: %+v", v.Files)
	}
}

// T7.37.4 — Preview doesn't persist an execution snapshot: after a
// successful preview the context_snapshots + context_snapshot_files tables
// hold zero rows for the project (direct SQL, not the service's readback).
func TestP723PreviewDoesNotPersistSnapshot(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	svc := p723Svc(t, h)
	for i := 0; i < 3; i++ { // repeat: no accumulation across calls
		if _, err := svc.Preview(context.Background(), projectID.String()); err != nil {
			t.Fatalf("Preview: %v", err)
		}
	}
	var snapCount, fileCount int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM context_snapshots WHERE project_id = '"+projectID.String()+"'", &snapCount); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM context_snapshot_files f JOIN context_snapshots s ON s.id = f.snapshot_id WHERE s.project_id = '"+projectID.String()+"'", &fileCount); err != nil {
		t.Fatal(err)
	}
	if snapCount != 0 || fileCount != 0 {
		t.Fatalf("preview persisted snapshots: %d snapshot rows, %d file rows (want 0/0)", snapCount, fileCount)
	}
}

// T7.37.5 — Preview doesn't alter runtime state: the runtime row's full
// position (state, version, machine version) is byte-identical after the
// preview (direct SQL signature).
func TestP723PreviewDoesNotAlterRuntime(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	sig := func() string {
		var stateID, machineID string
		var ver, mver int
		if err := h.DBScan(context.Background(),
			"SELECT current_state_id::text, state_machine_id::text, version, state_machine_version FROM runtime_state WHERE project_id = '"+projectID.String()+"'",
			&stateID, &machineID, &ver, &mver); err != nil {
			t.Fatalf("scan runtime: %v", err)
		}
		return stateID + "|" + machineID + "|" + itoaSig(ver) + "|" + itoaSig(mver)
	}
	before := sig()
	svc := p723Svc(t, h)
	if _, err := svc.Preview(context.Background(), projectID.String()); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if after := sig(); after != before {
		t.Fatalf("runtime altered by preview: before=%q after=%q", before, after)
	}
}

// T7.40.1 — Snapshot retrieved: a persisted snapshot (created via the P7-18
// creation op) is retrievable by id, returning its state key, the resolved
// role, and its pinned file versions.
func TestP723SnapshotRetrieved(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	created, err := p718Svc(t, h).CreateExecutionSnapshot(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	svc := p723Svc(t, h)
	rec, err := svc.GetSnapshot(context.Background(), projectID.String(), created.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if rec.ID != created.ID || rec.StateKey != "S" {
		t.Fatalf("retrieved snapshot id=%s state=%q, want %s S", rec.ID, rec.StateKey, created.ID)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("snapshot files = %d, want 2", len(rec.Files))
	}
}

// T7.40.2 — Historical versions represented: the retrieved snapshot carries
// each pinned file at the EXACT version + hash captured at creation time (a
// later content update to the live file does not change the pinned version).
func TestP723SnapshotHistoricalVersions(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, fileA, _ := p718Rig(t, h)
	created, err := p718Svc(t, h).CreateExecutionSnapshot(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	// Advance the live file A to v2 (the live current version moves on).
	if _, err := p708Service(t, h).UpdateContent(context.Background(), fileA.String(), "updated content v2", 1); err != nil {
		t.Fatalf("advance A: %v", err)
	}
	svc := p723Svc(t, h)
	rec, err := svc.GetSnapshot(context.Background(), projectID.String(), created.ID.String())
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	for _, f := range rec.Files {
		if f.TypeKey == "A" {
			if f.Version != 1 {
				t.Fatalf("pinned A version = %d, want the captured 1 (live file is now v2)", f.Version)
			}
			if f.Hash != created.StateFiles[0].Hash || f.Hash == "" {
				t.Fatalf("pinned A hash mismatch with the captured snapshot")
			}
		}
	}
}

// T7.40.3 — Unknown snapshot returns 404: an id that was never persisted,
// and a snapshot that belongs to a DIFFERENT project, are both not-found.
func TestP723UnknownSnapshotNotFound(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	if _, err := p718Svc(t, h).CreateExecutionSnapshot(context.Background(), projectID.String()); err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	svc := p723Svc(t, h)
	// A never-persisted id.
	if _, err := svc.GetSnapshot(context.Background(), projectID.String(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	// A snapshot from a different project addressed through this project's id.
	otherStr := createPhaseProject(t, h)
	rec, err := svc.GetSnapshot(context.Background(), otherStr, createdIDFor(t, h, projectID))
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("cross-project get err = %v, want ErrNotFound (rec=%v)", err, rec)
	}
}

// T7.40.4 — Mutation endpoint unavailable: the snapshot-read surface is
// read-only — no PUT/POST/PATCH route is registered (the mux 405s writes on
// the snapshot path), and the live stored snapshot row is unchanged after a
// rejected write (the load-bearing non-mutation proof).
func TestP723SnapshotMutationUnavailable(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	created, err := p718Svc(t, h).CreateExecutionSnapshot(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	before := p723SnapSig(t, h, created.ID.String())
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodPatch} {
		code := httpRaw(t, method, h.BaseURL+"/api/v1/projects/"+projectID.String()+"/context-snapshots/"+created.ID.String(), map[string]any{"state_key": "HACKED"})
		if code != http.StatusMethodNotAllowed && code != http.StatusBadRequest {
			t.Fatalf("%s snapshot = %d, want 405/400 (no mutation surface)", method, code)
		}
	}
	if after := p723SnapSig(t, h, created.ID.String()); after != before {
		t.Fatalf("snapshot altered by rejected write: before=%q after=%q", before, after)
	}
}

// createdIDFor returns the id of the project's single persisted snapshot.
func createdIDFor(t *testing.T, h *Harness, projectID interface{ String() string }) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(), "SELECT id::text FROM context_snapshots WHERE project_id = '"+projectID.String()+"'", &id); err != nil {
		t.Fatalf("scan snapshot id: %v", err)
	}
	return id
}

// p723SnapSig returns a byte-identical signature of the snapshot row + its
// file pins (the T7.40.4 non-mutation proof — a rejected write must not
// touch any persisted column).
func p723SnapSig(t *testing.T, h *Harness, id string) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(),
		"SELECT coalesce(string_agg(id::text||':'||state_key||':'||runtime_state_version::text, ',' ORDER BY id), '') FROM context_snapshots WHERE id = '"+id+"'", &sig); err != nil {
		t.Fatalf("sig: %v", err)
	}
	var filesSig string
	if err := h.DBScan(context.Background(),
		"SELECT coalesce(string_agg(type_key||':'||version::text||':'||content_hash, ',' ORDER BY type_key), '') FROM context_snapshot_files WHERE snapshot_id = '"+id+"'", &filesSig); err != nil {
		t.Fatalf("files sig: %v", err)
	}
	return sig + " | " + filesSig
}

// --- REST contract (live HTTP surface) ------------------------------------

// T7.37 REST — GET /context-preview returns 200 with the state, role, and
// the estimated token count for a configured project; 404 for a project with
// no runtime (unknown project), and the op does not persist a snapshot
// (direct SQL after the call).
func TestP723PreviewRESTContract(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+projectID.String()+"/context-preview", "")
	if status != http.StatusOK {
		t.Fatalf("GET context-preview = %d, want 200: %+v", status, out)
	}
	if out["state"] != "S" || out["role"] != "P718-ROLE" {
		t.Fatalf("preview body state/role = %v/%v, want S/P718-ROLE", out["state"], out["role"])
	}
	tok, ok := out["estimated_tokens"].(float64)
	if !ok || tok <= 0 {
		t.Fatalf("estimated_tokens = %v, want a positive number", out["estimated_tokens"])
	}
	// No snapshot was persisted by the preview call.
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM context_snapshots WHERE project_id = '"+projectID.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("REST preview persisted %d snapshot(s)", n)
	}
	// Unknown project (no runtime) -> 404.
	bare := createPhaseProject(t, h)
	status, _ = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+bare+"/context-preview", "")
	if status != http.StatusNotFound {
		t.Fatalf("preview for runtime-less project = %d, want 404", status)
	}
}

// T7.40 REST — the snapshot list/get endpoints: 200 with the persisted
// snapshots, an unknown snapshot id is 404, and the list is empty (not null)
// for a project with none.
func TestP723SnapshotRESTContract(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _ := p718Rig(t, h)
	created, err := p718Svc(t, h).CreateExecutionSnapshot(context.Background(), projectID.String())
	if err != nil {
		t.Fatalf("CreateExecutionSnapshot: %v", err)
	}
	base := h.BaseURL + "/api/v1/projects/" + projectID.String() + "/context-snapshots"
	// List.
	status, out := doJSON(t, http.MethodGet, base, "")
	if status != http.StatusOK {
		t.Fatalf("GET list = %d, want 200", status)
	}
	list, _ := out["snapshots"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %d snapshots, want 1", len(list))
	}
	// Get by id.
	status, out = doJSON(t, http.MethodGet, base+"/"+created.ID.String(), "")
	if status != http.StatusOK || out["id"] != created.ID.String() {
		t.Fatalf("GET snapshot = %d id=%v, want 200 %s", status, out["id"], created.ID.String())
	}
	// Unknown id -> 404.
	status, _ = doJSON(t, http.MethodGet, base+"/00000000-0000-0000-0000-000000000000", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown = %d, want 404", status)
	}
	// A project with no snapshots lists an EMPTY array (not null).
	bare := createPhaseProject(t, h)
	status, out = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+bare+"/context-snapshots", "")
	if status != http.StatusOK {
		t.Fatalf("empty list = %d, want 200", status)
	}
	empty, ok := out["snapshots"].([]any)
	if !ok || len(empty) != 0 {
		t.Fatalf("empty list = %v, want an empty array", out["snapshots"])
	}
}
