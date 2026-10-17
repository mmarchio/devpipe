package continuation

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// Decision is the persisted strategy decision (WBS 14.15).
type Decision struct {
	Strategy Strategy
	Reason   DecisionReason

	ContextTokens *int64
	ContextWindow *int64
	Utilization   *float64

	Threshold float64

	Estimated bool
	Source    worker.UsageSource
}

// DecisionContext are the pure, authoritative decision inputs (WBS 14.4).
// Worker-authored text is deliberately absent: next_action and any
// worker claim about context can never select a strategy (WBS 14.121,
// 14.122).
type DecisionContext struct {
	ResultStatus workerresult.WorkerResultStatus
	Trigger      Trigger

	Reading   ContextReading
	Threshold float64

	SessionStatus worker.SessionStatus

	Settings Settings

	// Override is an operator-requested strategy ("" none).
	Override Strategy

	ContinuationAttempts int
	SessionResets        int
}

// Policy decides how an unfinished work unit continues.
type Policy interface {
	Decide(ctx DecisionContext) (Decision, error)
}

// DefaultPolicy is the WBS 14 policy; it is the only place threshold
// comparison happens.
type DefaultPolicy struct{}

// NoContinuationError is returned for results that never continue
// automatically (WBS 14.10): COMPLETE, BLOCKED and FAILED.
type NoContinuationError struct {
	Status workerresult.WorkerResultStatus
}

func (e *NoContinuationError) Error() string {
	return fmt.Sprintf("continuation: %s result does not continue automatically", e.Status)
}

// Decision errors.
var (
	ErrInvalidThreshold = errors.New("continuation: threshold must satisfy 0 < threshold <= 1")
	ErrMissingResult    = errors.New("continuation: an INCOMPLETE trigger requires the result status")
	ErrUnsafeOverride   = errors.New("continuation: same-session override is unsafe")
)

// CompareThreshold compares tokens/window with threshold without float
// drift: the threshold is taken at 10^-6 resolution and compared in
// integers (PLAN Decision 5). It returns -1, 0 or 1.
func CompareThreshold(tokens, window int64, threshold float64) int {
	micro := big.NewInt(int64(math.Round(threshold * 1e6)))
	lhs := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(1_000_000))
	rhs := new(big.Int).Mul(micro, big.NewInt(window))
	return lhs.Cmp(rhs)
}

// Decide applies the WBS 14 policy.
func (DefaultPolicy) Decide(c DecisionContext) (Decision, error) {
	if c.ResultStatus != "" && c.ResultStatus != workerresult.StatusIncomplete {
		return Decision{}, &NoContinuationError{Status: c.ResultStatus}
	}
	if c.Trigger == TriggerResultIncomplete && c.ResultStatus == "" {
		return Decision{}, ErrMissingResult
	}
	if !ValidThreshold(c.Threshold) {
		return Decision{}, ErrInvalidThreshold
	}
	d := Decision{Threshold: c.Threshold, Source: c.Reading.Source, Estimated: c.Reading.Estimated}
	if c.Reading.Known {
		d.ContextTokens, d.ContextWindow, d.Utilization = copyI(c.Reading.Tokens), copyI(c.Reading.Window), copyF(c.Reading.Utilization)
	}
	if d.Source == "" {
		d.Source = worker.SourceUnknown
	}
	switch {
	case c.Trigger == TriggerContextLimit:
		d.Strategy, d.Reason = ContinueFreshSession, ReasonContextLimit
	case c.Trigger == TriggerSessionLost || c.SessionStatus != worker.SessionActive:
		d.Strategy, d.Reason = ContinueFreshSession, ReasonSessionLost
	case !c.Reading.Known || (c.Reading.Estimated && !c.Settings.EstimatedMayReset):
		d.Strategy, d.Reason = c.Settings.UnknownContextPolicy, ReasonContextUnknown
		if !d.Strategy.Valid() {
			d.Strategy = ContinueFreshSession
		}
	default:
		switch CompareThreshold(*c.Reading.Tokens, *c.Reading.Window, c.Threshold) {
		case -1:
			d.Strategy, d.Reason = ContinueSameSession, ReasonBelowThreshold
		case 0:
			d.Strategy, d.Reason = ContinueFreshSession, ReasonAtThreshold
		default:
			d.Strategy, d.Reason = ContinueFreshSession, ReasonAboveThreshold
		}
	}
	switch c.Override {
	case ContinueFreshSession:
		if d.Strategy != ContinueFreshSession {
			d.Strategy, d.Reason = ContinueFreshSession, ReasonOperatorOverride
		}
	case ContinueSameSession:
		if d.Strategy == ContinueFreshSession {
			if d.Reason != ReasonContextUnknown {
				return Decision{}, ErrUnsafeOverride
			}
			d.Strategy, d.Reason = ContinueSameSession, ReasonOperatorOverride
		}
	}
	return d, nil
}

func copyI(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyF(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
