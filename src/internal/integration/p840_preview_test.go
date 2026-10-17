package integration

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P8-40 (WBS 8.35): the prompt-preview op + REST endpoint.
//
// The preview renders the canonical prompt for the project's CURRENT runtime
// position WITHOUT persisting a CanonicalPrompt row and WITHOUT calling an
// LLM, returning the WBS 8.35 metadata (state, role, template, estimated
// tokens, component sizes, snapshot hash, prompt hash) and identifying any
// required content absent at the position (T8.35.4, non-fatal).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - TEMPLATE SELECTION: the request pins the (template_id, version); the
//     fixture pins the p838 template at version 1 (the p718Rig state carries
//     no prompt-template key, so the state-referenced default is not used).
//   - MISSING REQUIREMENT (T8.35.4): a required state-file type with no file
//     at its scope is REPORTED in MissingRequired and the preview SUCCEEDS
//     (the distinguishing property over the P7-18 creation op, which fails).
//   - SNAPSHOT HASH: derived deterministically from the resolved file pins
//     (sorted by type key) — stable over an unchanged position (T8.35.5).
//
// The fixture reuses p838Fixture (a complete composition-ready position:
// machine + state S, role, files A/B, protocol pinned to S, template, runtime
// bound to the work unit). The REST test drives the live HTTP surface for the
// 200/404/405 contract.

// p840PreviewSvc wires the prompt-preview service over the harness pool (the
// same composition the app wires in production).
func p840PreviewSvc(t *testing.T, h *Harness) *service.PromptPreviewService {
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
	return service.NewPromptPreviewService(
		dbrepo.NewRuntimeStateRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateRoleAssignmentRepository(raw),
		dbrepo.NewRoleRepository(raw),
		p718RoleSvc(t, h),
		scopeRes,
		dbrepo.NewStateFileVersionRepository(raw),
		dbrepo.NewPromptTemplateVersionRepository(raw),
		dbrepo.NewPromptTemplateRepository(raw),
		dbrepo.NewResponseProtocolRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
		dbrepo.NewPlanRepository(raw),
	).WithTxPool(raw)
}

// p840PreviewReq pins the fixture template at version 1.
func p840PreviewReq(templateID string) service.PromptPreviewRequest {
	id, _ := uuid.Parse(templateID)
	v := 1
	return service.PromptPreviewRequest{TemplateID: &id, TemplateVersion: &v}
}

// T8.35.1 — Preview renders the canonical prompt: non-empty content carrying
// the template's sections, with the metadata envelope (state, role,
// template) and a non-empty hash + positive token estimate.
func TestP840PreviewRendersPrompt(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	svc := p840PreviewSvc(t, h)

	out, err := svc.Preview(ctx, projectID.String(), p840PreviewReq(templateID.String()))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if out.Content == "" {
		t.Fatalf("preview content is empty")
	}
	for _, marker := range []string{"Task:", "Proto:"} {
		if !strings.Contains(out.Content, marker) {
			t.Fatalf("rendered content missing %q:\n%s", marker, out.Content)
		}
	}
	if out.StateKey != "S" {
		t.Fatalf("state = %q, want S", out.StateKey)
	}
	if out.RoleKey == "" || out.Hash == "" || out.SnapshotHash == "" {
		t.Fatalf("metadata incomplete: role=%q template=%s hash=%q snapHash=%q",
			out.RoleKey, out.TemplateID, out.Hash, out.SnapshotHash)
	}
	if out.EstimatedTokens <= 0 {
		t.Fatalf("estimated tokens = %d, want > 0", out.EstimatedTokens)
	}
	if len(out.Components) == 0 {
		t.Fatalf("no component sizes reported")
	}
}

// T8.35.2 — No LLM called and no CanonicalPrompt row persisted: after three
// previews the canonical_prompts table holds zero rows (direct SQL).
func TestP840PreviewPersistsNothing(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	svc := p840PreviewSvc(t, h)
	for i := 0; i < 3; i++ {
		if _, err := svc.Preview(ctx, projectID.String(), p840PreviewReq(templateID.String())); err != nil {
			t.Fatalf("Preview: %v", err)
		}
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("preview persisted %d canonical_prompts row(s), want 0", n)
	}
}

// T8.35.3 — Runtime state unchanged: the runtime row (state, version) + the
// transition-history signature are byte-identical after the preview (direct
// SQL).
func TestP840PreviewRuntimeUnchanged(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	runtimeSig := func() string {
		var stateID, machineID string
		var ver int
		if err := h.DBScan(ctx,
			"SELECT current_state_id::text, state_machine_id::text, version FROM runtime_state WHERE project_id = '"+projectID.String()+"'",
			&stateID, &machineID, &ver); err != nil {
			t.Fatalf("scan runtime: %v", err)
		}
		return stateID + "|" + machineID + "|" + itoaSig(ver)
	}
	histSig := func() string {
		var sig string
		var cnt int
		if err := h.DBScan(ctx,
			"SELECT coalesce(string_agg(outcome, '' ORDER BY seq), ''), count(*) FROM state_transition_history",
			&sig, &cnt); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		return sig + "|" + itoaSig(cnt)
	}
	before := runtimeSig() + "#" + histSig()
	svc := p840PreviewSvc(t, h)
	if _, err := svc.Preview(ctx, projectID.String(), p840PreviewReq(templateID.String())); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if after := runtimeSig() + "#" + histSig(); after != before {
		t.Fatalf("runtime altered by preview: before=%q after=%q", before, after)
	}
}

// T8.35.4 — Preview identifies missing requirements: a required state-file
// type with no file at its scope is reported in MissingRequired and the
// preview SUCCEEDS (non-fatal, not a 500).
func TestP840PreviewIdentifiesMissingRequirements(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	// The rig's role requires type A (present). Require an additional type C
	// with no file -> reported as missing.
	roleID := p712ProjectRole(t, h, projectID, "P840-ROLE")
	p712RequireRoleFiles(t, h, roleID, []string{"A", "C"})
	if p718Required(t, h, projectID, "C", "PROJECT") == uuid.Nil {
		t.Fatal("type C not created")
	}
	svc := p840PreviewSvc(t, h)
	out, err := svc.Preview(ctx, projectID.String(), p840PreviewReq(templateID.String()))
	if err != nil {
		t.Fatalf("Preview (missing file) = %v, want success (missing is reported, not fatal)", err)
	}
	found := false
	for _, m := range out.MissingRequired {
		if m == "C" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MissingRequired = %v, want to include C", out.MissingRequired)
	}
}

// T8.35.5 — Preview deterministic: two previews over the same pinned position
// are byte-identical in content + hash + metadata (content, hash, snapshot
// hash, estimated tokens, component sizes, state/role/template).
func TestP840PreviewDeterministic(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	svc := p840PreviewSvc(t, h)
	req := p840PreviewReq(templateID.String())
	a, err := svc.Preview(ctx, projectID.String(), req)
	if err != nil {
		t.Fatalf("Preview #1: %v", err)
	}
	b, err := svc.Preview(ctx, projectID.String(), req)
	if err != nil {
		t.Fatalf("Preview #2: %v", err)
	}
	if a.Content != b.Content {
		t.Fatalf("content not byte-identical")
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash not identical: %q vs %q", a.Hash, b.Hash)
	}
	if a.SnapshotHash != b.SnapshotHash {
		t.Fatalf("snapshot hash not identical: %q vs %q", a.SnapshotHash, b.SnapshotHash)
	}
	if a.EstimatedTokens != b.EstimatedTokens {
		t.Fatalf("estimated tokens not identical: %d vs %d", a.EstimatedTokens, b.EstimatedTokens)
	}
	if !reflect.DeepEqual(a.Components, b.Components) {
		t.Fatalf("component sizes not identical: %+v vs %+v", a.Components, b.Components)
	}
}

// T8.35 REST — POST /prompts/preview returns 200 with the rendered content +
// metadata for a configured position; 404 for a project with no runtime
// (unknown position); 405 for a write-method mismatch (GET on the POST-only
// path). The 200 call persists nothing (direct SQL).
func TestP840PreviewRESTContract(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID, _ := p838Fixture(t, h)
	url := h.BaseURL + "/api/v1/projects/" + projectID.String() + "/prompts/preview"

	body := `{"template_id":"` + templateID.String() + `","template_version":1}`
	status, out := doJSON(t, http.MethodPost, url, body)
	if status != http.StatusOK {
		t.Fatalf("POST preview = %d, want 200: %+v", status, out)
	}
	if out["content"] == "" || out["hash"] == "" {
		t.Fatalf("preview body missing content/hash: %+v", out)
	}
	if out["state"] != "S" {
		t.Fatalf("preview state = %v, want S", out["state"])
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("REST preview persisted %d canonical prompt(s)", n)
	}

	// Unknown project (no runtime) -> 404.
	bare := createPhaseProject(t, h)
	status, _ = doJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/"+bare+"/prompts/preview", body)
	if status != http.StatusNotFound {
		t.Fatalf("preview for runtime-less project = %d, want 404", status)
	}

	// GET on the POST-only path -> 405.
	if code := httpRaw(t, http.MethodGet, url, nil); code != http.StatusMethodNotAllowed && code != http.StatusBadRequest {
		t.Fatalf("GET preview = %d, want 405", code)
	}
}
