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

// P8-43 (WBS 8.44): canonical PLANNING prompt integration test. At a Phase 7
// position with WBS v3, role PLANNER, state PHASE_PLAN, and atomic
// granularity, the composed prompt (through the P8-38 core composer over the
// harness pool) contains the full WBS 8.44 content checklist and no LLM call
// occurs (the composer is a pure function of pinned inputs).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - EXACT WBS: the composed content carries the phase's CURRENT WBS version
//     record content VERBATIM (the fixture advances the phase WBS to v3; the
//     v3 document text is asserted in the composed content), plus the
//     worker-visible "WBS v3" line the planning composition renders.
//   - STATE INSTRUCTION: the PHASE_PLAN state's current instruction version
//     is composed into a STATE_INSTRUCTION component (the P8-12 primitive).
//   - PLANNING POLICY + GRANULARITY: project settings supply the policy and
//     granularity verbatim (WBS 8.16: planning granularity remains project
//     configuration). The fixture configures the atomic policy verbatim.
//   - RESPONSE SCHEMA: the planning response schema (P8-23) renders through
//     the PHASE_PLAN state's pinned response-protocol reference, and the
//     required-unit-field markers of the planning schema are present.
//   - NO LLM CALL: the composer has no LLM dependency by construction; the
//     no-side-effect pin is a repeated composition over the same snapshot
//     yielding byte-identical content/hash/estimate (a second row is
//     stored — the op is pure composition + persist, nothing else).
//   - PLANNER role: the fixture's PLANNER project role (v1 system prompt +
//     one assigned instruction) is the state's assigned role (ordinal 0), so
//     the P7-18 snapshot resolves it.

// p843AtomicPolicy is the WBS 8.16 configured atomic planning policy, used
// verbatim by the fixture (the atomic-project half of the WBS 8.44 Given).
const p843AtomicPolicy = "Decompose the phase into the smallest practical independently implementable and testable work units.\nPrefer narrowly scoped functions/methods where appropriate.\nImplement/test primitives before composition units.\nEach work unit should target completion in one worker invocation.\nDeclare dependencies explicitly."

// p843WBSv3Content is the fixture's phase WBS v3 document (exact content,
// asserted in the composed prompt).
const p843WBSv3Content = "Phase WBS v3 document content (exact).\nPlan v3 unit 1: foundation primitives.\nPlan v3 unit 2: composition layer.\nPlan v3 unit 3: integration and validation."

// p843StateInstruction is the PHASE_PLAN state's instruction content.
const p843StateInstruction = "PHASE_PLAN state instruction: produce the execution plan for the phase."

// p843SchemaProtocolBody is the pinned response-protocol content the
// PHASE_PLAN state references (the execution-plan response schema body).
const p843SchemaProtocolBody = "execution-plan response schema"

// p843Rig builds the WBS 8.44 position and returns the ids the composition
// request needs.
func p843Rig(t *testing.T, h *Harness) (projectID, snapID, templateID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	proj, phaseID, _ := p705Rig(t, h)

	// Machine v1 with the single enabled state PHASE_PLAN.
	mid := p617SeedMachine(t, h, proj.String(), "p843-machine",
		[]service.MachineStateDef{{Key: "PHASE_PLAN", Kind: "ACTION"}},
		nil, "PHASE_PLAN")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'PHASE_PLAN'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	// PLANNER role with one assigned instruction; the state's assigned role
	// (ordinal 0).
	plannerID := p712ProjectRole(t, h, proj, "P843-PLANNER")
	plannerStr := plannerID.String()
	// Project-scoped instruction (same scope as the project role — a global
	// instruction would be a cross-scope assign reject).
	projInstr := newInstructionRecord(t, &proj, "plan-policy", "Plan policy", "Produce independently testable units.")
	if err := dbrepo.NewInstructionRepository(h.App.DB().Raw()).Create(ctx, projInstr); err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	roleSvc := p718RoleSvc(t, h)
	if err := roleSvc.AssignInstruction(ctx, plannerStr, projInstr.ID.String()); err != nil {
		t.Fatalf("assign planner instruction: %v", err)
	}
	// The PLANNER role requires PROJECT_STATE + ARCHITECTURE so the
	// snapshot captures them (the WBS 8.44 Files half).
	p712RequireRoleFiles(t, h, plannerID, []string{"PROJECT_STATE", "ARCHITECTURE"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: plannerID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Required PROJECT_STATE + ARCHITECTURE files with pinned content.
	typePS := p718Required(t, h, proj, "PROJECT_STATE", "PROJECT")
	typeArch := p718Required(t, h, proj, "ARCHITECTURE", "PROJECT")
	svc8 := p708Service(t, h)
	fPS := p713ProjectFile(t, h, proj, typePS)
	fArch := p713ProjectFile(t, h, proj, typeArch)
	if _, err := svc8.UpdateContent(ctx, fPS.String(), "PROJECT_STATE pinned content", 0); err != nil {
		t.Fatalf("write PROJECT_STATE: %v", err)
	}
	if _, err := svc8.UpdateContent(ctx, fArch.String(), "ARCHITECTURE pinned content", 0); err != nil {
		t.Fatalf("write ARCHITECTURE: %v", err)
	}

	// Phase WBS advanced to v3 (the WBS 8.44 Given: WBS v3).
	wbsRepo := dbrepo.NewWBSVersionRepository(raw)
	if _, err := wbsRepo.AppendVersion(ctx, phaseID.String(), "wbs v1"); err != nil {
		t.Fatalf("append wbs v1: %v", err)
	}
	if _, err := wbsRepo.AppendVersion(ctx, phaseID.String(), "wbs v2"); err != nil {
		t.Fatalf("append wbs v2: %v", err)
	}
	wbs3, err := wbsRepo.AppendVersion(ctx, phaseID.String(), p843WBSv3Content)
	if err != nil {
		t.Fatalf("append wbs v3: %v", err)
	}
	if wbs3.Version != 3 {
		t.Fatalf("wbs version = %d, want 3", wbs3.Version)
	}

	// PHASE_PLAN state instruction (v1).
	stInstr := dbrepo.NewStateInstructionRepository(raw)
	if _, err := stInstr.AppendVersion(ctx, stateID, p843StateInstruction); err != nil {
		t.Fatalf("append state instruction: %v", err)
	}

	// Project settings: the atomic planning policy + granularity.
	p843SetPlanningSettings(t, h, proj, p843AtomicPolicy, "atomic")

	// Response protocol pinned to the PHASE_PLAN state (v1) — the execution-
	// plan response schema body the planning composition renders.
	protocolID := p813Protocol(t, h, &proj, "p843-plan-schema", prompt.KindPlanResponse, p843SchemaProtocolBody)
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	// Template v1 (the planning template source).
	templateID = p811Template(t, h, "p843-plan")
	tmplVer := dbrepo.NewPromptTemplateVersionRepository(raw)
	if _, err := tmplVer.AppendVersion(ctx, templateID, "Plan: {{phase.context}}\nSchema: {{response_protocol.schema}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}

	// Context snapshot through the P7-18 service (resolves PLANNER via the
	// state assignment, the required files, and the runtime position).
	rt := dbrepo.NewRuntimeStateRepository(raw)
	if err := rt.Create(ctx, &persistence.RuntimeStateRecord{
		ID: uuid.New(), ProjectID: proj,
		StateMachineID: uuid.MustParse(mid), StateMachineVersion: 1,
		CurrentStateID: stateID, PhaseID: &phaseID, WorkUnitID: nil,
		EnteredAt: time.Now().UTC(), Version: 1,
	}); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	snapSvc := p718Svc(t, h)
	snap, err := snapSvc.CreateExecutionSnapshot(ctx, proj.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return proj, snap.ID, templateID
}

// p843PlanningComposer wires the composer with the WBS-version + settings
// repositories the PHASE_PLAN planning composition needs.
func p843PlanningComposer(t *testing.T, h *Harness) *service.PromptComposerService {
	t.Helper()
	raw := h.App.DB().Raw()
	return p838Composer(t, h).
		WithWBSVersions(dbrepo.NewWBSVersionRepository(raw)).
		WithSettings(dbrepo.NewSettingsRepository(raw))
}

// p843SetPlanningSettings drives the project's settings to carry the given
// planning policy + granularity (direct settings repository update — the
// WBS 8.44 fixture bookkeeping).
func p843SetPlanningSettings(t *testing.T, h *Harness, projectID uuid.UUID, policy, granularity string) {
	t.Helper()
	ctx := context.Background()
	setRepo := dbrepo.NewSettingsRepository(h.App.DB().Raw())
	cur, err := setRepo.Get(ctx, projectID.String())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	upd := *cur
	upd.PlanningPolicy = policy
	upd.PlanningGranularity = granularity
	if err := setRepo.Update(ctx, &upd); err != nil {
		t.Fatalf("update settings: %v", err)
	}
}

// WBS 8.44 — Compose the planning prompt at the Given position and verify
// the full content checklist.
func TestP843CanonicalPlanningPromptContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID := p843Rig(t, h)
	svc := p843PlanningComposer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
	}
	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose planning prompt: %v", err)
	}
	content := rec.Content

	// The PLANNER role instruction (assigned in the fixture).
	if !strings.Contains(content, "Produce independently testable units.") {
		t.Fatalf("missing PLANNER instruction; content:\n%s", content)
	}
	// The PHASE_PLAN state instruction.
	if !strings.Contains(content, p843StateInstruction) {
		t.Fatalf("missing PHASE_PLAN state instruction; content:\n%s", content)
	}
	// Project context + architecture (the resolved state files, exact
	// pinned content).
	if !strings.Contains(content, "PROJECT_STATE pinned content") {
		t.Fatalf("missing project context; content:\n%s", content)
	}
	if !strings.Contains(content, "ARCHITECTURE pinned content") {
		t.Fatalf("missing architecture; content:\n%s", content)
	}
	// The EXACT WBS v3 (verbatim document content + the worker-visible
	// version line).
	if !strings.Contains(content, p843WBSv3Content) {
		t.Fatalf("missing exact WBS v3 content; content:\n%s", content)
	}
	if !strings.Contains(content, "WBS v3") {
		t.Fatalf("missing WBS v3 version line; content:\n%s", content)
	}
	// The atomic planning policy (verbatim) + the atomic granularity.
	if !strings.Contains(content, p843AtomicPolicy) {
		t.Fatalf("missing atomic planning policy; content:\n%s", content)
	}
	if !strings.Contains(content, "atomic") {
		t.Fatalf("missing atomic granularity; content:\n%s", content)
	}
	// The execution-plan response schema (the pinned protocol body + the
	// planning schema's required-unit-field markers).
	if !strings.Contains(content, p843SchemaProtocolBody) {
		t.Fatalf("missing execution-plan response schema body; content:\n%s", content)
	}
	if !strings.Contains(content, "acceptance") || !strings.Contains(content, "objective") {
		t.Fatalf("missing planning schema unit fields; content:\n%s", content)
	}
	// Persisted exactly (WBS 8.26.5 half — content retained).
	var stored string
	if err := h.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != rec.Content {
		t.Fatalf("persisted content differs from returned record")
	}
}

// WBS 8.44 — Verify no LLM call occurs: the composer is a pure function of
// pinned inputs. Repeated composition over the same snapshot yields
// byte-identical content/hash/estimate (a second row is stored — the op is
// composition + persist only).
func TestP843NoLLMCall(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID := p843Rig(t, h)
	svc := p843PlanningComposer(t, h)

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
