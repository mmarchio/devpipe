package worker

import (
	"errors"
	"math"
)

// ErrInvalidContextWindow is returned for a zero or negative window.
var ErrInvalidContextWindow = errors.New("worker: context window must be positive")

// Utilization is context usage (WBS 12.14). Known is false when either
// value is missing — never reported as 0%. OverLimit flags a reading above
// 100% (kept as reported, not clamped).
type Utilization struct {
	Known     bool
	Tokens    int64
	Window    int64
	Percent   float64
	OverLimit bool
}

// Utilization computes context_tokens / context_window when both are
// present. A non-positive window is an error; a negative token count is
// treated as unknown.
func (u WorkerUsage) Utilization() (Utilization, error) {
	if u.ContextWindow == nil || u.ContextTokens == nil {
		return Utilization{}, nil
	}
	if *u.ContextWindow <= 0 {
		return Utilization{}, ErrInvalidContextWindow
	}
	if *u.ContextTokens < 0 {
		return Utilization{}, nil
	}
	p := float64(*u.ContextTokens) / float64(*u.ContextWindow) * 100
	return Utilization{Known: true, Tokens: *u.ContextTokens, Window: *u.ContextWindow,
		Percent: math.Round(p*100) / 100, OverLimit: *u.ContextTokens > *u.ContextWindow}, nil
}

// UsageSource names where telemetry came from (WBS 12.15).
type UsageSource string

// Telemetry sources in precedence order.
const (
	SourceProviderActual  UsageSource = "PROVIDER_ACTUAL"
	SourceProviderDerived UsageSource = "PROVIDER_DERIVED"
	SourceEstimated       UsageSource = "ESTIMATED"
	SourceUnknown         UsageSource = "UNKNOWN"
)

// HasContext reports whether the usage carries a context reading.
func (u WorkerUsage) HasContext() bool { return u.ContextTokens != nil }

// HasAny reports whether any token value is present.
func (u WorkerUsage) HasAny() bool {
	return u.ContextTokens != nil || u.InputTokens != nil || u.OutputTokens != nil
}

// SelectUsage applies the telemetry precedence (WBS 12.15): gateway-actual
// context usage, then provider token usage sufficient to derive context
// (input + output), then the overseer estimate, then unknown. The estimate
// is always flagged Estimated; provider values never are.
func SelectUsage(actual, provider *WorkerUsage, estimate *WorkerUsage) (WorkerUsage, UsageSource) {
	if actual != nil && actual.HasContext() && !actual.Estimated {
		return *actual, SourceProviderActual
	}
	if provider != nil && !provider.Estimated && provider.InputTokens != nil {
		out := *provider
		if out.ContextTokens == nil {
			ctx := *provider.InputTokens
			if provider.OutputTokens != nil {
				ctx += *provider.OutputTokens
			}
			out.ContextTokens = Int64(ctx)
		}
		return out, SourceProviderDerived
	}
	if estimate != nil && estimate.HasAny() {
		out := *estimate
		out.Estimated = true
		return out, SourceEstimated
	}
	return WorkerUsage{}, SourceUnknown
}
