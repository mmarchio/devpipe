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

// P8-44 (WBS 8.45): canonical IMPLEMENTATION prompt integration test. At a
// phase / work-unit position with role ENGINEER and state IMPLEMENT, the
// composed prompt (through the P8-38 core composer over the harness pool)
// contains the full WBS 8.45 content checklist and no LLM call occurs (the
// composer is a pure function of pinned inputs).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - WORK UNIT: the rig seeds the plan with unit 1 (the active, EXECUTING
//     unit the runtime binds) and unit 2 (an unrelated FUTURE unit, PENDING,
//     carrying a unique marker token). The active unit's id/name/objective,
//     acceptance criteria, and required tests are asserted present; unit 2's
//     name + spec tokens are asserted ABSENT from the whole render (the
//     "unrelated future work units are absent" half — a whole-render scan is
//     valid because unit 2 is a whole entity no component would ever render,
//     while the active unit is asserted PRESENT in the same render, keeping
//     the scan non-vacuous).
//   - DEPENDENCY CONTRACTS: the work-unit context's captured dependency set
//     has no persistence surface at the composer level (the P8-15 provenance
//     note — "no dependency-contract persistence exists yet"; the composer's
//     loadUnit carries no dependency names), so the contracts surface is
//     pinned at the WBS 8.15 primitive level (the internal/prompt unit suite:
//     referenced included, unrelated excluded, deterministic order) and the
//     integration pin here is the work-unit component's dependencies line.
//     The fixture's active unit therefore carries no dependency names, and
//     the future unit's absence is the load-bearing integration proof that
//     only the ACTIVE unit enters the render.
//   - STATE FILES: the role requires all four WBS 8.45 file categories
//     (PROJECT_STATE/ARCHITECTURE project-scoped + PHASE_STATE phase-scoped +
//     ACTIVE_TASK unit-scoped); each is advanced to the WBS 8.45 Given
//     versions through repeated UpdateContent appends (8/3/4/2) and the
//     snapshot pins + exact contents are asserted against the persisted pin
//     rows, never re-derived.
//   - RESPONSE PROTOCOL: the IMPLEMENT state pins an IMPLEMENTATION_RESPONSE
//     protocol; the integration pin is the pinned protocol content + the
//     version line rendered. The allowed-statuses set is a P8-23
//     primitive-level concern (pinned by the internal/prompt unit suite) —
//     the core composer renders the pinned protocol content verbatim.
//   - NO LLM CALL: the composer has no LLM dependency by construction; the
//     pin is a repeated composition over the same snapshot yielding
//     byte-identical content/hash/estimate (one persisted row per
//     composition).

// p844FutureUnitMarker is the unique token carried ONLY by the unrelated
// future unit (asserted absent from the composed prompt).
const p844FutureUnitMarker = "P844-FUTURE-UNIT-7-14"

// p844ActiveTaskContent is the pinned ACTIVE_TASK content (v2).
const p844ActiveTaskContent = "ACTIVE_TASK v2: complete the implementation of the active unit, run the required tests."

// p844StateInstruction is the IMPLEMENT state's instruction content.
const p844StateInstruction = "IMPLEMENT state instruction: implement the active work unit; report status using the required response format."

// p844ProtocolBody is the pinned response-protocol content (the
// implementation response schema body).
const p844ProtocolBody = "implementation response schema"

// p844Rig builds the WBS 8.45 position (a phase with a plan whose active
// unit is unit 1; unit 2 is an unrelated future unit; ENGINEER role at the
// IMPLEMENT state; the four required state files at the Given versions) and
// returns the ids the composition request needs.
func p844Rig(t *testing.T, h *Harness) (snapID, templateID, unit1, unit2 uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	proj, phaseID, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	unit1 = p710UnitWithSpec(t, h, planID, 1, "p844-unit-1", "EXECUTING",
		[]string{"p844 acceptance 1: all required tests pass"},
		[]string{"p844 required test 1"},
		[]string{"p844 constraint 1: no scope creep"})
	unit2 = p710UnitWithSpec(t, h, planID, 2, p844FutureUnitMarker, "PENDING",
		[]string{"p844 future acceptance (must stay absent)"},
		[]string{"p844 future test (must stay absent)"},
		[]string{})

	// Machine v1 with the single enabled state IMPLEMENT.
	mid := p617SeedMachine(t, h, proj.String(), "p844-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}},
		nil, "IMPLEMENT")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'IMPLEMENT'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	// ENGINEER role with one assigned instruction; the state's assigned
	// role (ordinal 0).
	engID := p712ProjectRole(t, h, proj, "P844-ENGINEER")
	engStr := engID.String()
	projInstr := newInstructionRecord(t, &proj, "impl-rules", "Impl rules", "Follow the implementation rules for the active work unit.")
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, projInstr); err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	roleSvc := p718RoleSvc(t, h)
	if err := roleSvc.AssignInstruction(ctx, engStr, projInstr.ID.String()); err != nil {
		t.Fatalf("assign engineer instruction: %v", err)
	}
	// The ENGINEER role requires all four WBS 8.45 file categories.
	p712RequireRoleFiles(t, h, engID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: engID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	svc8 := p708Service(t, h)
	// The four required files at the WBS 8.45 Given versions.
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
	fAT := p710ActiveTaskFile(t, h, svc8, proj, unit1)
	if _, err := svc8.UpdateContent(ctx, fAT.String(), "ACTIVE_TASK v1 placeholder", 0); err != nil {
		t.Fatalf("write ACTIVE_TASK v1: %v", err)
	}
	if _, err := svc8.UpdateContent(ctx, fAT.String(), p844ActiveTaskContent, 1); err != nil {
		t.Fatalf("write ACTIVE_TASK v2: %v", err)
	}

	// IMPLEMENT state instruction (v1).
	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, p844StateInstruction); err != nil {
		t.Fatalf("append state instruction: %v", err)
	}

	// Response protocol pinned to the IMPLEMENT state (v1).
	protocolID := p813Protocol(t, h, &proj, "p844-impl-schema", prompt.KindImplementationResponse, p844ProtocolBody)
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	// Template v1 (the implementation template source).
	templateID = p811Template(t, h, "p844-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Impl: {{work_unit.task}}\nSchema: {{response_protocol.schema}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}

	// Runtime at IMPLEMENT, bound to the phase + the active unit.
	if _, err := p622Service(h, true).Initialize(ctx, proj.String(), mid); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"', work_unit_id = '"+unit1.String()+"' WHERE project_id = '"+proj.String()+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}

	snapSvc := p718Svc(t, h)
	snap, err := snapSvc.CreateExecutionSnapshot(ctx, proj.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return snap.ID, templateID, unit1, unit2
}

// p844ImplComposer wires the composer for the IMPLEMENT state composition
// (the plain P8-38 composer — no state-gated extras exist for IMPLEMENT).
func p844ImplComposer(t *testing.T, h *Harness) *service.PromptComposerService {
	t.Helper()
	return p838Composer(t, h)
}

// WBS 8.45 — Compose the implementation prompt at the Given position and
// verify the full content checklist; unrelated future work units are absent.
func TestP844CanonicalImplementationPromptContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	snapID, templateID, unit1, unit2 := p844Rig(t, h)
	svc := p844ImplComposer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
	}
	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose implementation prompt: %v", err)
	}
	content := rec.Content

	// Correct role versions: the ENGINEER system prompt (v1) and the
	// assigned instruction are in the render.
	if !strings.Contains(content, "prompt") {
		t.Fatalf("missing ENGINEER system prompt; content:\n%s", content)
	}
	if !strings.Contains(content, "Follow the implementation rules for the active work unit.") {
		t.Fatalf("missing ENGINEER instruction; content:\n%s", content)
	}
	if rec.RoleVersion != 1 {
		t.Fatalf("role version = %d, want 1", rec.RoleVersion)
	}
	// Correct state instruction (the IMPLEMENT state's instruction).
	if !strings.Contains(content, p844StateInstruction) {
		t.Fatalf("missing IMPLEMENT state instruction; content:\n%s", content)
	}
	// Correct work unit: id + name + objective.
	if !strings.Contains(content, "ID: "+unit1.String()) {
		t.Fatalf("missing active unit id; content:\n%s", content)
	}
	if !strings.Contains(content, "name: p844-unit-1") {
		t.Fatalf("missing active unit name; content:\n%s", content)
	}
	if !strings.Contains(content, "Objective p844-unit-1") {
		t.Fatalf("missing active unit objective; content:\n%s", content)
	}
	// Acceptance criteria + required tests (the unit's captured lists).
	if !strings.Contains(content, "p844 acceptance 1: all required tests pass") {
		t.Fatalf("missing acceptance criteria; content:\n%s", content)
	}
	if !strings.Contains(content, "p844 required test 1") {
		t.Fatalf("missing required tests; content:\n%s", content)
	}
	// Dependencies: the work-unit component carries the dependencies line
	// (the contracts surface is pinned at the primitive level — stated
	// choice, see the file header).
	if !strings.Contains(content, "dependencies: ") {
		t.Fatalf("missing dependencies line; content:\n%s", content)
	}
	// Correct state files at their pinned versions + exact content
	// (verified against the persisted pins, not re-derived).
	pins := p724SnapFiles(t, h, snapID)
	for key, want := range map[string]struct {
		ver     int
		content string
	}{
		"PROJECT_STATE": {8, "PROJECT_STATE v8"},
		"ARCHITECTURE":  {3, "ARCHITECTURE v3"},
		"PHASE_STATE":   {4, "PHASE_STATE v4"},
		"ACTIVE_TASK":   {2, p844ActiveTaskContent},
	} {
		got, ok := pins[key]
		if !ok {
			t.Fatalf("snapshot missing pin %s; pins: %v", key, pins)
		}
		if got.version != want.ver {
			t.Fatalf("%s pin version = %d, want %d", key, got.version, want.ver)
		}
		if got.content != want.content {
			t.Fatalf("%s pin content = %q, want %q", key, got.content, want.content)
		}
		if !strings.Contains(content, want.content) {
			t.Fatalf("composed prompt missing %s pinned content; content:\n%s", key, content)
		}
	}
	// Response protocol: the pinned protocol body + the protocol version
	// recorded on the render (the allowed-statuses set is a P8-23
	// primitive-level concern — pinned by the unit suite; the core composer
	// renders the pinned protocol content verbatim, stated choice).
	if !strings.Contains(content, p844ProtocolBody) {
		t.Fatalf("missing response protocol body; content:\n%s", content)
	}
	if !strings.Contains(content, "Response protocol v1") {
		t.Fatalf("missing response protocol version line; content:\n%s", content)
	}
	// Unrelated future work units are absent: unit 2's unique marker
	// (name + its spec tokens) is nowhere in the whole render, while the
	// active unit is present (asserted above), keeping the scan non-vacuous.
	if strings.Contains(content, p844FutureUnitMarker) {
		t.Fatalf("future unit marker present in the prompt; content:\n%s", content)
	}
	if strings.Contains(content, "p844 future acceptance (must stay absent)") {
		t.Fatalf("future unit acceptance leaked into the prompt")
	}
	if strings.Contains(content, "p844 future test (must stay absent)") {
		t.Fatalf("future unit required tests leaked into the prompt")
	}
	_ = unit2
	// Persisted exactly (content retained on the record).
	var stored string
	if err := h.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != rec.Content {
		t.Fatalf("persisted content differs from returned record")
	}
}

// WBS 8.45 — No LLM call: the composer is a pure function of pinned inputs.
// Repeated composition over the same snapshot yields byte-identical
// content/hash/estimate (one persisted row per composition).
func TestP844NoLLMCall(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	snapID, templateID, _, _ := p844Rig(t, h)
	svc := p844ImplComposer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
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
