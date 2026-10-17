package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/role"
)

// P8-49 (WBS 8.50 + Phase 8 DoD): the phase-closing end-to-end acceptance.
// Build the FULLY CONFIGURED execution context the WBS names — ENGINEER at
// IMPLEMENT with system prompt v4, ENGINEER.md v5, TDD.md v3, IMPLEMENT
// instructions v2, the four state files at v11/v6/v4/v2, template
// IMPLEMENT_WORK_UNIT v5, protocol IMPLEMENTATION_RESPONSE v2 — compose the
// canonical prompt through the P8-38 composer and verify (a) the full
// component checklist and (b) the persistence checklist from the persisted
// rows. Then modify ARCHITECTURE, ENGINEER.md, and the template; verify the
// HISTORICAL prompt is unchanged while a NEW composition reflects the new
// versions; and prove BOTH remain reconstructable after a restart (fresh
// service boot over the SAME persisted database, no re-seeding).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - ENGINEER.md / TDD.md = the ENGINEER role's two assigned ROLE
//     INSTRUCTIONS (the rig's impl-rules doc is not one of the WBS-named
//     sources; the two named role docs are created + assigned project-scoped
//     — a global instruction on a project role is a cross-scope reject).
//     "ENGINEER.md v5" / "TDD.md v3" are REACHED through the
//     instruction-version append path (the repository Create writes no
//     version row, so the first append mints v1).
//   - Given versions are reached by CONTROLLED appends through the
//     versioned paths BEFORE snapshot A is created (UpdateSystemPrompt /
//     InstructionVersionRepository.AppendVersion / StateFileService.
//     UpdateContent / PromptTemplateVersionRepository.AppendVersion /
//     StateInstructionRepository.AppendVersion); the source MODIFICATIONS
//     (WBS 8.50 "Then modify ARCHITECTURE, ENGINEER.md, and the template")
//     are the SAME ops applied again after Prompt A — so Prompt A is
//     genuinely historical and Prompt B (a fresh snapshot) is advanced.
//   - HISTORICAL IDENTITY: Prompt A's reconstruction is its persisted
//     canonical_prompts row (content + hash byte-identical) plus its pinned
//     provenance (template v5, role row v1, machine v1) AND every pinned
//     historical source row (system v4, ENGINEER.md v5, TDD v3, the four
//     state-file pins at the Given versions, template v5, protocol v2) read
//     back byte-exact — reconstruction from pinned rows only, no
//     re-derivation from live sources.
//   - RESTART: h1.Close() + NewHarnessOn(sameDBName) (the p834RestartRig /
//     P8-48 shape) — a fresh boot over the same persisted rows; BOTH the
//     historical Prompt A and the new Prompt B are recomposed from their
//     pinned inputs (snapshot + pinned template version) and must be
//     byte-identical to the pre-restart records.
//   - COMPRESSION POLICIES (the WBS persist-checklist item): derived per
//     component by prompt.DefaultPolicy (P8-30 — identity-bearing
//     work-unit/test content PRESERVE_EXACT, narrative context COMPRESSIBLE,
//     protocol/control PRESERVE_SEMANTICS); there is no compression-policy
//     persistence column in Phase 8 (Phase 9 introduces the compression
//     service), so the checklist item is pinned as the DERIVED per-component
//     policy set, stated here.

// p849 given / advanced version contents (the WBS 8.50 Given set + the
// three modified sources' Then set).
const (
	p849Sys4       = "ENGINEER system prompt v4"
	p849EngMD5     = "ENGINEER.md v5: implement the active work unit; verify with deterministic tests."
	p849TDD3       = "TDD.md v3: write the failing test first, then the minimal implementation."
	p849ImplInstr2 = "IMPLEMENT instructions v2: complete the active unit and report status in the required format."
	p849PS11       = "PROJECT_STATE v11"
	p849Arch6      = "ARCHITECTURE v6"
	p849PhS4       = "PHASE_STATE v4"
	p849AT2        = p844ActiveTaskContent // the rig's ACTIVE_TASK v2 content
	p849Tmpl5      = "TEMPLATE-5 marker: {{work_unit.task}} / {{response_protocol.schema}}"
	p849Proto2     = "IMPLEMENTATION_RESPONSE v2 schema body"

	// The three WBS 8.50 source MODIFICATIONS (Then set).
	p849Arch7  = "ARCHITECTURE v7 (modified)"
	p849EngMD6 = "ENGINEER.md v6 (modified)"
	p849Tmpl6  = "TEMPLATE-6 marker (modified): {{work_unit.task}}"
)

// p849EngineerID looks up the P844-ENGINEER role id by key.
func p849EngineerID(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM roles WHERE key = 'P844-ENGINEER'", &id); err != nil {
		t.Fatalf("scan engineer role: %v", err)
	}
	return id
}

// p849ProjectID returns the project id the ENGINEER role belongs to.
func p849ProjectID(t *testing.T, h *Harness, engID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT project_id FROM roles WHERE id = '"+engID.String()+"'", &id); err != nil {
		t.Fatalf("scan role project: %v", err)
	}
	return id
}

// p849ActiveTaskFileID returns the ACTIVE_TASK state file bound to the unit.
func p849ActiveTaskFileID(t *testing.T, h *Harness, unitID uuid.UUID) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(),
		"SELECT f.id::text FROM state_files f WHERE f.work_unit_id = '"+unitID.String()+"' AND f.type_id = (SELECT id FROM state_file_types WHERE key = 'ACTIVE_TASK')", &id); err != nil {
		t.Fatalf("scan active-task file: %v", err)
	}
	return id
}

// p849StateID returns the project's IMPLEMENT workflow-state id.
func p849StateID(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM workflow_states WHERE key = 'IMPLEMENT' AND machine_id = (SELECT id FROM state_machines WHERE project_id = '"+projectID.String()+"')", &id); err != nil {
		t.Fatalf("scan IMPLEMENT state: %v", err)
	}
	return id
}

// p849ReachGiven advances the WBS 8.50 sources to their Given versions from
// the rig's landed versions and returns the ids the later halves need.
func p849ReachGiven(t *testing.T, h *Harness, engID, tmplID, unitID uuid.UUID) (engMDID, tddID string) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	rsvc := p718RoleSvc(t, h)
	projectID := p849ProjectID(t, h, engID)

	// ENGINEER.md + TDD.md: two project-scoped role instructions assigned to
	// the ENGINEER role (created at v1, reached at v5/v3 by appends).
	engMD := newInstructionRecord(t, &projectID, "engineer-md", "ENGINEER.md", "ENGINEER.md v1 placeholder")
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, engMD); err != nil {
		t.Fatalf("create ENGINEER.md: %v", err)
	}
	engMDID = engMD.ID.String()
	tdd := newInstructionRecord(t, &projectID, "tdd", "TDD.md", "TDD.md v1 placeholder")
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, tdd); err != nil {
		t.Fatalf("create TDD.md: %v", err)
	}
	tddID = tdd.ID.String()
	if err := rsvc.AssignInstruction(ctx, engID.String(), engMDID); err != nil {
		t.Fatalf("assign ENGINEER.md: %v", err)
	}
	if err := rsvc.AssignInstruction(ctx, engID.String(), tddID); err != nil {
		t.Fatalf("assign TDD.md: %v", err)
	}

	// System prompt v1 -> v4 (a versioned update bumps the row).
	for v := 2; v <= 4; v++ {
		if _, err := rsvc.UpdateSystemPrompt(ctx, engID.String(), p849Sys4, v-1); err != nil {
			t.Fatalf("set system prompt v%d: %v", v, err)
		}
	}

	// ENGINEER.md -> v5 (the Create writes no version row; appends mint 1..5).
	ivRepo := dbrepo.NewInstructionVersionRepository(raw)
	for v := 1; v <= 5; v++ {
		content := "ENGINEER.md v" + itoa(v)
		if v == 5 {
			content = p849EngMD5
		}
		if _, err := ivRepo.AppendVersion(ctx, engMDID, content); err != nil {
			t.Fatalf("append ENGINEER.md v%d: %v", v, err)
		}
	}
	// TDD.md -> v3.
	for v := 1; v <= 3; v++ {
		content := "TDD.md v" + itoa(v)
		if v == 3 {
			content = p849TDD3
		}
		if _, err := ivRepo.AppendVersion(ctx, tddID, content); err != nil {
			t.Fatalf("append TDD v%d: %v", v, err)
		}
	}

	// IMPLEMENT state instruction v1 (rig) -> v2.
	stateID := p849StateID(t, h, projectID)
	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, p849ImplInstr2); err != nil {
		t.Fatalf("append IMPLEMENT instruction v2: %v", err)
	}

	// State files: the rig lands PROJECT_STATE v8 (-> v11), ARCHITECTURE v3
	// (-> v6); PHASE_STATE v4 and ACTIVE_TASK v2 are already at the Given
	// versions.
	svc8 := p708Service(t, h)
	var fPS string
	if err := h.DBScan(ctx, "SELECT f.id::text FROM state_files f JOIN state_file_types ft ON ft.id = f.type_id WHERE ft.key = 'PROJECT_STATE' AND f.scope = 'PROJECT' AND f.project_id = '"+projectID.String()+"'", &fPS); err != nil {
		t.Fatalf("scan PROJECT_STATE file: %v", err)
	}
	for v := 9; v <= 11; v++ {
		content := "PROJECT_STATE v" + itoa(v)
		if v == 11 {
			content = p849PS11
		}
		if _, err := svc8.UpdateContent(ctx, fPS, content, v-1); err != nil {
			t.Fatalf("write PROJECT_STATE v%d: %v", v, err)
		}
	}
	var fArch string
	if err := h.DBScan(ctx, "SELECT f.id::text FROM state_files f JOIN state_file_types ft ON ft.id = f.type_id WHERE ft.key = 'ARCHITECTURE' AND f.scope = 'PROJECT' AND f.project_id = '"+projectID.String()+"'", &fArch); err != nil {
		t.Fatalf("scan ARCHITECTURE file: %v", err)
	}
	for v := 4; v <= 6; v++ {
		content := "ARCHITECTURE v" + itoa(v)
		if v == 6 {
			content = p849Arch6
		}
		if _, err := svc8.UpdateContent(ctx, fArch, content, v-1); err != nil {
			t.Fatalf("write ARCHITECTURE v%d: %v", v, err)
		}
	}
	// The ACTIVE_TASK file is at its Given v2 content (the rig lands it
	// at v2 with p844ActiveTaskContent — asserted below, no overwrite).

	// Template v1 -> v5.
	tmplRepo := dbrepo.NewPromptTemplateVersionRepository(raw)
	for v := 2; v <= 5; v++ {
		content := "TEMPLATE-v" + itoa(v)
		if v == 5 {
			content = p849Tmpl5
		}
		if _, err := tmplRepo.AppendVersion(ctx, tmplID, content); err != nil {
			t.Fatalf("append template v%d: %v", v, err)
		}
	}
	return
}

// p849ModifySources performs the WBS 8.50 "Then modify ARCHITECTURE,
// ENGINEER.md, and the prompt template" step: advance those three sources to
// new versions (the other sources keep their Given versions, so Prompt B's
// drift is attributable to exactly the three modified sources + the fresh
// snapshot).
func p849ModifySources(t *testing.T, h *Harness, engID, tmplID uuid.UUID, engMDID, archID string) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	if _, err := dbrepo.NewInstructionVersionRepository(raw).AppendVersion(ctx, engMDID, p849EngMD6); err != nil {
		t.Fatalf("modify ENGINEER.md: %v", err)
	}
	svc8 := p708Service(t, h)
	if _, err := svc8.UpdateContent(ctx, archID, p849Arch7, 6); err != nil {
		t.Fatalf("modify ARCHITECTURE: %v", err)
	}
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, tmplID, p849Tmpl6); err != nil {
		t.Fatalf("modify template: %v", err)
	}
	_ = engID
}

// p849Compose issues one composition over the given snapshot + template
// version.
func p849Compose(t *testing.T, h *Harness, snapID, tmplID uuid.UUID, tmplVer int) *persistence.CanonicalPromptRecord {
	t.Helper()
	rec, err := p838Composer(t, h).ComposePrompt(context.Background(), prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: tmplID, TemplateVersion: tmplVer,
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return rec
}

// T8.50 — End-to-end: the configured context composes the full component
// checklist; the persistence checklist is verifiable from the persisted
// rows; the three source modifications leave the historical prompt unchanged
// while the new composition reflects the new versions; and after a restart
// BOTH prompts remain reconstructable.
func TestP849Phase8EndToEndAcceptance(t *testing.T) {
	h, dbName := p834RestartRig(t)
	ctx := context.Background()
	snapSvc := p718Svc(t, h)

	// The base position: the P8-44 rig (ENGINEER at IMPLEMENT, the active
	// unit bound, the four state files, template v1, protocol pinned).
	_, tmplID, unit1, _ := p844Rig(t, h)
	engID := p849EngineerID(t, h)
	projectID := p849ProjectID(t, h, engID)

	// Reach the WBS 8.50 Given versions.
	engMDID, tddID := p849ReachGiven(t, h, engID, tmplID, unit1)

	// Advance the protocol to v2 (the WBS Given version) and re-pin the
	// state reference at v2 (the rig pins v1).
	stateID := p849StateID(t, h, projectID)
	var protoID uuid.UUID
	if err := h.DBScan(ctx, "SELECT protocol_id FROM state_protocol_references WHERE state_id = '"+stateID.String()+"'", &protoID); err != nil {
		t.Fatalf("scan protocol ref: %v", err)
	}
	if _, err := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw()).AppendVersion(ctx, protoID, p849Proto2); err != nil {
		t.Fatalf("advance protocol to v2: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE state_protocol_references SET protocol_version = 2 WHERE state_id = '"+stateID.String()+"'"); err != nil {
		t.Fatalf("re-pin protocol ref to v2: %v", err)
	}

	// Snapshot A pins the Given versions; Prompt A composes at the Given
	// template (v5) + protocol (v2) versions.
	snapA, err := snapSvc.CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("create snapshot A: %v", err)
	}
	recA := p849Compose(t, h, snapA.ID, tmplID, 5)

	// --- Composition checklist (WBS 8.50 "Composition produces"). ---
	{
		c := recA.Content
		// System Prompt.
		if !strings.Contains(c, p849Sys4) {
			t.Fatalf("checklist: missing system prompt v4; content:\n%s", c)
		}
		// Role Instructions (ENGINEER.md v5 + TDD.md v3).
		if !strings.Contains(c, p849EngMD5) {
			t.Fatalf("checklist: missing ENGINEER.md v5; content:\n%s", c)
		}
		if !strings.Contains(c, p849TDD3) {
			t.Fatalf("checklist: missing TDD.md v3; content:\n%s", c)
		}
		// State Instructions (IMPLEMENT v2).
		if !strings.Contains(c, p849ImplInstr2) {
			t.Fatalf("checklist: missing IMPLEMENT instructions v2; content:\n%s", c)
		}
		// Project / Architecture / Phase / Task Context (the four state
		// files) + the work-unit implementation request (id/objective).
		for _, tok := range []string{p849PS11, p849Arch6, p849PhS4, p849AT2,
			"ID: " + unit1.String(), "Objective p844-unit-1"} {
			if !strings.Contains(c, tok) {
				t.Fatalf("checklist: missing context token %q; content:\n%s", tok, c)
			}
		}
		// Dependency Contracts / Acceptance Criteria / Required Tests ride
		// on the work-unit component (the rig's unit 1 spec).
		for _, tok := range []string{"dependencies: ", "acceptance criteria: ", "required tests: "} {
			if !strings.Contains(c, tok) {
				t.Fatalf("checklist: missing work-unit section %q; content:\n%s", tok, c)
			}
		}
		// Implementation Request (the active unit) + Response Protocol.
		if !strings.Contains(c, p849Proto2) {
			t.Fatalf("checklist: missing response protocol v2; content:\n%s", c)
		}
		if !strings.Contains(c, "Response protocol v2") {
			t.Fatalf("checklist: missing response protocol version line; content:\n%s", c)
		}
	}

	// --- Persistence checklist (WBS 8.50 "Persist"). ---
	{
		// canonical content + prompt hash (byte-identical to the record).
		var content, hash string
		if err := h.DBScan(ctx, "SELECT content, hash FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'", &content, &hash); err != nil {
			t.Fatalf("persistence: read prompt row: %v", err)
		}
		if content != recA.Content || hash != recA.Hash {
			t.Fatalf("persistence: canonical content/hash differ from the record")
		}
		// snapshot hash (the pin digest — the WBS 8.49 record).
		if p848SnapshotHash(t, h, snapA.ID.String()) == "" {
			t.Fatalf("persistence: empty snapshot hash")
		}
		// template version + role versions + state-machine version.
		var tmplVer, roleVer, smVer int32
		if err := h.DBScan(ctx, "SELECT template_version, role_version, state_machine_version FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'", &tmplVer, &roleVer, &smVer); err != nil {
			t.Fatalf("persistence: read versions: %v", err)
		}
		if int(tmplVer) != 5 {
			t.Fatalf("persistence: template_version = %d, want 5", tmplVer)
		}
		if int(roleVer) != 1 { // role ROW version (system-prompt appends do not bump it)
			t.Fatalf("persistence: role_version = %d, want 1", roleVer)
		}
		if int(smVer) != 1 {
			t.Fatalf("persistence: state_machine_version = %d, want 1", smVer)
		}
		// The snapshot's pinned role context carries the Given system prompt
		// v4 (role version pinned on the record).
		var roleJSON []byte
		if err := h.DBScan(ctx, "SELECT role_snapshot FROM context_snapshots WHERE id = '"+snapA.ID.String()+"'", &roleJSON); err != nil {
			t.Fatalf("persistence: read role snapshot: %v", err)
		}
		var rs role.RoleSnapshot
		if err := json.Unmarshal(roleJSON, &rs); err != nil {
			t.Fatalf("decode role snapshot: %v", err)
		}
		if rs.SystemPrompt.Version != 4 || rs.SystemPrompt.Content != p849Sys4 {
			t.Fatalf("persistence: pinned system prompt = v%d %q, want v4 %q", rs.SystemPrompt.Version, rs.SystemPrompt.Content, p849Sys4)
		}
		// estimated token usage.
		if recA.EstimatedTokens <= 0 {
			t.Fatalf("persistence: estimated_tokens = %d, want > 0", recA.EstimatedTokens)
		}
		// component provenance (distinct per-class entries).
		entries := p842GetEntries(t, h, projectID, recA.ID)
		if len(entries) < 5 {
			t.Fatalf("persistence: provenance entries = %d, want >= 5 (got %+v)", len(entries), entries)
		}
		// compression policies (derived per component by DefaultPolicy —
		// the stated choice; no Phase-8 persistence column).
		if prompt.DefaultPolicy(prompt.ComponentTaskContext) != prompt.PolicyPreserveExact {
			t.Fatalf("persistence: work-unit policy = %s, want PRESERVE_EXACT", prompt.DefaultPolicy(prompt.ComponentTaskContext))
		}
		if prompt.DefaultPolicy(prompt.ComponentProjectContext) != prompt.PolicyCompressible {
			t.Fatalf("persistence: project-context policy = %s, want COMPRESSIBLE", prompt.DefaultPolicy(prompt.ComponentProjectContext))
		}
	}

	// --- Source modifications: historical unchanged, new reflects. ---
	var modArch string
	if err := h.DBScan(ctx, "SELECT f.id::text FROM state_files f JOIN state_file_types ft ON ft.id = f.type_id WHERE ft.key = 'ARCHITECTURE' AND f.scope = 'PROJECT' AND f.project_id = '"+projectID.String()+"'", &modArch); err != nil {
		t.Fatalf("scan ARCHITECTURE file (modify): %v", err)
	}
	p849ModifySources(t, h, engID, tmplID, engMDID, modArch)

	// The HISTORICAL Prompt A is unchanged (content + hash + pinned
	// provenance), and its pinned historical source rows are byte-identical.
	{
		var histContent, histHash string
		var histTmpl, histRole, histSM int32
		if err := h.DBScan(ctx, "SELECT content, hash, template_version, role_version, state_machine_version FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'",
			&histContent, &histHash, &histTmpl, &histRole, &histSM); err != nil {
			t.Fatalf("read historical A: %v", err)
		}
		if histContent != recA.Content || histHash != recA.Hash {
			t.Fatalf("historical Prompt A moved after source modifications")
		}
		if int(histTmpl) != 5 || int(histRole) != 1 || int(histSM) != 1 {
			t.Fatalf("historical Prompt A provenance drifted: t%d/r%d/s%d", histTmpl, histRole, histSM)
		}
		// Pinned historical rows: ENGINEER.md v5, TDD v3, system v4,
		// ARCHITECTURE v6 pin, template v5, protocol v2.
		var histEng, histTDD, histSys string
		if err := h.DBScan(ctx, "SELECT content FROM instruction_versions WHERE instruction_id = '"+engMDID+"' AND version = 5", &histEng); err != nil {
			t.Fatalf("read historical ENGINEER.md v5: %v", err)
		}
		if histEng != p849EngMD5 {
			t.Fatalf("historical ENGINEER.md v5 = %q, want %q", histEng, p849EngMD5)
		}
		if err := h.DBScan(ctx, "SELECT content FROM instruction_versions WHERE instruction_id = '"+tddID+"' AND version = 3", &histTDD); err != nil {
			t.Fatalf("read historical TDD v3: %v", err)
		}
		if histTDD != p849TDD3 {
			t.Fatalf("historical TDD v3 = %q, want %q", histTDD, p849TDD3)
		}
		if err := h.DBScan(ctx, "SELECT content FROM role_system_prompts WHERE role_id = '"+engID.String()+"' AND version = 4", &histSys); err != nil {
			t.Fatalf("read historical system v4: %v", err)
		}
		if histSys != p849Sys4 {
			t.Fatalf("historical system v4 = %q, want %q", histSys, p849Sys4)
		}
		// The ARCHITECTURE v6 pin in snapshot A (byte-identical).
		var pinVer int32
		var pinContent string
		if err := h.DBScan(ctx, "SELECT version, content FROM context_snapshot_files WHERE snapshot_id = '"+snapA.ID.String()+"' AND type_key = 'ARCHITECTURE'", &pinVer, &pinContent); err != nil {
			t.Fatalf("read snapshot A ARCHITECTURE pin: %v", err)
		}
		if int(pinVer) != 6 || pinContent != p849Arch6 {
			t.Fatalf("snapshot A ARCHITECTURE pin = v%d %q, want v6 %q", pinVer, pinContent, p849Arch6)
		}
		var histTmplC, histProto string
		if err := h.DBScan(ctx, "SELECT content FROM prompt_template_versions WHERE template_id = '"+tmplID.String()+"' AND version = 5", &histTmplC); err != nil {
			t.Fatalf("read historical template v5: %v", err)
		}
		if histTmplC != p849Tmpl5 {
			t.Fatalf("historical template v5 = %q, want %q", histTmplC, p849Tmpl5)
		}
		if err := h.DBScan(ctx, "SELECT content FROM response_protocol_versions WHERE protocol_id = '"+protoID.String()+"' AND version = 2", &histProto); err != nil {
			t.Fatalf("read historical protocol v2: %v", err)
		}
		if histProto != p849Proto2 {
			t.Fatalf("historical protocol v2 = %q, want %q", histProto, p849Proto2)
		}
	}

	// A NEW snapshot B pins the modified versions; Prompt B reflects them
	// (the three modified sources) while the unmodified sources keep their
	// Given content.
	snapB, err := snapSvc.CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("create snapshot B: %v", err)
	}
	recB := p849Compose(t, h, snapB.ID, tmplID, 6)
	if recB.Content == recA.Content || recB.Hash == recA.Hash {
		t.Fatalf("Prompt B identical to Prompt A — the modified sources did not flow through")
	}
	// The three modified sources appear in B at their new versions.
	for _, tok := range []string{p849Arch7, p849EngMD6, "TEMPLATE-6 marker (modified):"} {
		if !strings.Contains(recB.Content, tok) {
			t.Fatalf("Prompt B missing modified source %q; content:\n%s", tok, recB.Content)
		}
	}
	// The UNMODIFIED Given sources keep their content in B.
	for _, tok := range []string{p849Sys4, p849TDD3, p849PS11, p849PhS4, p849ImplInstr2} {
		if !strings.Contains(recB.Content, tok) {
			t.Fatalf("Prompt B missing unmodified Given content %q; content:\n%s", tok, recB.Content)
		}
	}
	// The modified sources' Given content is superseded in B.
	if strings.Contains(recB.Content, p849Arch6) {
		t.Fatalf("Prompt B carries stale ARCHITECTURE v6; content:\n%s", recB.Content)
	}

	// --- Restart: BOTH prompts remain reconstructable. ---
	h.Close()
	h2 := NewHarnessOn(t, baseDSN(), dbName)
	defer h2.Close()
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}

	// Recompose Prompt A (historical inputs: snapshot A + pinned template
	// v5) and Prompt B (snapshot B + pinned template v6); both byte-identical.
	recA2 := p849Compose(t, h2, snapA.ID, tmplID, 5)
	recB2 := p849Compose(t, h2, snapB.ID, tmplID, 6)
	if recA2.Content != recA.Content || recA2.Hash != recA.Hash {
		t.Fatalf("Prompt A not reconstructable after restart")
	}
	if recB2.Content != recB.Content || recB2.Hash != recB.Hash {
		t.Fatalf("Prompt B not reconstructable after restart")
	}
	// The persisted rows survive byte-for-byte.
	var storedA, storedB string
	if err := h2.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'", &storedA); err != nil {
		t.Fatalf("post-restart read A: %v", err)
	}
	if err := h2.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+recB.ID.String()+"'", &storedB); err != nil {
		t.Fatalf("post-restart read B: %v", err)
	}
	if storedA != recA.Content || storedB != recB.Content {
		t.Fatalf("persisted prompts altered by restart")
	}
}
