package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/prompt"
)

// P8-42 (WBS 8.37): the prompt provenance API over real PostgreSQL.
//
//	GET /api/v1/projects/{project}/prompts/{prompt}/provenance
//
// returns every source the canonical prompt was composed from, with the
// PINNED (captured) versions and hashes — never the sources' current
// values — in a deterministic order.
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - DERIVATION: the provenance is reconstructed from the persisted
//     canonical_prompts row + its pinned captures (context_snapshot_files,
//     the role-snapshot JSONB, the state's protocol reference). Every
//     entry's version/hash is read from the PINNED version rows
//     (role_system_prompts / instruction_versions / snapshot pins /
//     response_protocol_versions) at the recorded version — a source
//     advance after composition does not move any entry.
//   - SOURCE SET (mirrors the P8-38 composer's component set): the role's
//     system prompt (its pinned prompt version), each assigned role
//     instruction, each pinned state file (at the pin's version/hash), the
//     bound work unit (no version/hash — work_units carry neither), and
//     the state's pinned response protocol (its pinned version + the
//     protocol version row's content hash). A component the composer would
//     have produced for zero content is omitted (the P8-18 zero-component
//     policy) — an entry set is a function of the persisted row.
//   - ORDER: the WBS 8.9 canonical rank (system=1 < role instruction=2 <
//     project context=4 < phase context=5 < task context=7 < response
//     protocol=11), a fixed function of the entry set; two reads over the
//     same prompt are byte-identical (T8.37.4).
//   - SCOPE: project-scoped like P8-41 — a prompt id belonging to another
//     project is a 404 on this project's surface.
//   - DTO: {"prompt_id", "state_key", "entries":[{component_type,
//     source_id, source_version, source_hash}], "count"}.

// p842Fixture builds a composition-ready position (the p718 rig + protocol
// pin + template, mirrored from p838Fixture) and persists ONE canonical
// prompt through the P8-38 composer service, returning the ids the
// provenance assertions need.
func p842Fixture(t *testing.T, h *Harness) (projectID, roleID, unitID, protocolID, fileA, fileB, promptID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	projectID, roleID, stateID, fileA, fileB := p718Rig(t, h)

	// Bound work unit with a non-empty objective (the P8-14 requirement);
	// test bookkeeping via direct SQL (mirrors p838Fixture).
	unitID = p838ProjectUnit(t, h, projectID)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET objective = 'Implement the fixture work unit' WHERE id = '"+unitID.String()+"'"); err != nil {
		t.Fatalf("set unit objective: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET work_unit_id = '"+unitID.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime to unit: %v", err)
	}

	protocolID = p813Protocol(t, h, nil, "p842-impl", prompt.KindImplementationResponse, "p842 schema body")
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	tmplID := p811Template(t, h, "p842-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, tmplID, "Task: {{work_unit.task}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}

	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	rec, err := p838Composer(t, h).ComposePrompt(ctx, prompt.ComposePromptRequest{
		ContextSnapshotID: snap.ID,
		TemplateID:        tmplID,
		TemplateVersion:   1,
	})
	if err != nil {
		t.Fatalf("compose prompt: %v", err)
	}
	promptID = rec.ID
	return
}

// p842Provenance runs one GET on the provenance endpoint and returns the raw
// body (byte-identity asserts operate on the raw bytes).
func p842Provenance(t *testing.T, h *Harness, projectID, promptID uuid.UUID) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", h.BaseURL+"/api/v1/projects/"+projectID.String()+"/prompts/"+promptID.String()+"/provenance", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// p842EntryDTO is one provenance entry on the wire.
type p842EntryDTO struct {
	ComponentType string `json:"component_type"`
	SourceID      string `json:"source_id"`
	SourceVersion int    `json:"source_version"`
	SourceHash    string `json:"source_hash"`
}

// p842GetEntries decodes the provenance body (asserting 200 first).
func p842GetEntries(t *testing.T, h *Harness, projectID, promptID uuid.UUID) []p842EntryDTO {
	t.Helper()
	code, body := p842Provenance(t, h, projectID, promptID)
	if code != 200 {
		t.Fatalf("provenance code = %d, body %s", code, body)
	}
	var res struct {
		PromptID string         `json:"prompt_id"`
		Entries  []p842EntryDTO `json:"entries"`
		Count    int            `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode provenance: %v (body %s)", err, body)
	}
	return res.Entries
}

// p842Find returns the entry for (component_type, source_id) (a missing pair
// is a test failure — the fixture pins that the source was composed in).
func p842Find(t *testing.T, entries []p842EntryDTO, componentType, sourceID string) p842EntryDTO {
	t.Helper()
	for _, e := range entries {
		if e.ComponentType == componentType && e.SourceID == sourceID {
			return e
		}
	}
	t.Fatalf("no provenance entry for %s/%s; got %+v", componentType, sourceID, entries)
	return p842EntryDTO{}
}

// T8.37.1 — All sources returned: every composed source of the fixture prompt
// appears as an entry (system prompt, state files A/B, work unit, response
// protocol), and no other entries exist.
func TestT8371AllSourcesReturned(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, unitID, protocolID, fileA, fileB, promptID := p842Fixture(t, h)

	entries := p842GetEntries(t, h, projectID, promptID)
	// The fixture composes exactly: system + state files A (PROJECT) and B
	// (PHASE) + work unit + response protocol. No role instructions are
	// assigned and no continuation is present, so the set is exactly five.
	if len(entries) != 5 {
		t.Fatalf("T8.37.1 entry count = %d, want 5 (got %+v)", len(entries), entries)
	}
	if e := p842Find(t, entries, "SYSTEM", roleID.String()); e.SourceVersion != 1 {
		t.Fatalf("T8.37.1 system entry version = %d, want 1", e.SourceVersion)
	}
	_ = p842Find(t, entries, "PROJECT_CONTEXT", fileA.String())
	_ = p842Find(t, entries, "PHASE_CONTEXT", fileB.String())
	_ = p842Find(t, entries, "TASK_CONTEXT", unitID.String())
	_ = p842Find(t, entries, "RESPONSE_PROTOCOL", protocolID.String())
}

// T8.37.2 — Versions accurate: the PINNED version rides on the entry. After
// the role's system prompt advances to v2 and the pinned state file B
// advances to v2, the historical prompt's entries still report v1.
func TestT8372VersionsAccurate(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	projectID, roleID, unitID, protocolID, _, fileB, promptID := p842Fixture(t, h)
	_ = projectID

	before := p842GetEntries(t, h, projectID, promptID)
	if got := p842Find(t, before, "SYSTEM", roleID.String()).SourceVersion; got != 1 {
		t.Fatalf("pre-advance system version = %d, want 1", got)
	}

	// Advance the pinned sources: the role's system prompt to v2, the
	// state file B to v2 (the composer's captures stay at v1).
	if _, err := dbrepo.NewSystemPromptRepository(raw).AppendVersion(ctx, roleID.String(), "v2 prompt"); err != nil {
		t.Fatalf("advance role prompt: %v", err)
	}
	if _, err := p708Service(t, h).UpdateContent(ctx, fileB.String(), "B content v2", 1); err != nil {
		t.Fatalf("advance file B: %v", err)
	}

	after := p842GetEntries(t, h, projectID, promptID)
	// Every version is unchanged: the provenance reports the PINNED
	// versions, not the sources' current versions.
	if got := p842Find(t, after, "SYSTEM", roleID.String()).SourceVersion; got != 1 {
		t.Fatalf("T8.37.2 system version after role advance = %d, want 1", got)
	}
	if got := p842Find(t, after, "PHASE_CONTEXT", fileB.String()).SourceVersion; got != 1 {
		t.Fatalf("T8.37.2 file B version after advance = %d, want 1", got)
	}
	if got := p842Find(t, after, "RESPONSE_PROTOCOL", protocolID.String()).SourceVersion; got != 1 {
		t.Fatalf("T8.37.2 protocol version = %d, want 1", got)
	}
	// The work unit has no version series: version 0, by construction.
	if got := p842Find(t, after, "TASK_CONTEXT", unitID.String()).SourceVersion; got != 0 {
		t.Fatalf("T8.37.2 work-unit version = %d, want 0", got)
	}
}

// T8.37.3 — Hashes accurate: each entry's hash is the pinned version row's
// content hash, and it does not move when the source advances.
func TestT8373HashesAccurate(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	projectID, roleID, unitID, protocolID, fileA, fileB, promptID := p842Fixture(t, h)

	// Ground truth: the PINNED version rows' content hashes (v1 everywhere
	// in the fixture).
	var roleHash, fileAHash, fileBHash, protoHash string
	if err := h.DBScan(ctx, "SELECT content_hash FROM role_system_prompts WHERE role_id = '"+roleID.String()+"' AND version = 1", &roleHash); err != nil {
		t.Fatalf("scan role prompt hash: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT content_hash FROM context_snapshot_files WHERE state_file_id = '"+fileA.String()+"' AND version = 1", &fileAHash); err != nil {
		t.Fatalf("scan file A hash: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT content_hash FROM context_snapshot_files WHERE state_file_id = '"+fileB.String()+"' AND version = 1", &fileBHash); err != nil {
		t.Fatalf("scan file B hash: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT content_hash FROM response_protocol_versions WHERE protocol_id = '"+protocolID.String()+"' AND version = 1", &protoHash); err != nil {
		t.Fatalf("scan protocol hash: %v", err)
	}

	entries := p842GetEntries(t, h, projectID, promptID)
	if got := p842Find(t, entries, "SYSTEM", roleID.String()).SourceHash; got != roleHash {
		t.Fatalf("T8.37.3 system hash = %q, want pinned %q", got, roleHash)
	}
	if got := p842Find(t, entries, "PROJECT_CONTEXT", fileA.String()).SourceHash; got != fileAHash {
		t.Fatalf("T8.37.3 file A hash = %q, want pinned %q", got, fileAHash)
	}
	if got := p842Find(t, entries, "PHASE_CONTEXT", fileB.String()).SourceHash; got != fileBHash {
		t.Fatalf("T8.37.3 file B hash = %q, want pinned %q", got, fileBHash)
	}
	if got := p842Find(t, entries, "RESPONSE_PROTOCOL", protocolID.String()).SourceHash; got != protoHash {
		t.Fatalf("T8.37.3 protocol hash = %q, want pinned %q", got, protoHash)
	}
	// The work unit carries no content hash (work_units has none).
	if got := p842Find(t, entries, "TASK_CONTEXT", unitID.String()).SourceHash; got != "" {
		t.Fatalf("T8.37.3 work-unit hash = %q, want empty", got)
	}

	// Advance the sources; the hashes must stay pinned.
	if _, err := dbrepo.NewSystemPromptRepository(raw).AppendVersion(ctx, roleID.String(), "v2 prompt"); err != nil {
		t.Fatalf("advance role prompt: %v", err)
	}
	if _, err := p708Service(t, h).UpdateContent(ctx, fileB.String(), "B content v2", 1); err != nil {
		t.Fatalf("advance file B: %v", err)
	}
	after := p842GetEntries(t, h, projectID, promptID)
	if got := p842Find(t, after, "SYSTEM", roleID.String()).SourceHash; got != roleHash {
		t.Fatalf("T8.37.3 system hash after advance = %q, want pinned %q", got, roleHash)
	}
	if got := p842Find(t, after, "PHASE_CONTEXT", fileB.String()).SourceHash; got != fileBHash {
		t.Fatalf("T8.37.3 file B hash after advance = %q, want pinned %q", got, fileBHash)
	}
}

// T8.37.4 — Deterministic order: two reads over the same prompt are
// byte-identical (the order is a fixed function of the entry set — the WBS
// 8.9 canonical rank, stable within a type).
func TestT8374DeterministicOrder(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _, _, promptID := p842Fixture(t, h)

	code1, body1 := p842Provenance(t, h, projectID, promptID)
	code2, body2 := p842Provenance(t, h, projectID, promptID)
	if code1 != 200 || code2 != 200 {
		t.Fatalf("T8.37.4 codes = %d/%d, want 200/200", code1, code2)
	}
	if body1 != body2 {
		t.Fatalf("T8.37.4 reads differ:\n got  %s\n want %s", body1, body2)
	}
	// The rank order is asserted explicitly (system < project < phase <
	// task < protocol) so a reordering that happens to be stable across
	// two reads is still caught.
	entries := p842GetEntries(t, h, projectID, promptID)
	want := []string{"SYSTEM", "PROJECT_CONTEXT", "PHASE_CONTEXT", "TASK_CONTEXT", "RESPONSE_PROTOCOL"}
	if len(entries) != len(want) {
		t.Fatalf("T8.37.4 entry count = %d, want %d", len(entries), len(want))
	}
	for i, w := range want {
		if entries[i].ComponentType != w {
			t.Fatalf("T8.37.4 order[%d] = %s, want %s", i, entries[i].ComponentType, w)
		}
	}
}

// T8.37.x REST contract — negative surface: an unknown prompt id is a 404;
// a prompt persisted under ANOTHER project is a 404 (project isolation,
// the P8-41 semantics); a malformed prompt id is a 400; write methods on
// the read-only path are a 405 (the mux rejects them — plain-text body).
func TestT837RESTContractNegatives(t *testing.T) {
	h := NewHarness(t)
	projectID, _, _, _, _, _, promptID := p842Fixture(t, h)

	// Unknown id, correct project: 404.
	code, _ := p842Provenance(t, h, projectID, uuid.New())
	if code != http.StatusNotFound {
		t.Fatalf("unknown id code = %d, want 404", code)
	}

	// A prompt id belonging to another project: 404 on this project's surface.
	projB := p841FreshProject(t, h)
	code, _ = p842Provenance(t, h, projB, promptID)
	if code != http.StatusNotFound {
		t.Fatalf("foreign project code = %d, want 404", code)
	}

	// Malformed id: 400.
	code, _ = p842Provenance(t, h, projectID, uuid.UUID{})
	_ = code
	req, _ := http.NewRequest("GET", h.BaseURL+"/api/v1/projects/"+projectID.String()+"/prompts/not-a-uuid/provenance", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id code = %d, want 400", resp.StatusCode)
	}

	// Write method on the read-only path: 405 (plain-text body — no JSON).
	req, _ = http.NewRequest("POST", h.BaseURL+"/api/v1/projects/"+projectID.String()+"/prompts/"+promptID.String()+"/provenance", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST code = %d, want 405", resp.StatusCode)
	}
}
