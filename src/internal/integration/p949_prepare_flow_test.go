package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/compression/compressiontest"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P9-49..P9-52 + P9-05/P9-12/P9-13/P9-18/P9-46..P9-48 persistence halves:
// the PrepareWorkerPrompt flows over real PostgreSQL.

// T9.30.1-T9.30.4 / T9.31.1 / T9.35.1 — a prompt within the target
// bypasses the compressor: no provider call, a CANONICAL worker prompt, the
// bypass reason recorded.
func TestP949BypassFlow(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	sc := compressiontest.NewScripted()
	svc := p9Service(h, p9Source{compressor: sc})
	n := p9Tokens(rec)
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n+10, 2))

	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Calls() != 0 {
		t.Fatal("T9.30.2 provider called on bypass")
	}
	wp := res.WorkerPrompt
	if wp == nil || wp.Source != compression.SourceCanonical || wp.Content != rec.Content || wp.CompressedPromptID != nil {
		t.Fatalf("T9.30.3 worker prompt %+v", wp)
	}
	if wp.Hash != compression.WorkerPromptHash(rec.Content, compression.SourceCanonical) {
		t.Fatal("T9.33.1 worker hash")
	}
	runs, _ := dbrepo.NewCompressionRepository(h.App.DB().Raw()).ListRunsByCanonical(ctx, rec.ID)
	if len(runs) != 1 || runs[0].Outcome != compression.RunBypassed || !strings.Contains(runs[0].Reason, "not required") || runs[0].Attempts != 0 {
		t.Fatalf("T9.30.4 bypass reason not recorded: %+v", runs)
	}
	if p9Count(t, h, "compression_attempts", "canonical_prompt_id = '"+rec.ID.String()+"'") != 0 {
		t.Fatal("attempt recorded on bypass")
	}
}

// T9.31.2 / T9.31.3 / T9.31.4 — ALWAYS compresses a small prompt; NEVER
// bypasses a large one but rejects one that cannot fit the context.
func TestP949ForcedModes(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)

	sc := compressiontest.NewScripted()
	svc := p9Service(h, p9Source{compressor: sc})
	always := p9Settings(n+100, 1)
	always.Mode = compression.ModeAlways
	p9Configure(t, h, svc, pos.ProjectID, n*4, always)
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || sc.Calls() != 1 || res.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatalf("T9.31.2 ALWAYS: calls=%d err=%v run=%+v", sc.Calls(), err, res.Run)
	}

	never := p9Settings(n/2, 1)
	never.Mode = compression.ModeNever
	sc2 := compressiontest.NewScripted()
	svc2 := p9Service(h, p9Source{compressor: sc2})
	p9Configure(t, h, svc2, pos.ProjectID, n*4, never)
	res, err = svc2.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || sc2.Calls() != 0 || res.Run.Outcome != compression.RunBypassed || res.WorkerPrompt.Source != compression.SourceCanonical {
		t.Fatalf("T9.31.3 NEVER: %v %+v", err, res)
	}

	p9Configure(t, h, svc2, pos.ProjectID, n-1, never)
	res, err = svc2.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if !errors.Is(err, compression.ErrCanonicalExceedsContext) || res.WorkerPrompt != nil || res.Run.Outcome != compression.RunFailed {
		t.Fatalf("T9.31.4 NEVER oversize: %v %+v", err, res)
	}
	if sc2.Calls() != 0 {
		t.Fatal("NEVER called the compressor")
	}
}

// T9.26.x / T9.27.x / T9.13.x / T9.25.5 / T9.28.4 — a failed attempt and
// the accepted retry are persisted with their validation results; the
// compressed prompts are separate immutable rows; the canonical row is
// unchanged; artifacts are immutable.
func TestP949RetryPersistenceAndImmutability(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	before := p9CanonicalRow(t, h, rec.ID)
	n := p9Tokens(rec)
	sc := compressiontest.NewScripted(compressiontest.Mutate(func(s string) string { return strings.Replace(s, p9AC3, "", 1) }), nil)
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 2))

	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || res.Run.Attempts != 2 || res.Run.Outcome != compression.RunCompressed {
		t.Fatalf("attempts %+v", res.Run)
	}
	// T9.27.1 the retry carried the AC-3 feedback; T9.27.2 same canonical
	// structured input.
	reqs := sc.Requests()
	if !strings.Contains(reqs[1].Feedback, "AC-3") || reqs[1].Content != reqs[0].Content {
		t.Fatalf("retry feedback %q", reqs[1].Feedback)
	}
	repo := dbrepo.NewCompressionRepository(h.App.DB().Raw())
	atts, err := repo.ListAttemptsByCanonical(ctx, rec.ID)
	if err != nil || len(atts) != 2 {
		t.Fatalf("T9.26.2 persisted attempts: %v %d", err, len(atts))
	}
	if atts[0].Number != 1 || atts[0].Status != compression.AttemptValidationFailed || atts[0].Validation == nil || atts[0].Output == "" {
		t.Fatalf("T9.26.5/T9.25.5 failed attempt %+v", atts[0])
	}
	if atts[1].Number != 2 || !atts[1].Accepted() || atts[1].CompressedPromptID == nil {
		t.Fatal("T9.26.4 accepted attempt")
	}
	cps, _ := repo.ListCompressedByCanonical(ctx, rec.ID)
	if len(cps) != 2 || cps[0].ValidationStatus != compression.ValidationFailed || cps[1].ValidationStatus != compression.ValidationPassed {
		t.Fatalf("T9.13.1 compressed prompts %+v", cps)
	}
	if cps[1].CanonicalPromptID != rec.ID || cps[1].Model != "compressor-7b" || cps[1].Attempt != 2 || cps[1].CompressionInstructionVersion != 1 {
		t.Fatal("T9.13.2-T9.13.4 metadata")
	}
	if cps[1].Hash != compression.CompressedPromptHash(cps[1].Content) || cps[1].Hash == rec.Hash {
		t.Fatal("T9.14.1/T9.14.4")
	}
	if p9CanonicalRow(t, h, rec.ID) != before {
		t.Fatal("T9.13.5 canonical modified")
	}
	// Immutability (T9.33.4).
	for _, q := range []string{
		"UPDATE worker_prompts SET hash = 'x' WHERE canonical_prompt_id = '" + rec.ID.String() + "'",
		"UPDATE compressed_prompts SET content = 'x' WHERE canonical_prompt_id = '" + rec.ID.String() + "'",
		"UPDATE compression_attempts SET status = 'ACCEPTED' WHERE canonical_prompt_id = '" + rec.ID.String() + "'",
	} {
		if _, err := h.DBExec(ctx, q); err == nil {
			t.Fatalf("immutable row updated: %s", q)
		}
	}
	// A run is finalized exactly once.
	if err := repo.FinishRun(ctx, res.Run.ID, compression.RunFailed, "again", 2, time.Now()); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("run finalized twice: %v", err)
	}
	// Worker prompt CHECK: COMPRESSED requires a compressed prompt id.
	if _, err := h.DBExec(ctx, "INSERT INTO worker_prompts (id, project_id, canonical_prompt_id, compressed_prompt_id, run_id, source, content, hash, estimated_tokens, created_at) VALUES ('"+uuid.NewString()+"', '"+pos.ProjectID.String()+"', '"+rec.ID.String()+"', NULL, '"+res.Run.ID.String()+"', 'COMPRESSED', 'x', 'x', 1, now())"); err == nil {
		t.Fatal("COMPRESSED worker prompt without compressed prompt accepted")
	}
}

// T9.43.2 / T9.43.3 / T9.42.1-T9.42.3 — an identical repeated preparation
// reuses the accepted result without a provider call; a configuration
// change yields a new identity; concurrent identical preparations make one
// provider call.
func TestP952IdempotencyAndSingleFlight(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)

	var mu sync.Mutex
	calls := 0
	slow := compressiontest.Step(func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		return compressiontest.Heuristic(ctx, req)
	})
	sc := compressiontest.NewScripted(slow)
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 2))

	var wg sync.WaitGroup
	results := make([]*service.PreparationResult, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || results[i].WorkerPrompt == nil {
			t.Fatalf("T9.42.2 caller %d: %v", i, err)
		}
		if results[i].WorkerPrompt.ID != results[0].WorkerPrompt.ID {
			t.Fatal("T9.42.2 callers got different results")
		}
	}
	if calls != 1 || p9Count(t, h, "compression_runs", "canonical_prompt_id = '"+rec.ID.String()+"'") != 1 {
		t.Fatalf("T9.42.1 provider calls=%d", calls)
	}

	again, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || !again.Reused || again.WorkerPrompt.ID != results[0].WorkerPrompt.ID || calls != 1 {
		t.Fatalf("T9.43.2 reuse: %v reused=%v calls=%d", err, again != nil && again.Reused, calls)
	}

	// T9.43.3 configuration change → new identity → new run.
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2-1, 2))
	changed, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || changed.Reused || changed.Run.RequestIdentity == results[0].Run.RequestIdentity || calls != 2 {
		t.Fatalf("T9.43.3: %v calls=%d", err, calls)
	}

	// T9.43.4 instruction change → new identity.
	if _, err := dbrepo.NewCompressionInstructionRepository(h.App.DB().Raw()).Append(ctx, compression.DefaultInstructionContent+"\nKeep section order."); err != nil {
		t.Fatal(err)
	}
	instr, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil || instr.Reused || instr.Run.InstructionVersion != 2 || instr.Run.RequestIdentity == changed.Run.RequestIdentity {
		t.Fatalf("T9.43.4: %v %+v", err, instr.Run)
	}
	// T9.42.4 a different prompt executes independently.
	rec2 := p9Compose(t, h, pos)
	if rec2.ID == rec.ID {
		t.Fatal("expected a new canonical row")
	}
	if r2, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec2.ID); err != nil || r2.Reused || r2.Run.CanonicalPromptID != rec2.ID {
		t.Fatalf("T9.42.4: %v", err)
	}
}

// T9.2.x DB half + T9.10.x persistence — settings validated and stored
// (absent → defaults; CHECKs reject invalid rows); instructions append-only
// with historical versions retained and hashes generated.
func TestP905SettingsAndInstructionPersistence(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	svc := p9Service(h, p9Source{})
	st, stored, err := svc.EffectiveSettings(ctx, pos.ProjectID)
	if err != nil || stored || st.Mode != compression.ModeAuto || st.MaxAttempts != compression.DefaultMaxAttempts || st.ContextWindow <= 0 {
		t.Fatalf("defaults: %v %+v", err, st)
	}
	bad := compression.DefaultSettings()
	bad.TargetRatio = 3
	if _, err := svc.UpdateCompressionSettings(ctx, pos.ProjectID, bad); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("T9.2.2 invalid ratio stored: %v", err)
	}
	good := p9Settings(1000, 3)
	good.Mode = compression.ModeAlways
	if _, err := svc.UpdateCompressionSettings(ctx, pos.ProjectID, good); err != nil {
		t.Fatal(err)
	}
	st, stored, _ = svc.EffectiveSettings(ctx, pos.ProjectID)
	if !stored || st.Mode != compression.ModeAlways || st.MaxAttempts != 3 || st.TargetTokens != 1000 || st.Timeout != compression.DefaultTimeout {
		t.Fatalf("T9.2.1 round trip %+v", st)
	}
	for _, q := range []string{
		"UPDATE project_compression_settings SET target_tokens = -1",
		"UPDATE project_compression_settings SET max_attempts = 0",
		"UPDATE project_compression_settings SET target_ratio = 1.5",
		"UPDATE project_compression_settings SET mode = 'SOMETIMES'",
	} {
		if _, err := h.DBExec(ctx, q); err == nil {
			t.Fatalf("DB CHECK accepted %s", q)
		}
	}

	instr := dbrepo.NewCompressionInstructionRepository(h.App.DB().Raw())
	cur, err := instr.EnsureDefault(ctx)
	if err != nil || cur.Version != 1 || cur.Hash != compression.InstructionHash(compression.DefaultInstructionContent) {
		t.Fatalf("T9.10.1 seeded: %v %+v", err, cur)
	}
	if again, _ := instr.EnsureDefault(ctx); again.Version != 1 {
		t.Fatal("seed not idempotent")
	}
	v2, err := instr.Append(ctx, "v2 instruction")
	if err != nil || v2.Version != 2 {
		t.Fatalf("T9.10.2: %v", err)
	}
	all, _ := instr.List(ctx)
	if len(all) != 2 || all[0].Content != compression.DefaultInstructionContent || all[1].Hash != compression.InstructionHash("v2 instruction") {
		t.Fatal("T9.10.3/T9.10.4 history")
	}
}

// T9.4.1/T9.4.2 DB half — components round-trip with policies + source
// provenance; the Phase 8 canonical content/hash are unchanged by component
// persistence (composition with and without the component store is
// byte-identical).
func TestP914ComponentPersistenceKeepsCanonicalIdentical(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	with := p9Compose(t, h, pos)
	without, err := p838Composer(t, h).ComposePrompt(ctx, prompt.ComposePromptRequest{ContextSnapshotID: pos.SnapID, TemplateID: pos.TemplateID, TemplateVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if with.Content != without.Content || with.Hash != without.Hash || with.EstimatedTokens != without.EstimatedTokens {
		t.Fatal("component persistence changed the canonical prompt")
	}
	comps, _ := p9ComponentRepo(h).ListComponents(ctx, with.ID)
	if none, _ := p9ComponentRepo(h).ListComponents(ctx, without.ID); len(none) != 0 {
		t.Fatal("components persisted without the store")
	}
	seenExact, seenCompressible := false, false
	for i, c := range comps {
		if c.Ordinal != i || !c.Policy.Known() {
			t.Fatalf("component %d %+v", i, c)
		}
		seenExact = seenExact || c.Policy == prompt.PolicyPreserveExact
		seenCompressible = seenCompressible || c.Policy == prompt.PolicyCompressible
		if c.Type == prompt.ComponentTaskContext && c.Source == prompt.SourceWorkUnit && c.SourceID != pos.UnitID {
			t.Fatal("source provenance lost")
		}
	}
	if !seenExact || !seenCompressible {
		t.Fatal("policies not retained")
	}
}
