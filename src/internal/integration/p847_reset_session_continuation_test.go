package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P8-46 (WBS 8.47): reset-session continuation integration test. At a
// position where the active unit is INCOMPLETE with context usage 87% against
// an 80% threshold (a reset is REQUIRED — the same-session compact path no
// longer fits), the prompt that will be sent after "new" composes correctly
// end-to-end through the P8-38 core composer over the harness pool and
// contains sufficient context to reconstruct the work WITHOUT the old
// conversation: the full WBS 8.47 checklist (ENGINEER identity, state
// instructions, project state, architecture, phase state, active task,
// acceptance criteria, required tests, captured status, remaining work,
// response protocol), and no previous chat transcript is required.
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - POSITION (WBS Given): the rig seeds the WBS position — unit 7.13 at
//     INCOMPLETE, the ENGINEER role at the IMPLEMENT state, the four required
//     state files at Given versions, a pinned response protocol, template v1,
//     a context snapshot. The WBS context/threshold values (87% usage > 80%
//     threshold) are the scenario condition that SELECTS the reset-session
//     reconstruction (the same-session compact path is only permitted when
//     usage < threshold, WBS 8.46); they are named constants below, not a
//     persisted surface.
//   - FULL RECONSTRUCTION (WBS 8.47 "without the old conversation"): the new
//     session has none of the role context / state files / work unit, so the
//     composed prompt re-composes ALL of them. The P8-38 core composer is
//     always the full composition (role snapshot, pinned state files, bound
//     work unit, continuation, pinned protocol — never "latest"), so the
//     integration pin is that every WBS 8.47 checklist item is PRESENT in the
//     composed content, with the pinned values (exact file contents at their
//     pinned versions, the unit's acceptance + required-test lists, the
//     state instruction, the protocol body + version line).
//   - CAPTURED STATUS / REMAINING WORK: the request carries a continuation
//     status validated against the snapshot's bound unit (T8.25.4). The
//     P8-38 loadContinuation captures the continued unit's IDENTITY only
//     (summary/completed/remaining/blocking empty — the same contract the
//     P8-45 same-session pin states), so the remaining/blocking sections
//     render the "(none)" placeholder; the SURFACES ("Current reported
//     status:", "remaining:", the continuation marker naming the unit) are
//     the pin, keeping it non-vacuous.
//   - NO CHAT TRANSCRIPT (WBS 8.47): the composition has no
//     conversation-history channel by construction (the canonical composer
//     consumes only snapshot-captured/pinned values — no conversation field
//     exists on CanonicalCompositionInput), and a bounded absence-scan pins
//     that no conversation-reliance phrasing is rendered (the T8.22.5 phrase
//     set, mirrored here at integration scope).
//   - DETERMINISM / NO LLM: the composer is a pure function of pinned
//     inputs; the repeated-composition byte-identity is re-asserted here for
//     the reset-session position (two compositions, identical
//     content/hash/estimate, one persisted row each).

// p847 context/threshold scenario constants (the WBS 8.47 Given). Context
// usage 87% > threshold 80% REQUIRES the reset-session reconstruction.
const (
	p847ContextUsagePercent = 87
	p847ContextThresholdPct = 80
)

// p847SystemPrompt is the ENGINEER role's captured system-prompt content (the
// ENGINEER-identity checklist item).
const p847SystemPrompt = "You are the ENGINEER role; implement the assigned work unit and report status using the required response format."

// p847ActiveTaskContent is the pinned ACTIVE_TASK content (v2).
const p847ActiveTaskContent = "ACTIVE_TASK v2: complete the implementation of the active unit, run the required tests."

// p847StateInstruction is the IMPLEMENT state's instruction content (the
// state-instructions checklist item).
const p847StateInstruction = "IMPLEMENT state instruction: implement the active work unit; report status using the required response format."

// p847ProtocolBody is the pinned response-protocol content (the implementation
// response schema body).
const p847ProtocolBody = "implementation response schema"

// p847Rig builds the WBS 8.47 position (unit 7.13 at INCOMPLETE; the ENGINEER
// role at the IMPLEMENT state; the four required state files at Given
// versions; a pinned response protocol; a template v1; a context snapshot
// bound to the unit) and returns the ids the composition request needs.
func p847Rig(t *testing.T, h *Harness) (snapID, templateID, unitID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	proj, phaseID, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	unitID = p710UnitWithSpec(t, h, planID, 1, "p847-unit-7-13", "INCOMPLETE",
		[]string{"p847 acceptance 1: all required tests pass"},
		[]string{"p847 required test 1"},
		[]string{"p847 constraint 1: no scope creep"})

	// Machine v1 with the single enabled state IMPLEMENT.
	mid := p617SeedMachine(t, h, proj.String(), "p847-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}},
		nil, "IMPLEMENT")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'IMPLEMENT'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	// ENGINEER role with the ENGINEER-identity system prompt and one assigned
	// instruction; the state's assigned role (ordinal 0).
	projStr := proj.String()
	roleSvc := service.NewRoleService(
		dbrepo.NewRoleRepository(raw),
		dbrepo.NewSystemPromptRepository(raw),
	).WithStateFiles(dbrepo.NewRoleStateFileRepository(raw))
	roleRec, err := roleSvc.CreateRole(ctx, &projStr, "P847-ENGINEER", "P847-ENGINEER", "", p847SystemPrompt)
	if err != nil {
		t.Fatalf("create engineer role: %v", err)
	}
	engID := roleRec.ID
	engStr := engID.String()
	projInstr := newInstructionRecord(t, &proj, "impl-rules", "Impl rules", "Follow the implementation rules for the active work unit.")
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, projInstr); err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	if err := p718RoleSvc(t, h).AssignInstruction(ctx, engStr, projInstr.ID.String()); err != nil {
		t.Fatalf("assign engineer instruction: %v", err)
	}
	// The ENGINEER role requires all four WBS 8.47 file categories.
	p712RequireRoleFiles(t, h, engID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: engID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	svc8 := p708Service(t, h)
	// The four required files at the Given versions.
	fPS := p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "PROJECT_STATE"))
	for v := 1; v <= 8; v++ {
		if _, err := svc8.UpdateContent(ctx, fPS.String(), "PROJECT_STATE v"+itoa(v), v-1); err != nil {
			t.Fatalf("write PROJECT_STATE v%d: %v", v, err)
		}
	}
	fArch := p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "ARCHITECTURE"))
	for v := 1; v <= 3; v++ {
		if _, err := svc8.UpdateContent(ctx, fArch.String(), "ARCHITECTURE v"+itoa(v), v-1); err != nil {
			t.Fatalf("write ARCHITECTURE v%d: %v", v, err)
		}
	}
	fPh := p713PhaseFile(t, h, proj, phaseID, p709DefaultTypeID(t, h, "PHASE_STATE"))
	for v := 1; v <= 4; v++ {
		if _, err := svc8.UpdateContent(ctx, fPh.String(), "PHASE_STATE v"+itoa(v), v-1); err != nil {
			t.Fatalf("write PHASE_STATE v%d: %v", v, err)
		}
	}
	fAT := p710ActiveTaskFile(t, h, svc8, proj, unitID)
	if _, err := svc8.UpdateContent(ctx, fAT.String(), "ACTIVE_TASK v1 placeholder", 0); err != nil {
		t.Fatalf("write ACTIVE_TASK v1: %v", err)
	}
	if _, err := svc8.UpdateContent(ctx, fAT.String(), p847ActiveTaskContent, 1); err != nil {
		t.Fatalf("write ACTIVE_TASK v2: %v", err)
	}

	// IMPLEMENT state instruction (v1).
	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, p847StateInstruction); err != nil {
		t.Fatalf("append state instruction: %v", err)
	}

	// Response protocol pinned to the IMPLEMENT state (v1).
	protocolID := p813Protocol(t, h, &proj, "p847-impl-schema", prompt.KindImplementationResponse, p847ProtocolBody)
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	// Template v1.
	templateID = p811Template(t, h, "p847-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Reset continuation: {{work_unit.task}}\nSchema: {{response_protocol.schema}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}

	// Runtime at IMPLEMENT, bound to the phase + the active (INCOMPLETE) unit.
	if _, err := p622Service(h, true).Initialize(ctx, proj.String(), mid); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"', work_unit_id = '"+unitID.String()+"' WHERE project_id = '"+proj.String()+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}

	snapSvc := p718Svc(t, h)
	snap, err := snapSvc.CreateExecutionSnapshot(ctx, proj.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return snap.ID, templateID, unitID
}

// WBS 8.47 — At the Given position (INCOMPLETE unit, context 87% > 80%
// threshold — a reset is required), compose the post-"new" prompt through the
// P8-38 core composer over the harness pool and verify the full WBS 8.47
// checklist: the work is reconstructable without the old conversation.
func TestP847ResetSessionContinuationChecklist(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	// The scenario condition selects the reset-session reconstruction (usage
	// above the threshold; the same-session compact path is no longer
	// permitted).
	if !(p847ContextUsagePercent > p847ContextThresholdPct) {
		t.Fatalf("position not a reset-session case: usage %d <= threshold %d",
			p847ContextUsagePercent, p847ContextThresholdPct)
	}
	snapID, templateID, unitID := p847Rig(t, h)
	svc := p838Composer(t, h)

	// The post-"new" prompt carries the continuation status, validated
	// against the snapshot's bound unit (T8.25.4).
	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
		ContinuationStatusID:   &unitID,
		ContinuationWorkUnitID: &unitID,
	}
	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose reset-session continuation: %v", err)
	}
	content := rec.Content

	// 1. ENGINEER identity (the captured ENGINEER system prompt).
	if !strings.Contains(content, p847SystemPrompt) {
		t.Fatalf("missing ENGINEER identity; content:\n%s", content)
	}
	// 2. State instructions (the IMPLEMENT state's instruction).
	if !strings.Contains(content, p847StateInstruction) {
		t.Fatalf("missing state instruction; content:\n%s", content)
	}
	// 3. Project state (PROJECT_STATE at its pinned version + content).
	if !strings.Contains(content, "PROJECT_STATE v8") {
		t.Fatalf("missing project state; content:\n%s", content)
	}
	// 4. Architecture.
	if !strings.Contains(content, "ARCHITECTURE v3") {
		t.Fatalf("missing architecture; content:\n%s", content)
	}
	// 5. Phase state.
	if !strings.Contains(content, "PHASE_STATE v4") {
		t.Fatalf("missing phase state; content:\n%s", content)
	}
	// 6. Active task (ACTIVE_TASK at its pinned version + content).
	if !strings.Contains(content, p847ActiveTaskContent) {
		t.Fatalf("missing active task; content:\n%s", content)
	}
	// 7. Acceptance criteria (the continued unit's captured list).
	if !strings.Contains(content, "p847 acceptance 1: all required tests pass") {
		t.Fatalf("missing acceptance criteria; content:\n%s", content)
	}
	// 8. Required tests.
	if !strings.Contains(content, "p847 required test 1") {
		t.Fatalf("missing required tests; content:\n%s", content)
	}
	// 9. Captured status (the continuation component: the continued unit
	// named + the reported-status surface).
	if !strings.Contains(content, "Continue the current work unit "+unitID.String()) {
		t.Fatalf("missing continuation marker naming the continued unit; content:\n%s", content)
	}
	if !strings.Contains(content, "Current reported status:") {
		t.Fatalf("missing captured status; content:\n%s", content)
	}
	// 10. Remaining work (the service resolves an identity-only continuation,
	// so the list renders the "(none)" placeholder — the surface is the pin).
	if !strings.Contains(content, "remaining: (none)") {
		t.Fatalf("missing remaining work; content:\n%s", content)
	}
	// 11. Response protocol (the pinned body + the version line).
	if !strings.Contains(content, p847ProtocolBody) {
		t.Fatalf("missing response protocol body; content:\n%s", content)
	}
	if !strings.Contains(content, "Response protocol v1") {
		t.Fatalf("missing response protocol version line; content:\n%s", content)
	}

	// The pinned state files are the snapshot's EXACT pins (verified against
	// the persisted pin rows, not re-derived).
	pins := p724SnapFiles(t, h, snapID)
	for key, want := range map[string]struct {
		ver     int
		content string
	}{
		"PROJECT_STATE": {8, "PROJECT_STATE v8"},
		"ARCHITECTURE":  {3, "ARCHITECTURE v3"},
		"PHASE_STATE":   {4, "PHASE_STATE v4"},
		"ACTIVE_TASK":   {2, p847ActiveTaskContent},
	} {
		got, ok := pins[key]
		if !ok {
			t.Fatalf("snapshot missing pin %s; pins: %v", key, pins)
		}
		if got.version != want.ver || got.content != want.content {
			t.Fatalf("%s pin = (%d,%q), want (%d,%q)", key, got.version, got.content, want.ver, want.content)
		}
	}
}

// WBS 8.47 — No previous chat transcript is required: the composed prompt
// carries no conversation-reliance phrasing (bounded absence-scan, the
// T8.22.5 phrase set), and the composition has no conversation-history
// channel by construction (CanonicalCompositionInput carries no such field —
// the P8-38 composer consumes only snapshot-captured/pinned values).
func TestP847NoChatTranscriptRequired(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	snapID, templateID, unitID := p847Rig(t, h)
	svc := p838Composer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
		ContinuationStatusID:   &unitID,
		ContinuationWorkUnitID: &unitID,
	}
	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose reset-session continuation: %v", err)
	}
	render := strings.ToLower(rec.Content)
	for _, phrase := range []string{
		"from the previous conversation",
		"the earlier discussion",
		"as we discussed",
		"the prior transcript",
		"recall the earlier chat",
	} {
		if strings.Contains(render, phrase) {
			t.Fatalf("conversation-reliance phrase %q present in prompt:\n%s", phrase, rec.Content)
		}
	}
	// Non-vacuity: the prompt is a full reconstruction, not a stub — it names
	// the continued unit and carries the full checklist (asserted above); the
	// absence-scan is meaningful against that full render.
	if !strings.Contains(render, unitID.String()) {
		t.Fatalf("continued unit id absent from the prompt — the absence-scan would be vacuous")
	}
}

// WBS 8.47 — No LLM call / determinism at the reset-session position: the
// composer is a pure function of pinned inputs; repeated composition over the
// same snapshot yields byte-identical content/hash/estimate (one persisted
// row per composition).
func TestP847ResetSessionDeterminism(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	snapID, templateID, unitID := p847Rig(t, h)
	svc := p838Composer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
		ContinuationStatusID:   &unitID,
		ContinuationWorkUnitID: &unitID,
	}
	first, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose #1: %v", err)
	}
	second, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose #2: %v", err)
	}
	if first.Content != second.Content {
		t.Fatalf("repeated composition content not byte-identical (an LLM/external call would perturb it)")
	}
	if first.Hash != second.Hash {
		t.Fatalf("repeated composition hash not identical")
	}
	if first.EstimatedTokens != second.EstimatedTokens {
		t.Fatalf("repeated composition estimate not identical")
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts", &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("canonical_prompts rows = %d, want 2 (one per composition — pure persist, no side channel)", n)
	}
}
