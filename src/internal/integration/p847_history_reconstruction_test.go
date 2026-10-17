package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/prompt"
	"overseer/internal/role"
)

// P8-47 (WBS 8.48): historical prompt reconstruction. Prompt A is composed at
// the WBS 8.48 Given source versions (ENGINEER system v3, the TDD role
// instruction v2, ACTIVE_TASK v5, template v4); then ALL of those sources
// advance (system v4, TDD v3, ACTIVE_TASK v6, template v5); a newly composed
// Prompt B uses the new versions while Prompt A remains exactly
// reconstructable from its historical references (its persisted artifact +
// provenance, read back by historical identity — not re-composed).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - SOURCE MAPPING: the four WBS 8.48 sources map to the P8-38 composition
//     inputs as follows — ENGINEER system = the role's captured system
//     prompt, TDD.md = a TDD role instruction assigned to the ENGINEER role
//     (the P8-44 rig assigns none, so the test creates + assigns it),
//     ACTIVE_TASK = the captured WORK_UNIT-scoped state file, Template = the
//     template version named by the ComposePromptRequest. The state files are
//     captured into the CONTEXT SNAPSHOT, so Prompt A uses snapshot A (pinned
//     at the Given versions) and Prompt B uses a FRESH snapshot B (pinned at
//     the advanced versions) — the composer reads only snapshot-captured
//     values, never "latest".
//   - VERSION REACH: the rig's Given position lands each source at v1 (system
//     prompt) / v2 (ACTIVE_TASK) / v1 (template), so the Given versions are
//     reached by controlled appends through the versioned paths
//     (UpdateSystemPrompt / InstructionVersionRepository.AppendVersion /
//     StateFileService.UpdateContent / PromptTemplateVersionRepository
//     AppendVersion) BEFORE snapshot A is created; the advances are the same
//     ops applied again after Prompt A.
//   - HISTORICAL IDENTITY: Prompt A's reconstruction is the persisted
//     canonical_prompts row (content + hash byte-identical to the returned
//     record) plus its provenance fields (template version 4, role row version 1)
//     read back — and every pinned historical source row (system prompt v3 in
//     role_system_prompts, TDD v2 in instruction_versions, ACTIVE_TASK v5 pin
//     in context_snapshot_files, template v4 in prompt_template_versions) is
//     verified to still carry the exact content that entered Prompt A (no
//     re-derivation, no re-composition).

const (
	// p847 given-version contents (the WBS 8.48 Given set).
	p847Sys3     = "ENGINEER system v3"
	p847Tdd2     = "TDD v2: test-first, deterministic verification."
	p847ActiveT5 = "ACTIVE_TASK v5 (given)"
	p847Tmpl4    = "TEMPLATE-4 marker: implementation task"
	// p847 advanced-version contents (the WBS 8.48 Then set).
	p847Sys4     = "ENGINEER system v4"
	p847Tdd3     = "TDD v3: test-first, deterministic verification (advanced)."
	p847ActiveT6 = "ACTIVE_TASK v6 (advanced)"
	p847Tmpl5    = "TEMPLATE-5 marker: implementation task (advanced)"
)

// p847Advance advances the four WBS 8.48 sources from their Given versions to
// the advanced versions (the WBS 8.48 "Then update all of them" step).
func p847Advance(t *testing.T, h *Harness, engID uuid.UUID, tmplID uuid.UUID, tddID, atID string) {
	t.Helper()
	ctx := context.Background()
	// ENGINEER system prompt: v3 -> v4 (a versioned update bumps the row).
	rsvc := p718RoleSvc(t, h)
	if _, err := rsvc.UpdateSystemPrompt(ctx, engID.String(), p847Sys4, 3); err != nil {
		t.Fatalf("advance system prompt: %v", err)
	}
	// TDD role instruction: v2 -> v3.
	if _, err := dbrepo.NewInstructionVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, tddID, p847Tdd3); err != nil {
		t.Fatalf("advance TDD instruction: %v", err)
	}
	// ACTIVE_TASK file: v5 -> v6.
	svc8 := p708Service(t, h)
	if _, err := svc8.UpdateContent(ctx, atID, p847ActiveT6, 5); err != nil {
		t.Fatalf("advance ACTIVE_TASK file: %v", err)
	}
	// Template: v4 -> v5.
	if _, err := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, tmplID, p847Tmpl5); err != nil {
		t.Fatalf("advance template: %v", err)
	}
}

// p847Role returns the project role id by key.
func p847Role(t *testing.T, h *Harness, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), "SELECT id FROM roles WHERE key = '"+key+"'", &id); err != nil {
		t.Fatalf("scan role %s: %v", key, err)
	}
	return id
}

// p847RoleProjectID returns the project id the role belongs to.
func p847RoleProjectID(t *testing.T, h *Harness, roleID uuid.UUID) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(), "SELECT project_id::text FROM roles WHERE id = '"+roleID.String()+"'", &id); err != nil {
		t.Fatalf("scan role project: %v", err)
	}
	return id
}

// p847ActiveTaskFileID returns the ACTIVE_TASK state file id bound to the unit.
func p847ActiveTaskFileID(t *testing.T, h *Harness, unitID uuid.UUID) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(),
		"SELECT f.id::text FROM state_files f WHERE f.work_unit_id = '"+unitID.String()+"' AND f.type_id = (SELECT id FROM state_file_types WHERE key = 'ACTIVE_TASK')", &id); err != nil {
		t.Fatalf("scan active-task file: %v", err)
	}
	return id
}

// p847Req builds a ComposePrompt request over the given snapshot + template
// version.
func p847Req(snapID, tmplID uuid.UUID, tmplVersion int) prompt.ComposePromptRequest {
	return prompt.ComposePromptRequest{ContextSnapshotID: snapID, TemplateID: tmplID, TemplateVersion: tmplVersion}
}

// WBS 8.48 — Prompt A (Given versions) stays exactly reconstructable from its
// historical references after ALL of its sources advance; a newly composed
// Prompt B uses the new versions.
func TestP847HistoricalPromptReconstruction(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	// Given position: the P8-44 rig (ENGINEER role at IMPLEMENT, unit 1
	// bound, four state files, template v1, protocol pinned).
	_, tmplID, unit1, _ := p844Rig(t, h)
	svc := p844ImplComposer(t, h)
	engID := p847Role(t, h, "P844-ENGINEER")

	// The WBS 8.48 "TDD.md" source: a TDD role instruction assigned to the
	// ENGINEER role (the P8-44 rig assigns none — the rig's single
	// instruction is an impl-rules doc, not the TDD one). Project-scoped (a
	// global instruction on a project role is a cross-scope reject) and
	// created at v1 with the placeholder content; advanced to v2 below.
	projID, err := uuid.Parse(p847RoleProjectID(t, h, engID))
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	tddRec := newInstructionRecord(t, &projID, "TDD", "TDD", "TDD v1 placeholder")
	if err := dbrepo.NewInstructionRepository(h.App.DB().Raw()).Create(ctx, tddRec); err != nil {
		t.Fatalf("create TDD instruction: %v", err)
	}
	tddID := tddRec.ID.String()
	if err := p718RoleSvc(t, h).AssignInstruction(ctx, engID.String(), tddID); err != nil {
		t.Fatalf("assign TDD instruction: %v", err)
	}

	// Reach the Given versions (the rig lands at system v1, ACTIVE_TASK v2,
	// template v1).
	rsvc := p718RoleSvc(t, h)
	for v := 2; v <= 3; v++ {
		if _, err := rsvc.UpdateSystemPrompt(ctx, engID.String(), p847Sys3, v-1); err != nil {
			t.Fatalf("set system prompt v%d: %v", v, err)
		}
	}
	// The repository Create writes NO version row (the first append mints v1),
	// so append the v1 placeholder then v2.
	if _, err := dbrepo.NewInstructionVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, tddID, "TDD v1 placeholder"); err != nil {
		t.Fatalf("set TDD v1: %v", err)
	}
	if _, err := dbrepo.NewInstructionVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, tddID, p847Tdd2); err != nil {
		t.Fatalf("set TDD v2: %v", err)
	}
	svc8 := p708Service(t, h)
	atID := p847ActiveTaskFileID(t, h, unit1)
	for v := 3; v <= 5; v++ {
		if _, err := svc8.UpdateContent(ctx, atID, p847ActiveT5, v-1); err != nil {
			t.Fatalf("set ACTIVE_TASK v%d: %v", v, err)
		}
	}
	tmplRepo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())
	for v := 2; v <= 3; v++ {
		if _, err := tmplRepo.AppendVersion(ctx, tmplID, "template filler v"+itoa(v)); err != nil {
			t.Fatalf("set template v%d: %v", v, err)
		}
	}
	if _, err := tmplRepo.AppendVersion(ctx, tmplID, p847Tmpl4); err != nil {
		t.Fatalf("set template v4: %v", err)
	}

	// Snapshot A pins the Given versions (system v3, TDD v2, ACTIVE_TASK v5).
	snapA, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projID.String())
	if err != nil {
		t.Fatalf("create snapshot A: %v", err)
	}

	// Prompt A at the Given versions.
	recA, err := svc.ComposePrompt(ctx, p847Req(snapA.ID, tmplID, 4))
	if err != nil {
		t.Fatalf("compose Prompt A: %v", err)
	}
	if recA.RoleVersion != 1 {
		t.Fatalf("Prompt A role version = %d, want 1 (the role ROW version — system-prompt appends do not bump it)", recA.RoleVersion)
	}
	for _, tok := range []string{p847Sys3, p847Tdd2, p847ActiveT5, p847Tmpl4} {
		if !strings.Contains(recA.Content, tok) {
			t.Fatalf("Prompt A missing Given content %q; content:\n%s", tok, recA.Content)
		}
	}
	// Prompt A persisted exactly (content retained on the record).
	var storedA string
	if err := h.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'", &storedA); err != nil {
		t.Fatalf("read back A: %v", err)
	}
	if storedA != recA.Content {
		t.Fatalf("Prompt A persisted content differs from returned record")
	}

	// Advance ALL four sources (WBS 8.48 "Then update all of them").
	p847Advance(t, h, engID, tmplID, tddID, atID)

	// Historical identity: Prompt A's persisted artifact + provenance are
	// byte-unchanged, and every pinned historical source row still carries
	// the exact content that entered Prompt A.
	var histContent, histHash string
	var histTmplVer, histRoleVer int32
	if err := h.DBScan(ctx, "SELECT content, hash, template_version, role_version FROM canonical_prompts WHERE id = '"+recA.ID.String()+"'",
		&histContent, &histHash, &histTmplVer, &histRoleVer); err != nil {
		t.Fatalf("read back A (historical): %v", err)
	}
	if histContent != recA.Content || histHash != recA.Hash {
		t.Fatalf("Prompt A not reconstructable: content/hash moved after source advances")
	}
	if int(histTmplVer) != 4 || int(histRoleVer) != 1 {
		t.Fatalf("Prompt A provenance = template v%d / role v%d, want v4 / v1", histTmplVer, histRoleVer)
	}
	// The snapshot's pinned role context (system prompt v3) + the historical
	// role_system_prompts row agree byte-for-byte.
	var roleJSON []byte
	if err := h.DBScan(ctx, "SELECT role_snapshot FROM context_snapshots WHERE id = '"+snapA.ID.String()+"'", &roleJSON); err != nil {
		t.Fatalf("read snapshot A role json: %v", err)
	}
	var roleSnap role.RoleSnapshot
	if err := json.Unmarshal(roleJSON, &roleSnap); err != nil {
		t.Fatalf("decode snapshot A role snapshot: %v", err)
	}
	if roleSnap.SystemPrompt.Version != 3 || roleSnap.SystemPrompt.Content != p847Sys3 {
		t.Fatalf("snapshot A pinned system prompt = v%d %q, want v3 %q", roleSnap.SystemPrompt.Version, roleSnap.SystemPrompt.Content, p847Sys3)
	}
	var histSys string
	if err := h.DBScan(ctx, "SELECT content FROM role_system_prompts WHERE role_id = '"+engID.String()+"' AND version = 3", &histSys); err != nil {
		t.Fatalf("read historical system prompt v3: %v", err)
	}
	if histSys != p847Sys3 {
		t.Fatalf("historical system prompt v3 = %q, want %q", histSys, p847Sys3)
	}
	// Historical TDD v2 row.
	var histTdd string
	if err := h.DBScan(ctx, "SELECT content FROM instruction_versions WHERE instruction_id = '"+tddID+"' AND version = 2", &histTdd); err != nil {
		t.Fatalf("read historical TDD v2: %v", err)
	}
	if histTdd != p847Tdd2 {
		t.Fatalf("historical TDD v2 = %q, want %q", histTdd, p847Tdd2)
	}
	// Historical ACTIVE_TASK v5 pin (the snapshot file pin row).
	var pinVer int32
	var pinContent string
	if err := h.DBScan(ctx, "SELECT version, content FROM context_snapshot_files WHERE snapshot_id = '"+snapA.ID.String()+"' AND type_key = 'ACTIVE_TASK'", &pinVer, &pinContent); err != nil {
		t.Fatalf("read snapshot A ACTIVE_TASK pin: %v", err)
	}
	if int(pinVer) != 5 || pinContent != p847ActiveT5 {
		t.Fatalf("snapshot A ACTIVE_TASK pin = v%d %q, want v5 %q", pinVer, pinContent, p847ActiveT5)
	}
	// Historical template v4 row.
	var histTmpl string
	if err := h.DBScan(ctx, "SELECT content FROM prompt_template_versions WHERE template_id = '"+tmplID.String()+"' AND version = 4", &histTmpl); err != nil {
		t.Fatalf("read historical template v4: %v", err)
	}
	if histTmpl != p847Tmpl4 {
		t.Fatalf("historical template v4 = %q, want %q", histTmpl, p847Tmpl4)
	}

	// Snapshot B pins the advanced versions; Prompt B must use the NEW
	// versions, not A's.
	snapB, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projID.String())
	if err != nil {
		t.Fatalf("create snapshot B: %v", err)
	}
	recB, err := svc.ComposePrompt(ctx, p847Req(snapB.ID, tmplID, 5))
	if err != nil {
		t.Fatalf("compose Prompt B: %v", err)
	}
	if recB.RoleVersion != 1 {
		t.Fatalf("Prompt B role version = %d, want 1 (the role ROW version — system-prompt appends do not bump it)", recB.RoleVersion)
	}
	for _, tok := range []string{p847Sys4, p847Tdd3, p847ActiveT6, p847Tmpl5} {
		if !strings.Contains(recB.Content, tok) {
			t.Fatalf("Prompt B missing advanced content %q; content:\n%s", tok, recB.Content)
		}
	}
	for _, tok := range []string{p847Sys3, p847Tdd2, p847ActiveT5, p847Tmpl4} {
		if strings.Contains(recB.Content, tok) {
			t.Fatalf("Prompt B carries Prompt A's stale content %q; content:\n%s", tok, recB.Content)
		}
	}
	if recB.Content == recA.Content || recB.Hash == recA.Hash {
		t.Fatalf("Prompt B identical to Prompt A — the new versions did not flow through")
	}
}
