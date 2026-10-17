package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P8-38 (WBS 8.25 + 8.32 halves T8.32.1-T8.32.2): the core prompt composer
// service over real PostgreSQL. ComposePrompt resolves a pinned context
// snapshot into canonical components through the per-state primitives, renders
// the pinned template, and persists the CanonicalPrompt with exact content +
// deterministic hash + token estimate. Repeated composition over identical
// pinned inputs is byte-identical (T8.32.1/T8.32.2; the DB-row-order half is
// P8-39).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - RESPONSE PROTOCOL: the snapshot state's pinned protocol reference
//     (T8.23.3 — by protocol identity + version, never "latest") supplies the
//     protocol content at the pinned version. A state with no pinned protocol
//     is a composition failure (the P8-10/P8-22 cleared-capture contract,
//     surfaced as service.ErrInvalidInput).
//   - CONTINUATION (T8.25.4): the request's ContinuationStatusID carries the
//     captured status's work-unit association (ContinuationWorkUnitID); it is
//     validated against the snapshot's bound unit — a mismatch is the
//     cross-unit reject. A snapshot with no bound unit cannot carry a
//     continuation.
//   - SENSITIVE-DATA EXCLUSION: the composed content is built only from
//     snapshot-captured values (prompt-safe by construction — the snapshot
//     carries no secret settings), and the P8-37 registry prompt-safe surface
//     is the only template-registration path (enforced in the pure core).
//   - PERSIST: a novel canonical prompt is stored; the P8-30 dedup decision is
//     computed over the candidate (a reuse would point an invocation at an
//     existing artifact — exercised by the P8-41 history / P8-47
//     reconstruction tasks).
//   - FIXTURE: the base position (machine/role/state/files/runtime) is seeded
//     through the P7-18 rig (a valid snapshot-able position); the runtime is
//     then bound to the work unit and the unit given an objective so the P8-14
//     work-unit composition succeeds (test bookkeeping, direct SQL).

// p838Composer wires the composer service over the harness pool.
func p838Composer(t *testing.T, h *Harness) *service.PromptComposerService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewPromptComposerService(
		dbrepo.NewContextSnapshotRepository(raw),
		dbrepo.NewPromptTemplateVersionRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewResponseProtocolRepository(raw),
		dbrepo.NewStateInstructionRepository(raw),
		dbrepo.NewPromptTemplateRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewCanonicalPromptRepository(raw),
	).WithTxPool(raw)
}

// p838Fixture builds a complete composition-ready fixture and returns the ids
// needed to issue a ComposePrompt request: a valid snapshot-able position
// (p718Rig), a response protocol pinned to state S, a template with v1
// content, and a context snapshot capturing the position (created through the
// P7-18 service) bound to the project's work unit.
func p838Fixture(t *testing.T, h *Harness) (projectID, snapID, templateID, unitID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	projectID, roleID, stateID, _, _ := p718Rig(t, h)
	_ = roleID

	// The project's single work unit, with a non-empty objective (the P8-14
	// work-unit composition requires one; the p705Rig unit's objective is
	// empty, so this is test bookkeeping via direct SQL).
	unitID = p838ProjectUnit(t, h, projectID)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET objective = 'Implement the fixture work unit' WHERE id = '"+unitID.String()+"'"); err != nil {
		t.Fatalf("set unit objective: %v", err)
	}
	// Bind the runtime (and thus the snapshot) to the work unit.
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET work_unit_id = '"+unitID.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime to unit: %v", err)
	}

	// A response protocol pinned to state S (version 1).
	protocolID := p813Protocol(t, h, nil, "impl-response", prompt.KindImplementationResponse, "impl schema body")
	protoRepo := dbrepo.NewResponseProtocolRepository(raw)
	if _, err := protoRepo.CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}

	// A template with v1 content.
	templateID = p811Template(t, h, "p838-impl")
	tmplVer := dbrepo.NewPromptTemplateVersionRepository(raw)
	if _, err := tmplVer.AppendVersion(ctx, templateID, "Task: {{work_unit.task}}\nProto: {{response_protocol.schema}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}

	// Create the context snapshot through the P7-18 service.
	snapSvc := p718Svc(t, h)
	snap, err := snapSvc.CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	snapID = snap.ID
	return
}

// p838ProjectUnit returns the project's single work unit id.
func p838ProjectUnit(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var u uuid.UUID
	if err := h.DBScan(context.Background(),
		"SELECT id FROM work_units WHERE project_id = '"+projectID.String()+"' LIMIT 1", &u); err != nil {
		t.Fatalf("scan unit: %v", err)
	}
	return u
}

// p838Request builds a valid request for the fixture snapshot.
func p838Request(snapID, templateID uuid.UUID) prompt.ComposePromptRequest {
	return prompt.ComposePromptRequest{
		ContextSnapshotID: snapID,
		TemplateID:        templateID,
		TemplateVersion:   1,
	}
}

// T8.32.1/T8.32.2 — Repeated composition over identical pinned inputs yields
// byte-identical content and hash. The snapshot, template, protocol, role,
// state files, and work unit are all pinned; two compositions against the same
// snapshot produce a byte-identical CanonicalPrompt.
func TestP838RepeatedCompositionByteIdentical(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID, _ := p838Fixture(t, h)
	svc := p838Composer(t, h)

	req := p838Request(snapID, templateID)
	first, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose #1: %v", err)
	}
	second, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose #2: %v", err)
	}
	if first.Content != second.Content {
		t.Fatalf("T8.32.1 repeated composition content not byte-identical:\n#1=%q\n#2=%q", first.Content, second.Content)
	}
	if first.Hash != second.Hash {
		t.Fatalf("T8.32.2 repeated composition hash not identical: %q vs %q", first.Hash, second.Hash)
	}
	if first.EstimatedTokens != second.EstimatedTokens {
		t.Fatalf("repeated composition token estimate not identical: %d vs %d", first.EstimatedTokens, second.EstimatedTokens)
	}
	if first.Content == "" {
		t.Fatalf("composed content is empty")
	}
	if !strings.Contains(first.Content, "Task:") || !strings.Contains(first.Content, "Proto:") {
		t.Fatalf("rendered content missing template sections: %q", first.Content)
	}
	var storedContent, storedHash string
	if err := h.DBScan(ctx, "SELECT content, hash FROM canonical_prompts WHERE id = '"+first.ID.String()+"'", &storedContent, &storedHash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if storedContent != first.Content || storedHash != first.Hash {
		t.Fatalf("persisted row differs from returned record")
	}
}

// T8.25.2 (service half) — Missing snapshot rejected: a request naming an
// unknown snapshot yields service.ErrNotFound (nothing persisted).
func TestP838MissingSnapshotRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p838Composer(t, h)
	templateID := p838TemplateOnly(t, h)
	req := prompt.ComposePromptRequest{
		ContextSnapshotID: uuid.New(),
		TemplateID:        templateID,
		TemplateVersion:   1,
	}
	if _, err := svc.ComposePrompt(ctx, req); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("missing snapshot err = %v, want ErrNotFound", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected request persisted %d rows, want 0", n)
	}
}

// T8.25.3 (service half) — Invalid template rejected: a request naming a known
// snapshot but an unknown template version yields an error (nothing persisted).
func TestP838InvalidTemplateRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID, _ := p838Fixture(t, h)
	svc := p838Composer(t, h)
	req := p838Request(snapID, templateID)
	req.TemplateVersion = 99
	if _, err := svc.ComposePrompt(ctx, req); err == nil {
		t.Fatalf("invalid template version must be rejected")
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected request persisted %d rows, want 0", n)
	}
}

// T8.25.4 (service half) — Continuation status validated against the work
// unit: a continuation status captured for a DIFFERENT unit than the snapshot's
// bound unit is rejected (cross-unit), and a matching one is accepted.
func TestP838ContinuationValidatedAgainstWorkUnit(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID, unitID := p838Fixture(t, h)
	svc := p838Composer(t, h)

	matching := p838Request(snapID, templateID)
	matching.ContinuationStatusID = &unitID
	matching.ContinuationWorkUnitID = &unitID
	if _, err := svc.ComposePrompt(ctx, matching); err != nil {
		t.Fatalf("matching continuation must be accepted: %v", err)
	}

	other := uuid.New()
	cross := p838Request(snapID, templateID)
	cross.ContinuationStatusID = &unitID
	cross.ContinuationWorkUnitID = &other
	if _, err := svc.ComposePrompt(ctx, cross); err == nil {
		t.Fatalf("cross-unit continuation must be rejected")
	}
}

// p838TemplateOnly creates a template with v1 content.
func p838TemplateOnly(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	tmplID := p811Template(t, h, "p838-tpl-only")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())
	if _, err := repo.AppendVersion(context.Background(), tmplID, "x"); err != nil {
		t.Fatalf("append template: %v", err)
	}
	return tmplID
}
