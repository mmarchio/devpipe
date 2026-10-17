package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/compression/compressiontest"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/dbrepo"
	"overseer/internal/prompt"
)

// P9-60..P9-68: the WBS 9.49-9.57 scenario tests over real PostgreSQL. The
// compressor is a deterministic test double (scripted mock or the
// OpenAI-compatible fake server through the real adapter).

// p9GrowFile appends a new version of the project's state file of the given
// type with content (fixture bookkeeping before a fresh snapshot).
func p9GrowFile(t *testing.T, h *Harness, projectID uuid.UUID, typeKey, content string) {
	t.Helper()
	ctx := context.Background()
	var fileID uuid.UUID
	var cur int
	if err := h.DBScan(ctx, `SELECT sf.id, sf.current_version FROM state_files sf JOIN state_file_types ty ON ty.id = sf.type_id
		WHERE sf.project_id = '`+projectID.String()+`' AND ty.key = '`+typeKey+`' LIMIT 1`, &fileID, &cur); err != nil {
		t.Fatalf("find %s file: %v", typeKey, err)
	}
	if _, err := p708Service(t, h).UpdateContent(ctx, fileID.String(), content, cur); err != nil {
		t.Fatalf("grow %s: %v", typeKey, err)
	}
}

// WBS 9.49 — planning prompt: WBS identifiers, planning constraints,
// allowed work-unit kinds, dependency/acceptance/test requirements and the
// response schema survive exactly; background may shrink.
func TestP960PlanningPromptCompression(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, templateID := p843Rig(t, h)
	p843SetPlanningSettings(t, h, projectID, p843AtomicPolicy, "atomic")
	p9GrowFile(t, h, projectID, "PROJECT_STATE", "PROJECT_STATE pinned content\n"+p9Background("PROJECT_STATE", 150))
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := p843PlanningComposer(t, h).WithComponents(dbrepo.NewCanonicalPromptComponentRepository(h.App.DB().Raw())).
		ComposePrompt(ctx, prompt.ComposePromptRequest{ContextSnapshotID: snap.ID, TemplateID: templateID, TemplateVersion: 1})
	if err != nil {
		t.Fatalf("compose planning prompt: %v", err)
	}
	if !strings.Contains(rec.Content, p843WBSv3Content) {
		t.Fatal("fixture: WBS v3 not in canonical")
	}
	n := p9Tokens(rec)
	svc := p9Service(h, p9Source{compressor: compressiontest.NewScripted()})
	p9Configure(t, h, svc, projectID, n*4, p9Settings(n/2, 2))
	res, err := svc.PrepareWorkerPrompt(ctx, projectID, rec.ID)
	if err != nil || res.WorkerPrompt == nil || res.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatalf("planning compression: %v %+v", err, res)
	}
	wp := res.WorkerPrompt.Content
	p9MustContainAll(t, "planning worker prompt", wp,
		p843WBSv3Content, "Plan v3 unit 1: foundation primitives.", // WBS requirement identifiers
		p843AtomicPolicy, "Declare dependencies explicitly.", // planning constraints + dependency requirement
		"WBS v3", p843StateInstruction, p843SchemaProtocolBody,
	)
	comps, _ := p9ComponentRepo(h).ListComponents(ctx, rec.ID)
	for _, c := range compression.ApplyEffectivePolicies(comps) {
		if c.Type == prompt.ComponentResponseProtocol && !strings.Contains(wp, c.Content) {
			t.Fatal("planning response schema / allowed kinds not preserved")
		}
	}
	if strings.Contains(wp, "Earlier phase 99 delivered") || res.WorkerPrompt.EstimatedTokens >= n {
		t.Fatal("background did not shrink")
	}
}

// WBS 9.51 — a reset-session continuation prompt preserves the work-unit
// identity, the captured completed/remaining/blocking status, and the
// original acceptance criteria; completed and remaining are never swapped.
func TestP962ContinuationPromptCompression(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	sid := uuid.New()
	rec, err := p9Composer(t, h).ComposePrompt(ctx, prompt.ComposePromptRequest{
		ContextSnapshotID: pos.SnapID, TemplateID: pos.TemplateID, TemplateVersion: 1,
		ContinuationStatusID: &sid, ContinuationWorkUnitID: &pos.UnitID,
		CapturedStatus: &prompt.ContinuationStatusInput{
			WorkUnitID: pos.UnitID, Summary: "repository layer done",
			Completed: []string{"repository implementation"},
			Remaining: []string{"integration test", "error mapping"},
			Blocking:  []string{"waiting for schema review"},
		},
	})
	if err != nil {
		t.Fatalf("compose continuation: %v", err)
	}
	n := p9Tokens(rec)
	swap := compressiontest.Mutate(func(s string) string {
		s = strings.Replace(s, "completed: repository implementation", "completed: integration test; error mapping", 1)
		return strings.Replace(s, "remaining: integration test; error mapping", "remaining: repository implementation", 1)
	})
	sc := compressiontest.NewScripted(swap, nil)
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 2))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || res.WorkerPrompt == nil || res.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatalf("continuation compression: %v %+v", err, res)
	}
	if res.Attempts[0].Status != compression.AttemptValidationFailed {
		t.Fatal("swapped completed/remaining accepted")
	}
	p9MustContainAll(t, "continuation worker prompt", res.WorkerPrompt.Content,
		"Continue the current work unit "+pos.UnitID.String(),
		"completed: repository implementation",
		"remaining: integration test; error mapping",
		"blocking: waiting for schema review",
		p9AC1, p9AC2, p9AC3, p9AC4,
	)
}

// WBS 9.52 — a compressor that drops AC-3 and TEST-2 fails validation on
// every attempt; the invalid output never becomes worker-ready: with a
// context that cannot hold the canonical prompt there is no worker prompt
// at all; with one that can, the worker prompt is the canonical prompt.
func TestP963FailedCompression(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	drop := compressiontest.Mutate(func(s string) string {
		s = strings.Replace(s, p9AC3, "", -1)
		return strings.Replace(s, p9T2, "", -1)
	})
	sc := compressiontest.NewScripted(drop)
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, n-1, p9Settings(n/2, 2))

	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if !errors.Is(err, compression.ErrPromptPreparationFailed) || res.WorkerPrompt != nil || res.Run.Outcome != compression.RunFailed {
		t.Fatalf("invalid compression produced a worker prompt: %v %+v", err, res)
	}
	if sc.Calls() != 2 {
		t.Fatalf("retry not applied: %d calls", sc.Calls())
	}
	for _, a := range res.Attempts {
		v := a.Validation
		if a.Status != compression.AttemptValidationFailed || v == nil || !p9HasFailure(v, "AC-3") || !p9HasFailure(v, "TEST-2") {
			t.Fatalf("attempt %d not rejected for AC-3/TEST-2: %+v", a.Number, v)
		}
	}
	if p9Count(t, h, "worker_prompts", "canonical_prompt_id = '"+rec.ID.String()+"'") != 0 {
		t.Fatal("worker prompt persisted from invalid output")
	}

	// Context large enough: canonical fallback, never the invalid output.
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 2))
	res, err = svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || res.WorkerPrompt == nil || res.WorkerPrompt.Source != compression.SourceCanonical ||
		res.WorkerPrompt.Content != rec.Content || res.Run.Outcome != compression.RunFallbackCanonical {
		t.Fatalf("T9.29.1 canonical fallback: %v %+v", err, res)
	}
	if !strings.Contains(res.Run.Reason, "failed validation") {
		t.Fatalf("T9.29.4 fallback decision not recorded: %q", res.Run.Reason)
	}
}

func p9HasFailure(v *compression.ValidationResult, id string) bool {
	for _, f := range v.Failures {
		if f.ItemID == id {
			return true
		}
	}
	return false
}

// WBS 9.53 — a hallucinated requirement is detected by semantic validation
// and the compression rejected; the canonical prompt is unchanged.
func TestP964HallucinatedRequirement(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	before := p9CanonicalRow(t, h, rec.ID)
	n := p9Tokens(rec)
	add := compressiontest.Mutate(func(s string) string { return s + "\nAlso migrate the project from PostgreSQL to MongoDB." })
	svc := p9Service(h, p9Source{compressor: compressiontest.NewScripted(add)})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 1))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Attempts[0]
	if a.Status != compression.AttemptValidationFailed || a.Validation.Semantic == nil || len(a.Validation.Semantic.AddedRequirements) == 0 ||
		!strings.Contains(a.Validation.Semantic.AddedRequirements[0], "MongoDB") {
		t.Fatalf("T9.53.1 added requirement not detected: %+v", a.Validation)
	}
	if res.WorkerPrompt.Source != compression.SourceCanonical || strings.Contains(res.WorkerPrompt.Content, "MongoDB") {
		t.Fatal("T9.53.2 compression not rejected")
	}
	if p9CanonicalRow(t, h, rec.ID) != before {
		t.Fatal("T9.53.3 canonical modified")
	}
}

// WBS 9.54 — a reversed constraint fails deterministic validation.
func TestP965ReversedConstraint(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	rev := compressiontest.Mutate(func(s string) string {
		return strings.Replace(s, p9C2, "CONSTRAINT-2: Add external dependencies if useful.", -1)
	})
	svc := p9Service(h, p9Source{compressor: compressiontest.NewScripted(rev)})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 1))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Attempts[0].Validation
	reversed := false
	for _, f := range v.Failures {
		reversed = reversed || (f.ItemID == "CONSTRAINT-2" && f.Code == compression.CodeReversed)
	}
	if !reversed || res.Attempts[0].Status != compression.AttemptValidationFailed || res.WorkerPrompt.Source != compression.SourceCanonical {
		t.Fatalf("reversed constraint accepted: %+v", v.Failures)
	}
}

// WBS 9.55 — an output longer than the canonical prompt is classified
// ineffective even though it preserves everything; canonical fallback is
// used when safe; metrics record the expansion, never a compression.
func TestP966CompressionExpansion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	expand := compressiontest.Mutate(func(s string) string { return s + "\n" + strings.Repeat(s+"\n", 40) })
	svc := p9Service(h, p9Source{compressor: compressiontest.NewScripted(expand)})
	p9Configure(t, h, svc, pos.ProjectID, n*10, p9Settings(n/2, 1))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Attempts[0]
	if a.Status != compression.AttemptIneffective || a.Validation.Savings != compression.SavingsExpanded || a.Validation.Output <= n {
		t.Fatalf("T9.55.1/T9.55.2 expansion not classified: %+v", a.Validation)
	}
	if res.Run.Outcome != compression.RunFallbackCanonical || res.WorkerPrompt.Source != compression.SourceCanonical {
		t.Fatal("T9.55.3 canonical fallback")
	}
	m, err := svc.ProjectMetrics(ctx, pos.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if m.PromptsCompressed != 0 || m.PromptsFallback != 1 || m.ValidationFailures != 1 || m.AverageReduction != 0 {
		t.Fatalf("T9.55.4 metrics: %+v", m)
	}
	cps, _ := dbrepo.NewCompressionRepository(h.App.DB().Raw()).ListCompressedByCanonical(ctx, rec.ID)
	if len(cps) != 1 || cps[0].ValidationStatus != compression.ValidationIneffective || cps[0].CompressedTokens <= cps[0].CanonicalTokens {
		t.Fatalf("T9.55.4 expansion not recorded: %+v", cps)
	}
}

// WBS 9.56 — exact-preserve content larger than the target yields
// ErrCompressionTargetImpossible: no provider call, no truncation.
func TestP967OversizedExactContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	spec := p9DefaultSpec()
	for i := 0; i < 60; i++ {
		spec.Acceptance = append(spec.Acceptance, "AC-extra-"+itoa(i)+": the validator rejects every modified identifier, command, path and status value in the exact content set")
	}
	pos := p9Rig(t, h, spec)
	rec := p9Compose(t, h, pos)
	comps, _ := p9ComponentRepo(h).ListComponents(ctx, rec.ID)
	exact := compression.ExactTokens(compression.ApplyEffectivePolicies(comps))
	n := p9Tokens(rec)
	sc := compressiontest.NewScripted()
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(exact-1, 2))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if !errors.Is(err, compression.ErrCompressionTargetImpossible) {
		t.Fatalf("expected ErrCompressionTargetImpossible, got %v", err)
	}
	if sc.Calls() != 0 || res.WorkerPrompt != nil || res.Run.Outcome != compression.RunFailed || !strings.Contains(res.Run.Reason, "target impossible") {
		t.Fatalf("oversized exact content: calls=%d run=%+v", sc.Calls(), res.Run)
	}
	if p9Count(t, h, "compressed_prompts", "canonical_prompt_id = '"+rec.ID.String()+"'") != 0 {
		t.Fatal("truncated output persisted")
	}
}

// WBS 9.57 — provider outage through the real HTTP adapter: the retry
// policy runs, then the fallback is evaluated (canonical if safe, else a
// preparation failure); workflow state is unchanged.
func TestP968ProviderOutage(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	srv := providertest.New(providertest.Reply{Status: http.StatusServiceUnavailable, Body: `{"error":{"message":"overloaded"}}`})
	defer srv.Close()
	svc := p9Service(h, p9FakeFactory(srv, ""))
	runtimeBefore := p9RuntimeRow(t, h, pos.ProjectID)

	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 3))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || res.Run.Outcome != compression.RunFallbackCanonical || res.WorkerPrompt.Source != compression.SourceCanonical {
		t.Fatalf("outage fallback: %v %+v", err, res.Run)
	}
	if srv.Calls() != 3 || len(res.Attempts) != 3 {
		t.Fatalf("retry policy not applied: %d calls", srv.Calls())
	}
	for _, a := range res.Attempts {
		if a.Status != compression.AttemptProviderError || a.ErrorKind != compression.ErrorUnavailable {
			t.Fatalf("attempt %+v", a)
		}
	}
	p9Configure(t, h, svc, pos.ProjectID, n-1, p9Settings(n/2, 2))
	res, err = svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if !errors.Is(err, compression.ErrPromptPreparationFailed) || res.WorkerPrompt != nil {
		t.Fatalf("outage with oversized canonical: %v", err)
	}
	if p9RuntimeRow(t, h, pos.ProjectID) != runtimeBefore {
		t.Fatal("workflow/runtime state changed by a provider outage")
	}
	// Permanent failure (invalid credentials) stops immediately.
	auth := providertest.New(providertest.Reply{Status: http.StatusUnauthorized, Body: `{"error":"bad key"}`})
	defer auth.Close()
	svc2 := p9Service(h, p9FakeFactory(auth, ""))
	p9Configure(t, h, svc2, pos.ProjectID, n*4+1, p9Settings(n/2, 3))
	res, err = svc2.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || auth.Calls() != 1 || res.Attempts[0].ErrorKind != compression.ErrorAuth || res.Run.Outcome != compression.RunFallbackCanonical {
		t.Fatalf("auth failure: %v calls=%d", err, auth.Calls())
	}
}
