package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"overseer/internal/compression"
	"overseer/internal/resilience"
)

// ErrCompressionDeferred: speculative (precompute) compression yielded its
// capacity to active execution (WBS 18.95). Nothing failed; the work is
// simply not done now.
var ErrCompressionDeferred = errors.New("compression deferred: capacity reserved for active execution")

// CompressionAvailabilityConfig tunes compression admission (Phase 18,
// WBS 18.29 / 18.94 / 18.95).
type CompressionAvailabilityConfig struct {
	Breaker resilience.BreakerConfig
	// Breakers overrides the breaker per provider.
	Breakers map[string]resilience.BreakerConfig
	// MaxConcurrent bounds simultaneous compression provider runs
	// (0 = unlimited).
	MaxConcurrent int
}

// compressionGuard holds the per-provider breakers and the shared
// priority limiter. The zero value (no WithAvailability) admits
// everything: no breaker, no limit (Phase 9/10 behaviour).
type compressionGuard struct {
	mu       sync.Mutex
	enabled  bool
	cfg      CompressionAvailabilityConfig
	breakers map[string]*resilience.Breaker
	limiter  *resilience.PriorityLimiter
}

// WithAvailability configures compression admission.
func (s *CompressionService) WithAvailability(cfg CompressionAvailabilityConfig) *CompressionService {
	g := &s.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enabled, g.cfg = true, cfg
	g.breakers = nil
	g.limiter = nil
	if cfg.MaxConcurrent > 0 {
		g.limiter = resilience.NewPriorityLimiter(cfg.MaxConcurrent)
	}
	return s
}

func (g *compressionGuard) breaker(provider string) *resilience.Breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.breakers == nil {
		g.breakers = map[string]*resilience.Breaker{}
	}
	b := g.breakers[provider]
	if b == nil {
		cfg := g.cfg.Breaker
		if c, ok := g.cfg.Breakers[provider]; ok {
			cfg = c
		}
		b = resilience.NewBreaker(cfg)
		g.breakers[provider] = b
	}
	return b
}

// acquire takes a compression slot at the context's priority. Speculative
// work that cannot run now gets ErrCompressionDeferred.
func (s *CompressionService) acquire(ctx context.Context) (context.Context, func(), error) {
	s.guard.mu.Lock()
	l := s.guard.limiter
	s.guard.mu.Unlock()
	cctx, release, err := l.Acquire(ctx, resilience.PriorityOf(ctx))
	if errors.Is(err, resilience.ErrSaturated) {
		return nil, nil, ErrCompressionDeferred
	}
	return cctx, release, err
}

// deferredErr maps a speculative run cancelled by preemption to
// ErrCompressionDeferred.
func deferredErr(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), resilience.ErrPreempted) {
		return fmt.Errorf("%w: %v", ErrCompressionDeferred, err)
	}
	return err
}

// ProviderAvailability reports every known compression provider's
// availability (health model, WBS 18.117).
func (s *CompressionService) ProviderAvailability() map[string]resilience.Availability {
	g := &s.guard
	g.mu.Lock()
	names := make([]string, 0, len(g.breakers))
	for n := range g.breakers {
		names = append(names, n)
	}
	g.mu.Unlock()
	sort.Strings(names)
	out := map[string]resilience.Availability{}
	for _, n := range names {
		st, _, _ := g.breaker(n).Status()
		out[n] = st
	}
	return out
}

// guardedCompressor wraps a provider with its circuit breaker: the run is
// admitted before its first call (a HALF_OPEN admission makes that call
// the probe); each later call is re-admitted, so a circuit that opens
// mid-run stops the remaining attempts without a request (CIRCUIT_OPEN,
// not retryable). Connectivity failures count against the circuit; any
// provider response proves connectivity.
type guardedCompressor struct {
	inner       compression.Compressor
	b           *resilience.Breaker
	now         func() time.Time
	mu          sync.Mutex
	preAdmitted bool // the first call was admitted by admitCompression
	probe       bool // the admitted probe has not produced a verdict yet
}

// admitCompression decides whether the provider may be called for this
// run. ok=false returns the reason (the circuit is open): the caller falls
// back without any provider call (WBS 18.29).
func (s *CompressionService) admitCompression(provider string, c compression.Compressor) (*guardedCompressor, string, bool) {
	s.guard.mu.Lock()
	enabled := s.guard.enabled
	s.guard.mu.Unlock()
	if !enabled {
		return &guardedCompressor{inner: c}, "", true
	}
	b := s.guard.breaker(provider)
	adm := b.Allow(s.now())
	if !adm.Allowed {
		_, _, why := b.Status()
		return nil, fmt.Sprintf("compression provider %s unavailable (circuit open: %s)", provider, why), false
	}
	return &guardedCompressor{inner: c, b: b, now: s.now, preAdmitted: true, probe: adm.Probe}, "", true
}

func (g *guardedCompressor) Compress(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
	if g.b == nil {
		return g.inner.Compress(ctx, req)
	}
	g.mu.Lock()
	if !g.preAdmitted {
		if !g.b.Allow(g.now()).Allowed {
			g.mu.Unlock()
			return compression.CompressionResponse{}, compression.NewError(compression.ErrorCircuitOpen, "provider circuit opened during the run; not sent")
		}
	}
	g.preAdmitted = false
	g.mu.Unlock()
	resp, err := g.inner.Compress(ctx, req)
	g.record(ctx, err)
	return resp, err
}

func (g *guardedCompressor) record(ctx context.Context, err error) {
	g.mu.Lock()
	g.probe = false
	g.mu.Unlock()
	switch k := compression.KindOf(err); {
	case err == nil:
		g.b.Success(g.now())
	case errors.Is(ctx.Err(), context.Canceled):
		g.b.Release() // our own cancellation: no verdict
	case k == compression.ErrorNetwork || k == compression.ErrorUnavailable || k == compression.ErrorTimeout,
		errors.Is(ctx.Err(), context.DeadlineExceeded):
		g.b.Failure(g.now(), string(k))
	default:
		g.b.Success(g.now()) // the provider answered
	}
}

// done frees an admitted probe that never reached the provider.
func (g *guardedCompressor) done() {
	if g.b == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probe {
		g.probe = false
		g.b.Release()
	}
}
