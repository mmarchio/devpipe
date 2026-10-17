package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/persistence"
)

// ---------------------------------------------------------------------------
// WBS 9.45: compression preview — runs the decision + compression +
// validation for a canonical prompt WITHOUT persisting anything and without
// touching workflow state.

// CompressionPreview is the preview result.
type CompressionPreview struct {
	CanonicalPromptID uuid.UUID
	Decision          compression.Action
	Reason            string

	CanonicalTokens  int
	CompressedTokens int
	TargetTokens     int
	Metrics          compression.Metrics

	ValidationStatus compression.ValidationStatus
	Items            []compression.ItemResult
	Failures         []compression.Failure
	Attempts         int
	Dropped          []compression.DroppedComponent

	// Preview is the worker-ready content the preparation would produce
	// (compressed when accepted, else canonical when it safely fits, else
	// empty).
	Preview string
	Source  compression.WorkerPromptSource
}

// PreviewCompression implements WBS 9.45.
func (s *CompressionService) PreviewCompression(ctx context.Context, projectID, canonicalID uuid.UUID) (*CompressionPreview, error) {
	p, err := s.load(ctx, projectID, canonicalID)
	if err != nil {
		return nil, err
	}
	st := p.settings
	out := &CompressionPreview{CanonicalPromptID: canonicalID, CanonicalTokens: p.tokens, TargetTokens: st.EffectiveTarget()}
	d, derr := compression.Decide(st, p.tokens)
	if derr != nil {
		out.Reason = derr.Error()
		out.ValidationStatus = compression.ValidationFailed
		return out, nil
	}
	out.Decision, out.Reason = d.Action, d.Reason
	canonicalOut := func(why string) (*CompressionPreview, error) {
		if why != "" {
			out.Reason = why
		}
		if p.tokens <= st.ContextWindow {
			out.Preview, out.Source = p.canonical.Content, compression.SourceCanonical
		}
		out.Metrics = compression.ComputeMetrics(p.tokens, p.tokens, 0, out.Attempts)
		out.CompressedTokens = p.tokens
		return out, nil
	}
	if d.Action == compression.ActionBypass {
		out.ValidationStatus = compression.ValidationPassed
		return canonicalOut("")
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
		out.ValidationStatus = compression.ValidationFailed
		return canonicalOut("canonical prompt has no persisted components")
	}
	comps = compression.ApplyEffectivePolicies(comps)
	if p.tokens > out.TargetTokens {
		if err := compression.CheckTargetFeasible(comps, out.TargetTokens); err != nil {
			out.ValidationStatus = compression.ValidationFailed
			out.Reason = err.Error()
			return out, nil
		}
	}
	kept, dropped := compression.DropOptional(comps, out.TargetTokens)
	out.Dropped = dropped
	compressor, err := s.compressor(st)
	if err != nil {
		out.ValidationStatus = compression.ValidationFailed
		return canonicalOut("compression provider unavailable: " + err.Error())
	}
	pipeline, err := s.pipeline(st)
	if err != nil {
		out.ValidationStatus = compression.ValidationFailed
		return canonicalOut("semantic validator unavailable: " + err.Error())
	}
	loop := compression.AttemptLoop{
		Compressor: compressor, Pipeline: pipeline, Sleeper: s.sleeper,
		Backoff:     compression.Backoff{Base: st.BackoffBase, Max: st.BackoffMax},
		MaxAttempts: st.MaxAttempts, Timeout: st.Timeout, Provider: st.Provider, Model: st.Model, Now: s.now,
	}
	start := time.Now()
	lres, lerr := loop.Run(ctx, compression.LoopInput{
		CanonicalPromptID: canonicalID, CanonicalTokens: p.tokens,
		Components: kept, Canonical: comps, Manifest: manifest, Instruction: p.instruction,
		TargetTokens: out.TargetTokens, MinimumSavings: st.MinimumSavings,
	}, nil)
	if lerr != nil {
		return nil, lerr
	}
	out.Attempts = len(lres.Attempts)
	// Report the accepted attempt, else the last attempt that produced
	// validation results.
	var shown *compression.Attempt
	if lres.Accepted != nil {
		shown = lres.Accepted
	} else {
		for i := len(lres.Attempts) - 1; i >= 0; i-- {
			if lres.Attempts[i].Validation != nil {
				a := lres.Attempts[i]
				shown = &a
				break
			}
		}
	}
	if shown != nil && shown.Validation != nil {
		out.ValidationStatus = shown.Validation.Status
		out.Items, out.Failures = shown.Validation.Items, shown.Validation.Failures
	} else {
		out.ValidationStatus = compression.ValidationFailed
	}
	if lres.Accepted != nil {
		out.CompressedTokens = compression.EstimateTokens(lres.Accepted.Output)
		out.Metrics = compression.ComputeMetrics(p.tokens, out.CompressedTokens, time.Since(start), out.Attempts)
		out.Preview, out.Source = lres.Accepted.Output, compression.SourceCompressed
		return out, nil
	}
	return canonicalOut(describeFailure(lres))
}

// ---------------------------------------------------------------------------
// WBS 9.46: compression attempt history.

// AttemptHistoryItem is one attempt with its run context.
type AttemptHistoryItem struct {
	Attempt compression.Attempt
	Run     *compression.Run
}

// ListAttempts returns every attempt of a canonical prompt in order (run
// creation, then attempt number), project-scoped (T9.46.4).
func (s *CompressionService) ListAttempts(ctx context.Context, projectID, canonicalID uuid.UUID) ([]AttemptHistoryItem, error) {
	if err := s.checkCanonical(ctx, projectID, canonicalID); err != nil {
		return nil, err
	}
	runs, err := s.repo.ListRunsByCanonical(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	byID := map[uuid.UUID]*compression.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	atts, err := s.repo.ListAttemptsByCanonical(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]AttemptHistoryItem, 0, len(atts))
	for _, a := range atts {
		out = append(out, AttemptHistoryItem{Attempt: a, Run: byID[a.RunID]})
	}
	return out, nil
}

// ListRuns returns the canonical prompt's preparation runs.
func (s *CompressionService) ListRuns(ctx context.Context, projectID, canonicalID uuid.UUID) ([]*compression.Run, error) {
	if err := s.checkCanonical(ctx, projectID, canonicalID); err != nil {
		return nil, err
	}
	runs, err := s.repo.ListRunsByCanonical(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	return runs, nil
}

func (s *CompressionService) checkCanonical(ctx context.Context, projectID, canonicalID uuid.UUID) error {
	cp, err := s.canonical.Get(ctx, canonicalID)
	if err != nil {
		return translate(err)
	}
	if cp.ProjectID != projectID {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// WBS 9.47: aggregate compression metrics.

// CompressionMetrics is the project aggregate.
type CompressionMetrics struct {
	Runs                  int
	PromptsCompressed     int
	PromptsBypassed       int
	PromptsFallback       int
	PromptsFailed         int
	AverageCanonicalTok   float64
	AverageCompressedTok  float64
	AverageReduction      float64
	Attempts              int
	ValidationFailures    int
	ValidationFailureRate float64
	RetryRate             float64
	FallbackRate          float64
}

// ProjectMetrics computes the aggregate metrics from the persisted runs and
// attempts. Only accepted attempts count as compression (T9.47.4).
func (s *CompressionService) ProjectMetrics(ctx context.Context, projectID uuid.UUID) (*CompressionMetrics, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return nil, translate(err)
	}
	runs, err := s.repo.ListRunsByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	atts, err := s.repo.ListAttemptsByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	accepted := map[uuid.UUID]compression.Attempt{}
	var outputs int
	m := &CompressionMetrics{Runs: len(runs), Attempts: len(atts)}
	for _, a := range atts {
		if a.Status == compression.AttemptAccepted {
			accepted[a.RunID] = a
		}
		if a.Validation != nil {
			outputs++
			if !a.Validation.Passed() {
				m.ValidationFailures++
			}
		}
	}
	var sumCanon, sumComp, sumRed float64
	attemptedRuns, retried := 0, 0
	for _, r := range runs {
		switch r.Outcome {
		case compression.RunBypassed:
			m.PromptsBypassed++
		case compression.RunFallbackCanonical:
			m.PromptsFallback++
		case compression.RunFailed:
			m.PromptsFailed++
		case compression.RunCompressed:
			a, ok := accepted[r.ID]
			if !ok || a.Validation == nil {
				continue
			}
			m.PromptsCompressed++
			sumCanon += float64(r.CanonicalTokens)
			sumComp += float64(a.Validation.Output)
			sumRed += compression.ComputeMetrics(r.CanonicalTokens, a.Validation.Output, 0, 0).PercentSaved
		}
		if r.Attempts > 0 {
			attemptedRuns++
			if r.Attempts > 1 {
				retried++
			}
		}
	}
	if m.PromptsCompressed > 0 {
		n := float64(m.PromptsCompressed)
		m.AverageCanonicalTok, m.AverageCompressedTok, m.AverageReduction = sumCanon/n, sumComp/n, sumRed/n
	}
	if outputs > 0 {
		m.ValidationFailureRate = float64(m.ValidationFailures) / float64(outputs)
	}
	if attemptedRuns > 0 {
		m.RetryRate = float64(retried) / float64(attemptedRuns)
		m.FallbackRate = float64(m.PromptsFallback) / float64(attemptedRuns)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// WBS 9.48: canonical vs compressed diff.

// AttemptDiff returns the deterministic line diff between the canonical
// component text and one attempt's compressed output (T9.48.1-T9.48.4).
func (s *CompressionService) AttemptDiff(ctx context.Context, projectID, canonicalID, attemptID uuid.UUID) (*compression.Diff, error) {
	if err := s.checkCanonical(ctx, projectID, canonicalID); err != nil {
		return nil, err
	}
	atts, err := s.repo.ListAttemptsByCanonical(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	var att *compression.Attempt
	for i := range atts {
		if atts[i].ID == attemptID {
			att = &atts[i]
		}
	}
	if att == nil {
		return nil, ErrNotFound
	}
	comps, err := s.components.ListComponents(ctx, canonicalID)
	if err != nil {
		return nil, translate(err)
	}
	d := compression.LineDiff(compression.ComponentsText(comps), att.Output)
	return &d, nil
}

// ---------------------------------------------------------------------------
// WBS 9.34/9.59: lineage.

// CompressionLineage traces a worker prompt back to its context snapshot.
type CompressionLineage struct {
	WorkerPrompt      *compression.WorkerPrompt
	CompressedPrompt  *compression.CompressedPrompt
	AcceptedAttempt   *compression.Attempt
	Run               *compression.Run
	Attempts          []compression.Attempt
	Canonical         *persistence.CanonicalPromptRecord
	ContextSnapshotID uuid.UUID
}

// Lineage returns the full chain for a worker prompt (project-scoped).
func (s *CompressionService) Lineage(ctx context.Context, projectID, workerPromptID uuid.UUID) (*CompressionLineage, error) {
	wp, err := s.repo.GetWorkerPrompt(ctx, workerPromptID)
	if err != nil {
		return nil, translate(err)
	}
	cp, err := s.canonical.Get(ctx, wp.CanonicalPromptID)
	if err != nil {
		return nil, translate(err)
	}
	if cp.ProjectID != projectID {
		return nil, ErrNotFound
	}
	run, err := s.repo.GetRun(ctx, wp.RunID)
	if err != nil {
		return nil, translate(err)
	}
	atts, err := s.repo.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		return nil, translate(err)
	}
	out := &CompressionLineage{WorkerPrompt: wp, Run: run, Attempts: atts, Canonical: cp, ContextSnapshotID: cp.ContextSnapshotID}
	if wp.CompressedPromptID != nil {
		c, err := s.repo.GetCompressedPrompt(ctx, *wp.CompressedPromptID)
		if err != nil {
			return nil, translate(err)
		}
		if c.CanonicalPromptID != cp.ID {
			return nil, errors.New("service: compressed prompt references a different canonical prompt")
		}
		out.CompressedPrompt = c
		for i := range atts {
			if atts[i].ID == c.AttemptID {
				a := atts[i]
				out.AcceptedAttempt = &a
			}
		}
	}
	return out, nil
}

// ProjectRuns returns a page of the project's preparation runs, newest
// first, with the total count (console Compressor tab).
func (s *CompressionService) ProjectRuns(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]*compression.Run, int, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return nil, 0, translate(err)
	}
	runs, err := s.repo.ListRunsByProject(ctx, projectID)
	if err != nil {
		return nil, 0, translate(err)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	total := len(runs)
	if offset > total {
		offset = total
	}
	return runs[offset:min(offset+limit, total)], total, nil
}

// ProjectAttempts returns the project's latest compression attempts,
// newest first, at most limit.
func (s *CompressionService) ProjectAttempts(ctx context.Context, projectID uuid.UUID, limit int) ([]compression.Attempt, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return nil, translate(err)
	}
	atts, err := s.repo.ListAttemptsByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	sort.SliceStable(atts, func(i, j int) bool { return atts[i].CreatedAt.After(atts[j].CreatedAt) })
	return atts[:min(limit, len(atts))], nil
}
