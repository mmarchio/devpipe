package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/persistence"
)

// Phase 11 (PLAN Decision 5): compression operations for precomputed
// placeholder templates.

// Template content sources.
const (
	TemplateFromCompressed = "COMPRESSED"
	TemplateFromCanonical  = "CANONICAL"
)

// ErrAdoptRejected: a precomputed prompt could not be adopted (policy
// mismatch or failed validation) — the caller prepares normally.
var ErrAdoptRejected = errors.New("service: precomputed prompt not adoptable")

// TemplateCompression is the result of compressing a placeholder template.
type TemplateCompression struct {
	Run           *compression.Run
	Compressed    *compression.CompressedPrompt
	ContentSource string
	Content       string
	Attempts      []compression.Attempt
}

// CompressTemplate compresses a canonical prompt that carries a placeholder
// (the projected unit's ACTIVE_TASK) through the normal Phase 9 loop —
// same instruction, retries and validation, with placeholder markers
// permitted — and records the run with outcome TEMPLATE. No worker prompt
// is created. When the policy bypasses compression (or compression fails
// but the canonical fits the context) the template content is the
// canonical prompt itself.
func (s *CompressionService) CompressTemplate(ctx context.Context, projectID, canonicalID uuid.UUID) (*TemplateCompression, error) {
	p, err := s.load(ctx, projectID, canonicalID)
	if err != nil {
		return nil, err
	}
	st := p.settings
	run := &compression.Run{
		ID: uuid.New(), ProjectID: projectID, CanonicalPromptID: canonicalID,
		RequestIdentity: p.identity, ConfigHash: p.configHash, InstructionVersion: p.instruction.Version,
		Provider: st.Provider, Model: st.Model, Mode: st.Mode,
		TargetTokens: st.EffectiveTarget(), CanonicalTokens: p.tokens, CreatedAt: s.now(),
	}
	decision, derr := compression.Decide(st, p.tokens)
	if derr != nil {
		return nil, derr
	}
	out := &TemplateCompression{Run: run}
	canonical := func(reason string) (*TemplateCompression, error) {
		if p.tokens > st.ContextWindow {
			return nil, fmt.Errorf("%w: %s", compression.ErrPromptPreparationFailed, reason)
		}
		out.ContentSource, out.Content = TemplateFromCanonical, p.canonical.Content
		return out, s.finishRun(ctx, run, "TEMPLATE", "template (canonical): "+reason)
	}
	var comps []compression.CompressionComponent
	var manifest compression.Manifest
	if decision.Action == compression.ActionCompress {
		if comps, err = s.components.ListComponents(ctx, canonicalID); err != nil {
			return nil, translate(err)
		}
		if manifest, err = s.components.ListManifest(ctx, canonicalID); err != nil {
			return nil, translate(err)
		}
		comps = compression.ApplyEffectivePolicies(comps)
	}
	var kept []compression.CompressionComponent
	if len(comps) > 0 {
		kept, run.Dropped = compression.DropOptional(comps, run.TargetTokens)
		cctx, release, err := s.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		ctx = cctx
	}
	if err := s.repo.CreateRun(ctx, run); err != nil {
		return nil, translate(err)
	}
	if decision.Action == compression.ActionBypass {
		return canonical(decision.Reason)
	}
	if len(comps) == 0 {
		return canonical("no persisted components")
	}
	if p.tokens > run.TargetTokens {
		if err := compression.CheckTargetFeasible(comps, run.TargetTokens); err != nil {
			_ = s.finishRun(context.WithoutCancel(ctx), run, compression.RunFailed, err.Error())
			return nil, err
		}
	}
	compressor, err := s.compressor(st)
	if err != nil {
		return canonical("compression provider unavailable: " + err.Error())
	}
	pipeline, err := s.pipeline(st)
	if err != nil {
		return canonical("semantic validator unavailable: " + err.Error())
	}
	guarded, why, ok := s.admitCompression(st.Provider, compressor)
	if !ok {
		return canonical(why)
	}
	defer guarded.done()
	loop := compression.AttemptLoop{
		Compressor: guarded, Pipeline: pipeline, Sleeper: s.sleeper,
		Backoff:     compression.Backoff{Base: st.BackoffBase, Max: st.BackoffMax},
		MaxAttempts: st.MaxAttempts, Timeout: st.Timeout, Provider: st.Provider, Model: st.Model, Now: s.now,
	}
	compressed := map[uuid.UUID]*compression.CompressedPrompt{}
	lres, lerr := loop.Run(ctx, compression.LoopInput{
		CanonicalPromptID: canonicalID, CanonicalTokens: p.tokens, Components: kept, Canonical: comps, Manifest: manifest,
		Instruction: p.instruction, TargetTokens: run.TargetTokens, MinimumSavings: st.MinimumSavings, AllowPlaceholders: true,
	}, func(ctx context.Context, a *compression.Attempt) error {
		a.RunID = run.ID
		cp, err := s.persistAttempt(ctx, p, a)
		if cp != nil {
			compressed[a.ID] = cp
		}
		return err
	})
	out.Attempts = lres.Attempts
	run.Attempts = len(lres.Attempts)
	if lerr != nil {
		_ = s.finishRun(context.WithoutCancel(ctx), run, compression.RunFailed, "template compression aborted: "+lerr.Error())
		return nil, deferredErr(ctx, lerr)
	}
	if lres.Accepted == nil {
		return canonical(describeFailure(lres))
	}
	cp := compressed[lres.Accepted.ID]
	out.Compressed, out.ContentSource, out.Content = cp, TemplateFromCompressed, cp.Content
	return out, s.finishRun(ctx, run, "TEMPLATE", fmt.Sprintf("template compressed; attempt %d accepted", lres.Accepted.Number))
}

// AdoptPrecomputed turns a resolved precomputed prompt (placeholder already
// replaced with the real value) into a worker prompt for the REAL canonical
// prompt — only after it passes the full Phase 9 validation pipeline
// against the real canonical components (exact fragments, manifest,
// semantics, size). Any policy mismatch or validation failure returns
// ErrAdoptRejected (recorded) and the caller prepares normally. No
// compression provider is called.
func (s *CompressionService) AdoptPrecomputed(ctx context.Context, projectID, canonicalID uuid.UUID, content, contentSource string) (*PreparationResult, error) {
	if compression.ContainsPlaceholder(content) {
		return nil, fmt.Errorf("%w: unresolved placeholder", ErrAdoptRejected)
	}
	p, err := s.load(ctx, projectID, canonicalID)
	if err != nil {
		return nil, err
	}
	st := p.settings
	decision, derr := compression.Decide(st, p.tokens)
	if derr != nil {
		return nil, fmt.Errorf("%w: %v", ErrAdoptRejected, derr)
	}
	run := &compression.Run{
		ID: uuid.New(), ProjectID: projectID, CanonicalPromptID: canonicalID,
		RequestIdentity: p.identity, ConfigHash: p.configHash, InstructionVersion: p.instruction.Version,
		Provider: st.Provider, Model: st.Model, Mode: st.Mode,
		TargetTokens: st.EffectiveTarget(), CanonicalTokens: p.tokens, CreatedAt: s.now(),
	}
	result := &PreparationResult{Run: run}
	switch contentSource {
	case TemplateFromCanonical:
		if decision.Action != compression.ActionBypass || content != p.canonical.Content {
			return nil, fmt.Errorf("%w: canonical template does not match the real canonical prompt / policy", ErrAdoptRejected)
		}
		if err := s.repo.CreateRun(ctx, run); err != nil {
			return nil, translate(err)
		}
		return result, s.finishCanonical(ctx, result, compression.RunBypassed, "precomputed template resolved (canonical): "+decision.Reason)
	case TemplateFromCompressed:
	default:
		return nil, fmt.Errorf("%w: unknown content source %q", ErrAdoptRejected, contentSource)
	}
	if decision.Action != compression.ActionCompress {
		return nil, fmt.Errorf("%w: policy no longer compresses this prompt", ErrAdoptRejected)
	}
	comps, err := s.components.ListComponents(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	manifest, err := s.components.ListManifest(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("%w: canonical prompt has no persisted components", ErrAdoptRejected)
	}
	comps = compression.ApplyEffectivePolicies(comps)
	kept, dropped := compression.DropOptional(comps, run.TargetTokens)
	run.Dropped = dropped
	pipeline, err := s.pipeline(st)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAdoptRejected, err)
	}
	si := compression.RenderStructuredInput(kept)
	vres := pipeline.Validate(ctx, compression.ValidationInput{
		Components: kept, Canonical: comps, Manifest: manifest, Boundary: si.Boundary, Compressed: content,
		CanonicalTokens: p.tokens, TargetTokens: run.TargetTokens, MinimumSavings: st.MinimumSavings,
	})
	if err := s.repo.CreateRun(ctx, run); err != nil {
		return nil, translate(err)
	}
	a := compression.Attempt{
		ID: uuid.New(), RunID: run.ID, Number: 1, Provider: "precomputed-template", Model: st.Model,
		Output: content, Validation: &vres, CreatedAt: s.now(),
	}
	switch vres.Status {
	case compression.ValidationPassed:
		a.Status = compression.AttemptAccepted
	case compression.ValidationIneffective:
		a.Status = compression.AttemptIneffective
	default:
		a.Status = compression.AttemptValidationFailed
	}
	cp, err := s.persistAttempt(ctx, p, &a)
	if err != nil {
		return nil, err
	}
	run.Attempts = 1
	result.Attempts = []compression.Attempt{a}
	if a.Status != compression.AttemptAccepted || cp == nil {
		_ = s.finishRun(context.WithoutCancel(ctx), run, compression.RunFailed, "precomputed template failed validation")
		return result, fmt.Errorf("%w: validation %s", ErrAdoptRejected, vres.Status)
	}
	wp, err := compression.WorkerPromptFromCompressed(*cp, run.ID, s.now())
	if err != nil {
		return nil, err
	}
	return result, s.finishWithWorker(ctx, result, &wp, compression.RunCompressed, "precomputed placeholder template resolved and validated")
}

var _ = persistence.ErrNotFound
