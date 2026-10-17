package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/compression"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/resilience"
)

// Phase 9 (WBS 9.35): the prompt-compression application service. It turns
// a persisted canonical prompt into a worker-ready prompt:
//
//	load canonical → load compression policy → need compression?
//	  NO  → worker prompt = canonical (bypass, reason recorded)
//	  YES → drop optional (deterministic) → compress → validate →
//	        retry (canonical always the source) → accepted compressed
//	        prompt, or fallback (canonical when it safely fits, else fail)
//
// Every run, attempt, compressed output and worker prompt is persisted; the
// canonical prompt is never modified, and compression failure never touches
// workflow/runtime state.

// CompressorSource resolves provider adapters by the project's configured
// provider name (implemented by compressionprovider.Factory).
type CompressorSource interface {
	Compressor(name string) (compression.Compressor, error)
	SemanticValidator(name, model string) (compression.SemanticValidator, error)
}

// Compression service errors (service-level aliases of the domain errors).
var (
	ErrCompressionTargetImpossible = compression.ErrCompressionTargetImpossible
	ErrPromptPreparationFailed     = compression.ErrPromptPreparationFailed
	ErrCanonicalExceedsContext     = compression.ErrCanonicalExceedsContext
)

// CompressionService implements PrepareWorkerPrompt and the compression
// read surfaces (preview, attempts, metrics, diff, lineage, settings).
type CompressionService struct {
	canonical    persistence.CanonicalPromptRepository
	components   persistence.CanonicalPromptComponentRepository
	projSettings persistence.SettingsRepository
	settings     persistence.CompressionSettingsRepository
	instructions persistence.CompressionInstructionRepository
	repo         persistence.CompressionRepository
	source       CompressorSource
	txPool       *pgxpool.Pool

	sleeper compression.Sleeper
	now     func() time.Time
	flight  compression.SingleFlight
	guard   compressionGuard
}

// NewCompressionService wires the service.
func NewCompressionService(
	canonical persistence.CanonicalPromptRepository,
	components persistence.CanonicalPromptComponentRepository,
	projSettings persistence.SettingsRepository,
	settings persistence.CompressionSettingsRepository,
	instructions persistence.CompressionInstructionRepository,
	repo persistence.CompressionRepository,
	source CompressorSource,
) *CompressionService {
	return &CompressionService{
		canonical: canonical, components: components, projSettings: projSettings,
		settings: settings, instructions: instructions, repo: repo, source: source,
		sleeper: compression.RealSleeper{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithTxPool wires the pool used for atomic multi-row writes.
func (s *CompressionService) WithTxPool(pool *pgxpool.Pool) *CompressionService {
	s.txPool = pool
	return s
}

// WithSleeper overrides the backoff sleeper (tests).
func (s *CompressionService) WithSleeper(sl compression.Sleeper) *CompressionService {
	s.sleeper = sl
	return s
}

// WithSource replaces the compressor source (tests).
func (s *CompressionService) WithSource(src CompressorSource) *CompressionService {
	s.source = src
	return s
}

// PreparationResult is the outcome of PrepareWorkerPrompt.
type PreparationResult struct {
	Run          *compression.Run
	WorkerPrompt *compression.WorkerPrompt
	Attempts     []compression.Attempt
	// Reused reports that an existing accepted result for the same request
	// identity was returned without a provider call (WBS 9.43).
	Reused bool
}

// ---------------------------------------------------------------------------
// Settings.

// EffectiveSettings merges the project configuration (provider, model,
// context window, threshold) with the stored compression settings (or the
// defaults when none are stored).
func (s *CompressionService) EffectiveSettings(ctx context.Context, projectID uuid.UUID) (compression.Settings, bool, error) {
	ps, err := s.projSettings.Get(ctx, projectID.String())
	if err != nil {
		return compression.Settings{}, false, translate(err)
	}
	out := compression.DefaultSettings()
	stored := false
	if rec, err := s.settings.Get(ctx, projectID); err == nil {
		out = rec.Settings
		stored = true
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return compression.Settings{}, false, translate(err)
	}
	out.Provider, out.Model = ps.CompressionProvider, ps.CompressionModel
	out.ContextWindow, out.ContextThreshold = ps.ContextWindow, ps.ContextThreshold
	return out, stored, nil
}

// UpdateCompressionSettings validates and stores the compression-specific
// settings (T9.2.1-T9.2.5). A *compression.SettingsError is returned for
// invalid input (wrapped with ErrInvalidInput).
func (s *CompressionService) UpdateCompressionSettings(ctx context.Context, projectID uuid.UUID, in compression.Settings) (compression.Settings, error) {
	cur, _, err := s.EffectiveSettings(ctx, projectID)
	if err != nil {
		return compression.Settings{}, err
	}
	merged := in
	merged.Provider, merged.Model = cur.Provider, cur.Model
	merged.ContextWindow, merged.ContextThreshold = cur.ContextWindow, cur.ContextThreshold
	if err := merged.Validate(); err != nil {
		return compression.Settings{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if err := s.settings.Upsert(ctx, &persistence.CompressionSettingsRecord{ProjectID: projectID, Settings: merged, UpdatedAt: s.now()}); err != nil {
		return compression.Settings{}, translate(err)
	}
	return merged, nil
}

// ---------------------------------------------------------------------------
// PrepareWorkerPrompt.

// ConfigIdentity returns the project's effective compression settings, the
// current compression instruction and the semantic compression
// configuration hash (WBS 9.44) — a Phase 10 cache-key input.
func (s *CompressionService) ConfigIdentity(ctx context.Context, projectID uuid.UUID) (compression.Settings, compression.Instruction, string, error) {
	st, _, err := s.EffectiveSettings(ctx, projectID)
	if err != nil {
		return compression.Settings{}, compression.Instruction{}, "", err
	}
	instr, err := s.instructions.GetCurrent(ctx)
	if errors.Is(err, persistence.ErrNotFound) {
		instr, err = s.instructions.EnsureDefault(ctx)
	}
	if err != nil {
		return compression.Settings{}, compression.Instruction{}, "", translate(err)
	}
	return st, instr, compression.ConfigHashFor(st, instr), nil
}

// preparation is the loaded material of one preparation.
type preparation struct {
	canonical   *persistence.CanonicalPromptRecord
	settings    compression.Settings
	instruction compression.Instruction
	configHash  string
	identity    string
	tokens      int
	skip        func(uuid.UUID) bool
}

func (s *CompressionService) load(ctx context.Context, projectID, canonicalID uuid.UUID) (*preparation, error) {
	if s.canonical == nil || s.repo == nil {
		return nil, ErrInfrastructure
	}
	cp, err := s.canonical.Get(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	if cp.ProjectID != projectID {
		return nil, ErrNotFound
	}
	st, _, err := s.EffectiveSettings(ctx, projectID)
	if err != nil {
		return nil, err
	}
	instr, err := s.instructions.GetCurrent(ctx)
	if errors.Is(err, persistence.ErrNotFound) {
		instr, err = s.instructions.EnsureDefault(ctx)
	}
	if err != nil {
		return nil, translate(err)
	}
	cfg := compression.ConfigHashFor(st, instr)
	return &preparation{
		canonical: cp, settings: st, instruction: instr, configHash: cfg,
		identity: compression.RequestIdentity(cp.Hash, cfg),
		tokens:   compression.EstimateTokens(cp.Content),
	}, nil
}

// PrepareWorkerPrompt prepares the worker-ready prompt for a canonical
// prompt (WBS 9.35). Errors: ErrNotFound (unknown prompt / foreign
// project), ErrCompressionTargetImpossible, ErrCanonicalExceedsContext,
// ErrPromptPreparationFailed (a FAILED run is persisted for each of the
// last three).
func (s *CompressionService) PrepareWorkerPrompt(ctx context.Context, projectID, canonicalID uuid.UUID) (*PreparationResult, error) {
	return s.PrepareWorkerPromptExcluding(ctx, projectID, canonicalID, nil)
}

// PrepareWorkerPromptExcluding is PrepareWorkerPrompt where stored results
// whose worker prompt is revoked (skip returns true — e.g. an administrator
// invalidated the cache entry serving it, Phase 10 WBS 10.41) are never
// reused: a fresh preparation runs instead.
func (s *CompressionService) PrepareWorkerPromptExcluding(ctx context.Context, projectID, canonicalID uuid.UUID, skip func(workerPromptID uuid.UUID) bool) (*PreparationResult, error) {
	p, err := s.load(ctx, projectID, canonicalID)
	if err != nil {
		return nil, err
	}
	p.skip = skip
	if res, ok, err := s.reuse(ctx, p); err != nil || ok {
		return res, err
	}
	key := p.identity
	if skip != nil {
		key += "|fresh"
	}
	// Speculative and active preparations never share a flight: a
	// preempted speculative run must not fail an active caller.
	if resilience.PriorityOf(ctx) == resilience.Speculative {
		key += "|speculative"
	}
	v, err, _ := s.flight.Do(key, func() (any, error) {
		// Re-check inside the flight: a concurrent identical call may have
		// completed between the first check and acquiring the key.
		if res, ok, err := s.reuse(ctx, p); err != nil || ok {
			return res, err
		}
		return s.prepare(ctx, p)
	})
	if v == nil {
		return nil, err
	}
	return v.(*PreparationResult), err
}

// reuse returns the newest successful run for the same request identity
// (WBS 9.43: repeated preparation is safely repeatable; model output is not
// assumed deterministic, so the stored result is reused).
func (s *CompressionService) reuse(ctx context.Context, p *preparation) (*PreparationResult, bool, error) {
	runs, err := s.repo.ListRunsByIdentity(ctx, p.identity)
	if err != nil {
		return nil, false, translate(err)
	}
	for _, r := range runs {
		if r.WorkerPromptID == nil || r.CanonicalPromptID != p.canonical.ID || (p.skip != nil && p.skip(*r.WorkerPromptID)) {
			continue
		}
		wp, err := s.repo.GetWorkerPrompt(ctx, *r.WorkerPromptID)
		if err != nil {
			return nil, false, translate(err)
		}
		atts, err := s.repo.ListAttemptsByRun(ctx, r.ID)
		if err != nil {
			return nil, false, translate(err)
		}
		return &PreparationResult{Run: r, WorkerPrompt: wp, Attempts: atts, Reused: true}, true, nil
	}
	return nil, false, nil
}

func (s *CompressionService) prepare(ctx context.Context, p *preparation) (*PreparationResult, error) {
	st := p.settings
	run := &compression.Run{
		ID: uuid.New(), ProjectID: p.canonical.ProjectID, CanonicalPromptID: p.canonical.ID,
		RequestIdentity: p.identity, ConfigHash: p.configHash, InstructionVersion: p.instruction.Version,
		Provider: st.Provider, Model: st.Model, Mode: st.Mode,
		TargetTokens: st.EffectiveTarget(), CanonicalTokens: p.tokens, CreatedAt: s.now(),
	}
	decision, derr := compression.Decide(st, p.tokens)

	var comps []compression.CompressionComponent
	var manifest compression.Manifest
	if derr == nil && decision.Action == compression.ActionCompress {
		var err error
		if comps, err = s.components.ListComponents(ctx, p.canonical.ID); err != nil {
			return nil, translate(err)
		}
		if manifest, err = s.components.ListManifest(ctx, p.canonical.ID); err != nil {
			return nil, translate(err)
		}
		if err := compression.ValidateComponents(comps); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		comps = compression.ApplyEffectivePolicies(comps)
	}
	// Optional components are dropped deterministically before the run is
	// recorded so the run carries the dropped set (T9.8.3).
	var kept []compression.CompressionComponent
	if len(comps) > 0 {
		kept, run.Dropped = compression.DropOptional(comps, run.TargetTokens)
		// A provider run takes a compression slot (WBS 18.94); deferred
		// speculative work records nothing (WBS 18.95).
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
	result := &PreparationResult{Run: run}

	if derr != nil {
		return result, s.fail(ctx, result, derr.Error(), derr)
	}
	if decision.Action == compression.ActionBypass {
		return result, s.finishCanonical(ctx, result, compression.RunBypassed, decision.Reason)
	}
	if len(comps) == 0 {
		return result, s.fallback(ctx, result, "compression required but the canonical prompt has no persisted components")
	}
	// Exact content that alone exceeds the target can never be compressed
	// safely: explicit error, no truncation, no provider call (WBS 9.9/9.56).
	if p.tokens > run.TargetTokens {
		if err := compression.CheckTargetFeasible(comps, run.TargetTokens); err != nil {
			return result, s.fail(ctx, result, err.Error(), err)
		}
	}
	compressor, err := s.compressor(st)
	if err != nil {
		return result, s.fallback(ctx, result, "compression provider unavailable: "+err.Error())
	}
	pipeline, err := s.pipeline(st)
	if err != nil {
		return result, s.fallback(ctx, result, "semantic validator unavailable: "+err.Error())
	}
	// An open circuit (WBS 18.29) goes straight to the Phase 9 fallback:
	// canonical when it safely fits, else preparation stops.
	guarded, why, ok := s.admitCompression(st.Provider, compressor)
	if !ok {
		return result, s.fallback(ctx, result, why)
	}
	defer guarded.done()
	loop := compression.AttemptLoop{
		Compressor: guarded, Pipeline: pipeline, Sleeper: s.sleeper,
		Backoff:     compression.Backoff{Base: st.BackoffBase, Max: st.BackoffMax},
		MaxAttempts: st.MaxAttempts, Timeout: st.Timeout,
		Provider: st.Provider, Model: st.Model, Now: s.now,
	}
	compressed := map[uuid.UUID]*compression.CompressedPrompt{}
	sink := func(ctx context.Context, a *compression.Attempt) error {
		a.RunID = run.ID
		cp, err := s.persistAttempt(ctx, p, a)
		if err != nil {
			return err
		}
		if cp != nil {
			compressed[a.ID] = cp
		}
		return nil
	}
	lres, lerr := loop.Run(ctx, compression.LoopInput{
		CanonicalPromptID: p.canonical.ID, CanonicalTokens: p.tokens,
		Components: kept, Canonical: comps, Manifest: manifest, Instruction: p.instruction,
		TargetTokens: run.TargetTokens, MinimumSavings: st.MinimumSavings,
	}, sink)
	result.Attempts = lres.Attempts
	run.Attempts = len(lres.Attempts)
	if lerr != nil {
		// Caller cancellation or an infrastructure failure while recording
		// attempts: the run fails; nothing is dispatched.
		_ = s.finishRun(context.WithoutCancel(ctx), run, compression.RunFailed, "preparation aborted: "+lerr.Error())
		return result, deferredErr(ctx, lerr)
	}
	if lres.Accepted != nil {
		cp := compressed[lres.Accepted.ID]
		if cp == nil {
			return result, ErrInfrastructure
		}
		wp, err := compression.WorkerPromptFromCompressed(*cp, run.ID, s.now())
		if err != nil {
			return result, err
		}
		reason := fmt.Sprintf("%s; attempt %d accepted", decision.Reason, lres.Accepted.Number)
		return result, s.finishWithWorker(ctx, result, &wp, compression.RunCompressed, reason)
	}
	return result, s.fallback(ctx, result, describeFailure(lres))
}

// persistAttempt writes an attempt and, when it produced output, its
// compressed prompt, in one transaction, as soon as the attempt completes.
func (s *CompressionService) persistAttempt(ctx context.Context, p *preparation, a *compression.Attempt) (*compression.CompressedPrompt, error) {
	var cp *compression.CompressedPrompt
	if a.Output != "" {
		status := compression.ValidationFailed
		if a.Validation != nil {
			status = a.Validation.Status
		}
		c, err := compression.NewCompressedPrompt(compression.CompressedPrompt{
			CanonicalPromptID: p.canonical.ID, AttemptID: a.ID,
			CompressionInstructionVersion: p.instruction.Version,
			Provider:                      a.Provider, Model: a.Model, Attempt: a.Number,
			Content: a.Output, CanonicalTokens: p.tokens, ValidationStatus: status, CreatedAt: s.now(),
		})
		if err != nil {
			return nil, err
		}
		cp = &c
		id := c.ID
		a.CompressedPromptID = &id
	}
	err := s.inTx(ctx, func(repo persistence.CompressionRepository) error {
		if err := repo.CreateAttempt(ctx, p.canonical.ID, a); err != nil {
			return err
		}
		if cp != nil {
			return repo.CreateCompressedPrompt(ctx, cp)
		}
		return nil
	})
	if err != nil {
		return nil, translate(err)
	}
	return cp, nil
}

func describeFailure(l compression.LoopResult) string {
	if len(l.Attempts) == 0 {
		return "compression produced no attempt"
	}
	last := l.Attempts[len(l.Attempts)-1]
	switch {
	case l.Permanent:
		return fmt.Sprintf("compression failed: permanent provider error %s after %d attempt(s)", last.ErrorKind, len(l.Attempts))
	case last.Status == compression.AttemptProviderError:
		return fmt.Sprintf("compression failed: provider error %s after %d attempt(s)", last.ErrorKind, len(l.Attempts))
	case last.Status == compression.AttemptIneffective:
		return fmt.Sprintf("compression ineffective after %d attempt(s)", len(l.Attempts))
	default:
		return fmt.Sprintf("compression failed validation after %d attempt(s)", len(l.Attempts))
	}
}

func (s *CompressionService) compressor(st compression.Settings) (compression.Compressor, error) {
	if s.source == nil {
		return nil, errors.New("no compression providers configured")
	}
	if strings.TrimSpace(st.Provider) == "" {
		return nil, errors.New("project has no compression provider configured")
	}
	return s.source.Compressor(st.Provider)
}

func (s *CompressionService) pipeline(st compression.Settings) (compression.Pipeline, error) {
	p := compression.Pipeline{
		Semantic:        []compression.SemanticValidator{compression.DeterministicSemanticValidator{}},
		SemanticTimeout: st.Timeout,
	}
	if st.ValidationMode == compression.ValidationHybrid {
		v, err := s.source.SemanticValidator(st.Provider, st.Model)
		if err != nil {
			return compression.Pipeline{}, err
		}
		p.Semantic = append(p.Semantic, v)
	}
	return p, nil
}

// fallback applies the WBS 9.29 fallback policy after a failed/ineffective
// compression: canonical when it safely fits, else a FAILED run.
func (s *CompressionService) fallback(ctx context.Context, res *PreparationResult, why string) error {
	outcome, reason, err := compression.FallbackDecision(res.Run.CanonicalTokens, s.contextWindow(ctx, res.Run), why)
	if outcome == compression.RunFallbackCanonical {
		return s.finishCanonical(ctx, res, outcome, reason)
	}
	return s.fail(ctx, res, reason, err)
}

func (s *CompressionService) contextWindow(ctx context.Context, run *compression.Run) int {
	st, _, err := s.EffectiveSettings(ctx, run.ProjectID)
	if err != nil {
		return 0
	}
	return st.ContextWindow
}

func (s *CompressionService) fail(ctx context.Context, res *PreparationResult, reason string, cause error) error {
	if err := s.finishRun(context.WithoutCancel(ctx), res.Run, compression.RunFailed, reason); err != nil {
		return err
	}
	return cause
}

func (s *CompressionService) finishRun(ctx context.Context, run *compression.Run, outcome compression.RunOutcome, reason string) error {
	if err := s.repo.FinishRun(ctx, run.ID, outcome, reason, run.Attempts, s.now()); err != nil {
		return translate(err)
	}
	run.Outcome, run.Reason = outcome, reason
	return nil
}

func (s *CompressionService) finishCanonical(ctx context.Context, res *PreparationResult, outcome compression.RunOutcome, reason string) error {
	cp, err := s.canonical.Get(ctx, res.Run.CanonicalPromptID)
	if err != nil {
		return translate(err)
	}
	wp, err := compression.WorkerPromptFromCanonical(cp.ID, res.Run.ID, cp.Content, s.now())
	if err != nil {
		return err
	}
	return s.finishWithWorker(ctx, res, &wp, outcome, reason)
}

// finishWithWorker persists the worker prompt and finalizes the run in one
// transaction.
func (s *CompressionService) finishWithWorker(ctx context.Context, res *PreparationResult, wp *compression.WorkerPrompt, outcome compression.RunOutcome, reason string) error {
	run := res.Run
	err := s.inTx(ctx, func(repo persistence.CompressionRepository) error {
		if err := repo.CreateWorkerPrompt(ctx, run.ProjectID, wp); err != nil {
			return err
		}
		return repo.FinishRun(ctx, run.ID, outcome, reason, run.Attempts, s.now())
	})
	if err != nil {
		return translate(err)
	}
	id := wp.ID
	run.Outcome, run.Reason, run.WorkerPromptID = outcome, reason, &id
	res.WorkerPrompt = wp
	return nil
}

func (s *CompressionService) inTx(ctx context.Context, fn func(repo persistence.CompressionRepository) error) error {
	if s.txPool == nil {
		return fn(s.repo)
	}
	return dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		return fn(s.repo.WithTx(tx))
	})
}
