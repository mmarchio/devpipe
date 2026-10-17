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
	"overseer/internal/role"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P8-45 (WBS 8.46): same-session continuation integration test. At a position
// where the active unit is INCOMPLETE with context usage 55% against an 80%
// threshold, the SAME-SESSION continuation prompt (the compact CONTINUE path,
// WBS 8.21) composes correctly end-to-end through the P8-38 core composer over
// the harness pool and emphasizes continuing the current work: captured
// status, remaining work, blocking information, and the required response.
// The TDD half verifies it is significantly smaller than a full reset-session
// reconstruction (WBS 8.22) when the configuration permits the compact path,
// comparing token estimates WITHOUT pinning exact token savings.
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - POSITION (WBS Given): the rig seeds an active work unit at INCOMPLETE
//     (unit 7.13), the ENGINEER role at the IMPLEMENT state, the four required
//     state files at Given versions, a pinned response protocol, and a template
//     v1. The runtime binds the unit, and a context snapshot captures the
//     position. The WBS context/threshold values (55% usage < 80% threshold)
//     are the scenario condition that SELECTS the same-session compact path
//     (a reset-session reconstruction is only required when usage >= threshold,
//     WBS 8.47); they are carried as named constants below and are not a
//     persisted surface — the composer selects the path from the request.
//   - EMHASIS (WBS 8.46 checklist): the P8-38 composer, given a continuation
//     status validated against the snapshot's bound unit (T8.25.4), renders a
//     CONTINUATION_CONTEXT component whose surfaces ARE the five emphasis
//     items: "Continue the current work unit <id>" (continue-current-work),
//     "Current reported status:" (captured status), the "remaining:" section
//     (remaining work), the "blocking:" section (blocking information), and
//     the fixed "Complete the remaining acceptance criteria and report status
//     using the required response format" line plus the P8-22 response
//     protocol (required response). The continuation status the service
//     resolves carries the continued unit's identity; its summary/completed/
//     remaining/blocking lists are empty (the P8-38 loadContinuation captures
//     identity only), so the remaining/blocking sections render the "(none)"
//     placeholder — the SURFACES are the emphasis, and each is asserted
//     present, keeping the pin non-vacuous.
//   - SIZE (WBS 8.46 TDD, non-brittle): the same-session compact prompt is the
//     P8-21 ComposeContinuePrompt (continuation + response schema ONLY — the
//     live session already has the role context / state files / work unit),
//     while the reset-session reconstruction is the P8-29
//     ComposeResetSessionPrompt (full role context + state files + work unit +
//     continuation + schema). Both are composed over the SAME captured values
//     (identical status, protocol, role, files, and unit), so the comparison
//     isolates the composition's size, not its inputs. The assertion is a
//     robust inequality with margin (same-session strictly smaller AND below
//     a fixed fraction of the reset total) — NOT an exact token-savings figure
//     (WBS: "Do not make exact token savings a brittle test").
//   - DETERMINISM / NO LLM: both primitives are pure functions of their
//     captured inputs; the repeated-composition byte-identity of the P8-38
//     composer is pinned by the P8-38/P8-44 suite, so this task reuses that
//     guarantee rather than re-deriving it.

// p846 context/threshold scenario constants (the WBS 8.46 Given). Context
// usage 55% < threshold 80% permits the SAME-SESSION compact continuation.
const (
	p846ContextUsagePercent = 55
	p846ContextThresholdPct = 80
)

// p846ContSummary / remaining / blocking are the captured worker status values
// the size-comparison half renders (substantive, so the compact prompt is
// non-trivially sized). They mirror the WBS position: the unit is INCOMPLETE,
// mid-implementation.
const (
	p846Summary    = "Implementation partially complete; repository appends pending"
	p846Remaining1 = "append repository"
	p846Remaining2 = "advisory-locked MAX+1"
	p846NextAction = "Implement the append repository"
)

// p846ActiveTaskContent is the pinned ACTIVE_TASK content (v2).
const p846ActiveTaskContent = "ACTIVE_TASK v2: complete the implementation of the active unit, run the required tests."

// p846StateInstruction is the IMPLEMENT state's instruction content.
const p846StateInstruction = "IMPLEMENT state instruction: implement the active work unit; report status using the required response format."

// p846ProtocolBody is the pinned response-protocol content (the
// implementation response schema body).
const p846ProtocolBody = "implementation response schema"

// p846Rig builds the WBS 8.46 position (an active unit at INCOMPLETE; the
// ENGINEER role at the IMPLEMENT state; the four required state files at Given
// versions; a pinned response protocol; a template v1) and returns the ids the
// composition request needs.
func p846Rig(t *testing.T, h *Harness) (snapID, templateID, unitID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	proj, phaseID, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	unitID = p710UnitWithSpec(t, h, planID, 1, "p846-unit-7-13", "INCOMPLETE",
		[]string{"p846 acceptance 1: all required tests pass"},
		[]string{"p846 required test 1"},
		[]string{"p846 constraint 1: no scope creep"})

	// Machine v1 with the single enabled state IMPLEMENT.
	mid := p617SeedMachine(t, h, proj.String(), "p846-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}},
		nil, "IMPLEMENT")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'IMPLEMENT'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	// ENGINEER role with one assigned instruction; the state's assigned role
	// (ordinal 0).
	engID := p712ProjectRole(t, h, proj, "P846-ENGINEER")
	engStr := engID.String()
	projInstr := newInstructionRecord(t, &proj, "impl-rules", "Impl rules", "Follow the implementation rules for the active work unit.")
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, projInstr); err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	roleSvc := p718RoleSvc(t, h)
	if err := roleSvc.AssignInstruction(ctx, engStr, projInstr.ID.String()); err != nil {
		t.Fatalf("assign engineer instruction: %v", err)
	}
	// The ENGINEER role requires all four WBS 8.46 file categories.
	p712RequireRoleFiles(t, h, engID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})
	asgns := dbrepo.NewStateRoleAssignmentRepository(raw)
	if err := asgns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: engID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	svc8 := p708Service(t, h)
	// The four required files at the WBS 8.46 Given versions.
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
	if _, err := svc8.UpdateContent(ctx, fAT.String(), p846ActiveTaskContent, 1); err != nil {
		t.Fatalf("write ACTIVE_TASK v2: %v", err)
	}

	// IMPLEMENT state instruction (v1).
	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, p846StateInstruction); err != nil {
		t.Fatalf("append state instruction: %v", err)
	}

	// Response protocol pinned to the IMPLEMENT state (v1).
	protocolID := p813Protocol(t, h, &proj, "p846-impl-schema", prompt.KindImplementationResponse, p846ProtocolBody)
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	// Template v1.
	templateID = p811Template(t, h, "p846-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Continue: {{work_unit.task}}\nSchema: {{response_protocol.schema}}"); err != nil {
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

// WBS 8.46 — Compose the same-session continuation at the Given position
// (INCOMPLETE unit, context 55% < 80% threshold) through the P8-38 composer
// over the harness pool and verify the five emphasis items.
func TestP846SameSessionContinuationEmphasis(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	// The scenario condition selects the same-session compact path.
	if !(p846ContextUsagePercent < p846ContextThresholdPct) {
		t.Fatalf("position not a same-session case: usage %d >= threshold %d",
			p846ContextUsagePercent, p846ContextThresholdPct)
	}
	snapID, templateID, unitID := p846Rig(t, h)
	svc := p838Composer(t, h)

	req := prompt.ComposePromptRequest{
		ContextSnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1,
		ContinuationStatusID:   &unitID,
		ContinuationWorkUnitID: &unitID,
	}
	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose same-session continuation: %v", err)
	}
	content := rec.Content

	// 1. Continue current work.
	if !strings.Contains(content, "Continue the current work unit "+unitID.String()) {
		t.Fatalf("missing continue-current-work emphasis; content:\n%s", content)
	}
	// 2. Captured status.
	if !strings.Contains(content, "Current reported status:") {
		t.Fatalf("missing captured-status emphasis; content:\n%s", content)
	}
	// 3. Remaining work (the service resolves an identity-only continuation,
	// so the list renders the "(none)" placeholder — the surface is the pin).
	if !strings.Contains(content, "remaining: (none)") {
		t.Fatalf("missing remaining-work emphasis; content:\n%s", content)
	}
	// 4. Blocking information.
	if !strings.Contains(content, "blocking: (none)") {
		t.Fatalf("missing blocking-information emphasis; content:\n%s", content)
	}
	// 5. Required response (the fixed response-format line + the response
	// protocol the continuing worker reports through).
	if !strings.Contains(content, "report status using the required response format") {
		t.Fatalf("missing required-response emphasis; content:\n%s", content)
	}
	if !strings.Contains(content, p846ProtocolBody) {
		t.Fatalf("missing response protocol body; content:\n%s", content)
	}
}

// p846SharedCaptured returns the captured values fed to BOTH the same-session
// and reset-session compositions, so the size comparison isolates the
// composition's shape, not its inputs.
func p846SharedCaptured(unitID uuid.UUID) (prompt.ContinuationStatus, role.RoleSnapshot, []statefile.StateFileSnapshot, []string, prompt.WorkUnitContextInput) {
	status := prompt.ContinuationStatus{
		WorkUnitID: unitID,
		Summary:    p846Summary,
		Completed:  []string{"domain types", "schema migration"},
		Remaining:  []string{p846Remaining1, p846Remaining2},
		Blocking:   []string{"none"},
		NextAction: p846NextAction,
		CapturedAt: time.Unix(1, 0).UTC(),
	}
	roleSnap := role.RoleSnapshot{
		RoleID:  uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-role")),
		RoleKey: "ENGINEER",
		SystemPrompt: role.PromptSnapshot{
			Version: 4,
			Content: "You are the engineer role; implement the assigned work unit and report status through the implementation protocol.",
		},
		Instructions: []role.InstructionSnapshot{
			{Key: "TDD", Name: "TDD", Version: 2, Content: "Write the failing test first, then implement. Keep scope to the active work unit."},
			{Key: "RULES", Name: "Rules", Version: 1, Content: "Follow the project rules: deterministic output, explicit errors, no scope creep."},
		},
	}
	files := []statefile.StateFileSnapshot{
		{StateFileID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-f-ps")), TypeKey: "PROJECT_STATE", Version: 8,
			Hash: "ps-hash", Content: "PROJECT_STATE v8: the project's running state, module layout, and phase progression.", Classification: role.ClassificationProjectContext},
		{StateFileID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-f-arch")), TypeKey: "ARCHITECTURE", Version: 3,
			Hash: "arch-hash", Content: "ARCHITECTURE v3: the system architecture, service boundaries, and persistence contracts.", Classification: role.ClassificationProjectContext},
		{StateFileID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-f-ph")), TypeKey: "PHASE_STATE", Version: 4,
			Hash: "ph-hash", Content: "PHASE_STATE v4: the current phase's state, its work plan, and completion criteria.", Classification: role.ClassificationPhaseContext},
	}
	required := []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"}
	unit := prompt.WorkUnitContextInput{
		ID:        unitID,
		Name:      "p846-unit-7-13",
		Kind:      "ATOMIC",
		Objective: "Provide advisory-locked WBS version appends for the active phase.",
		Acceptance: []string{
			"the append op is advisory-locked and recomputes on conflict",
			"repeated appends yield a contiguous version sequence",
			"all required tests pass under the plain and -race gates",
		},
		RequiredTests: []string{
			"the append service test",
			"the concurrency test over real PostgreSQL",
			"the restart-reconstruction test",
		},
		Constraints: []string{"no scope creep; only the active unit"},
	}
	return status, roleSnap, files, required, unit
}

// WBS 8.46 TDD — The same-session continuation is significantly smaller than a
// full reset-session reconstruction when the configuration permits the compact
// path. Both compositions consume the SAME captured values; the size difference
// is asserted with a robust margin, not an exact token-savings figure.
func TestP846SameSessionSmallerThanResetSession(t *testing.T) {
	if !(p846ContextUsagePercent < p846ContextThresholdPct) {
		t.Fatalf("position not a same-session case: usage %d >= threshold %d",
			p846ContextUsagePercent, p846ContextThresholdPct)
	}
	unitID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-size-unit"))
	status, roleSnap, files, required, unit := p846SharedCaptured(unitID)
	protocolID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("p846-size-protocol"))
	allowed := []string{"DONE", "INCOMPLETE", "BLOCKED"}

	// Same-session compact (WBS 8.21): continuation + response schema only.
	sameComps, err := prompt.ComposeContinuePrompt(prompt.ContinuePromptInput{
		Status: status, ProtocolID: protocolID, ProtocolVersion: 1, AllowedStatuses: allowed,
	})
	if err != nil {
		t.Fatalf("compose same-session: %v", err)
	}
	// Full reset-session reconstruction (WBS 8.22): full role context + files +
	// work unit + continuation + schema.
	resetComps, err := prompt.ComposeResetSessionPrompt(prompt.ResetSessionPromptInput{
		Role: roleSnap, StateFiles: files, Required: required, ActiveUnit: unit,
		Status: status, ProtocolID: protocolID, ProtocolVersion: 1, AllowedStatuses: allowed,
	})
	if err != nil {
		t.Fatalf("compose reset-session: %v", err)
	}

	sameTok := prompt.EstimateTokens(sameComps).Total
	resetTok := prompt.EstimateTokens(resetComps).Total
	if sameTok <= 0 || resetTok <= 0 {
		t.Fatalf("non-positive token estimates: same=%d reset=%d", sameTok, resetTok)
	}
	// Strictly smaller (the load-bearing half: the compact path omits the full
	// role context / state files / work unit that the reconstruction restores).
	if sameTok >= resetTok {
		t.Fatalf("same-session not smaller than reset-session: same=%d reset=%d", sameTok, resetTok)
	}
	// Significantly smaller, with a robust margin (same < 75% of the reset
	// total) — non-brittle; no exact token-savings figure is pinned.
	if sameTok*4 >= resetTok*3 {
		t.Fatalf("same-session not significantly smaller (margin): same=%d reset=%d (same*4=%d >= reset*3=%d)",
			sameTok, resetTok, sameTok*4, resetTok*3)
	}
	// The compact prompt is not vacuously small: it still carries the
	// continuation component and the response schema (the two components the
	// WBS 8.21 minimal set requires).
	if len(sameComps) < 2 {
		t.Fatalf("same-session composition too small: %d components", len(sameComps))
	}
}
