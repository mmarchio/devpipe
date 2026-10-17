package continuation

import (
	"errors"
	"time"

	"overseer/internal/worker"
)

// ErrInvalidWindow reports a non-positive context window (T14.6.4).
var ErrInvalidWindow = errors.New("continuation: context window must be positive")

// ContextReading is the authoritative context telemetry a decision uses
// (WBS 14.6). It is built only from Phase 12 usage — never from
// worker-authored result text. Unknown readings have nil Utilization.
type ContextReading struct {
	Known       bool
	Tokens      *int64
	Window      *int64
	Utilization *float64
	Estimated   bool
	Source      worker.UsageSource
}

// UnknownReading is the reading when no usable telemetry exists.
func UnknownReading() ContextReading { return ContextReading{Source: worker.SourceUnknown} }

// ReadingFromUsage computes utilization = context_tokens / context_window.
// A missing value or negative token count is unknown; a non-positive
// window is an error.
func ReadingFromUsage(u worker.WorkerUsage, src worker.UsageSource) (ContextReading, error) {
	if u.ContextWindow != nil && *u.ContextWindow <= 0 {
		return ContextReading{}, ErrInvalidWindow
	}
	if src == worker.SourceUnknown || u.ContextTokens == nil || u.ContextWindow == nil || *u.ContextTokens < 0 {
		return UnknownReading(), nil
	}
	t, w := *u.ContextTokens, *u.ContextWindow
	ut := float64(t) / float64(w)
	return ContextReading{Known: true, Tokens: &t, Window: &w, Utilization: &ut,
		Estimated: u.Estimated || src == worker.SourceEstimated, Source: src}, nil
}

// Observation is one Phase 12 usage observation.
type Observation struct {
	Usage      worker.WorkerUsage
	Source     worker.UsageSource
	Seq        int64
	ObservedAt time.Time
}

func sourceRank(s worker.UsageSource) int {
	switch s {
	case worker.SourceProviderActual:
		return 3
	case worker.SourceProviderDerived:
		return 2
	case worker.SourceEstimated:
		return 1
	}
	return 0
}

// SelectReading applies the Phase 12 precedence (WBS 14.11): actual
// provider telemetry, then derived, then the overseer estimate, then
// unknown; the latest observation wins within one source. Observations
// without a usable reading are skipped.
func SelectReading(obs []Observation) ContextReading {
	best := UnknownReading()
	bestRank, bestSeq := -1, int64(0)
	var bestAt time.Time
	for _, o := range obs {
		r, err := ReadingFromUsage(o.Usage, o.Source)
		if err != nil || !r.Known {
			continue
		}
		rank := sourceRank(o.Source)
		if r.Estimated && rank > 1 {
			rank = 1
		}
		later := o.Seq > bestSeq || (o.Seq == bestSeq && o.ObservedAt.After(bestAt))
		if rank > bestRank || (rank == bestRank && later) {
			best, bestRank, bestSeq, bestAt = r, rank, o.Seq, o.ObservedAt
		}
	}
	return best
}
