package compression

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AttemptLoop runs compression attempts against the canonical source (WBS
// 9.26-9.28, 9.36-9.38):
//   - attempts are numbered from 1; MaxAttempts is a hard stop;
//   - every attempt is built from the canonical components (the structured
//     input is rendered once); a failed output is never the next source;
//   - a validation failure retries with concise feedback;
//   - a retryable provider failure retries after a bounded backoff; a
//     permanent provider failure stops immediately;
//   - each attempt runs under its own timeout;
//   - every attempt is handed to the sink as soon as it completes.
type AttemptLoop struct {
	Compressor Compressor
	Pipeline   Pipeline
	Sleeper    Sleeper
	Backoff    Backoff

	MaxAttempts int
	Timeout     time.Duration

	Provider string
	Model    string

	Now func() time.Time
}

// LoopInput is the canonical material of one run.
type LoopInput struct {
	CanonicalPromptID uuid.UUID
	CanonicalTokens   int

	// Components are the effective-policy components sent for compression
	// (after deterministic optional drops); Canonical is the full set.
	Components []CompressionComponent
	Canonical  []CompressionComponent
	Manifest   Manifest

	Instruction Instruction

	TargetTokens   int
	MinimumSavings float64

	// AllowPlaceholders lets placeholder markers through validation — only
	// for compressing a pending precomputed template (Phase 11).
	AllowPlaceholders bool
}

// AttemptSink receives each completed attempt; it may persist it and set
// IDs. A sink error aborts the loop (infrastructure failure).
type AttemptSink func(ctx context.Context, a *Attempt) error

// LoopResult is the loop outcome.
type LoopResult struct {
	Attempts []Attempt
	// Accepted is the accepted attempt (nil when none passed).
	Accepted *Attempt
	// LastProviderError is the last provider failure (nil if none).
	LastProviderError error
	// Permanent reports a permanent provider failure stopped the loop.
	Permanent bool
}

// ErrLoopMisconfigured reports an unusable loop configuration.
var ErrLoopMisconfigured = errors.New("compression: attempt loop misconfigured")

// Run executes the loop. It returns an error only for caller cancellation,
// a sink failure, or misconfiguration; compression and validation failures
// are reported in the result.
func (l AttemptLoop) Run(ctx context.Context, in LoopInput, sink AttemptSink) (LoopResult, error) {
	var res LoopResult
	if l.Compressor == nil || l.MaxAttempts < 1 || l.Timeout <= 0 {
		return res, ErrLoopMisconfigured
	}
	now := l.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	sleeper := l.Sleeper
	if sleeper == nil {
		sleeper = RealSleeper{}
	}
	structured := RenderStructuredInput(in.Components)
	feedback := ""
	for n := 1; n <= l.MaxAttempts; n++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		req, err := NewCompressionRequest(CompressionRequest{
			CanonicalPromptID:  in.CanonicalPromptID,
			Content:            structured.Text,
			Components:         in.Components,
			TargetTokens:       in.TargetTokens,
			Model:              l.Model,
			Attempt:            n,
			InstructionContent: in.Instruction.Content,
			Feedback:           feedback,
		})
		if err != nil {
			return res, err
		}
		actx, cancel := context.WithTimeout(ctx, l.Timeout)
		start := time.Now()
		resp, cerr := l.Compressor.Compress(actx, req)
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded)
		cancel()
		dur := time.Since(start)
		if resp.Duration > 0 {
			dur = resp.Duration
		}
		if cerr == nil && strings.TrimSpace(resp.Content) == "" {
			cerr = NewError(ErrorEmpty, "compressor returned empty content")
		}
		a := Attempt{
			ID: uuid.New(), Number: n, Provider: l.Provider, Model: l.Model,
			InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
			FinishReason: resp.FinishReason, ProviderRequestID: resp.ProviderRequestID,
			Duration: dur, CreatedAt: now(),
		}
		if resp.Provider != "" {
			a.Provider = resp.Provider
		}
		if resp.Model != "" {
			a.Model = resp.Model
		}
		if cerr != nil {
			kind := KindOf(cerr)
			if ctx.Err() != nil {
				kind = ErrorCancelled
			} else if timedOut && (kind == ErrorUnknown || kind == ErrorCancelled) {
				kind = ErrorTimeout
			}
			a.Status = AttemptProviderError
			a.ErrorKind = kind
			a.ErrorMessage = truncate(cerr.Error(), 500)
			res.LastProviderError = &CompressionError{Kind: kind, Message: a.ErrorMessage}
			if err := l.emit(ctx, sink, &a, &res); err != nil {
				return res, err
			}
			if kind == ErrorCancelled && ctx.Err() != nil {
				return res, ctx.Err()
			}
			if !kind.Retryable() {
				res.Permanent = true
				return res, nil
			}
			if n < l.MaxAttempts {
				if err := sleeper.Sleep(ctx, l.Backoff.Delay(n)); err != nil {
					return res, err
				}
			}
			continue
		}
		vres := l.Pipeline.Validate(ctx, ValidationInput{
			Components:      in.Components,
			Canonical:       in.Canonical,
			Manifest:        in.Manifest,
			Boundary:        structured.Boundary,
			Compressed:      resp.Content,
			CanonicalTokens: in.CanonicalTokens,
			TargetTokens:    in.TargetTokens,
			MinimumSavings:  in.MinimumSavings,

			AllowPlaceholders: in.AllowPlaceholders,
		})
		a.Output = resp.Content
		a.Validation = &vres
		switch vres.Status {
		case ValidationPassed:
			a.Status = AttemptAccepted
		case ValidationIneffective:
			a.Status = AttemptIneffective
		default:
			a.Status = AttemptValidationFailed
		}
		if err := l.emit(ctx, sink, &a, &res); err != nil {
			return res, err
		}
		if a.Status == AttemptAccepted {
			acc := res.Attempts[len(res.Attempts)-1]
			res.Accepted = &acc
			return res, nil
		}
		feedback = RetryFeedback(vres)
	}
	return res, nil
}

func (l AttemptLoop) emit(ctx context.Context, sink AttemptSink, a *Attempt, res *LoopResult) error {
	if sink != nil {
		if err := sink(ctx, a); err != nil {
			return err
		}
	}
	res.Attempts = append(res.Attempts, *a)
	return nil
}
