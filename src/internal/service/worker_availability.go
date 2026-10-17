package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"

	"overseer/internal/resilience"
	"overseer/internal/worker"
)

// ErrProviderUnavailable: the worker provider's circuit is open (repeated
// connectivity failures, WBS 18.26 / 18.27); no request is sent and the
// execution waits.
var ErrProviderUnavailable = errors.New("worker provider unavailable")

// ErrWorkerCapacity: the global or per-provider dispatch limit is reached
// (WBS 18.92); the work is queued (the execution waits).
var ErrWorkerCapacity = errors.New("worker dispatch capacity reached")

// AvailabilityConfig tunes provider admission (Phase 18).
type AvailabilityConfig struct {
	Breaker resilience.BreakerConfig
	// Breakers overrides the breaker per provider.
	Breakers map[string]resilience.BreakerConfig
	// MaxConcurrent bounds simultaneous engine dispatches across all
	// providers (0 = unlimited).
	MaxConcurrent int
	// ProviderMax bounds simultaneous dispatches per provider (0 = none).
	ProviderMax map[string]int
}

type availability struct {
	mu       sync.Mutex
	cfg      AvailabilityConfig
	breakers map[string]*resilience.Breaker
	active   int
	perProv  map[string]int
}

func (s *WorkerService) avail() *availability {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availability == nil {
		s.availability = &availability{breakers: map[string]*resilience.Breaker{}, perProv: map[string]int{}}
	}
	return s.availability
}

// WithAvailability configures admission control.
func (s *WorkerService) WithAvailability(cfg AvailabilityConfig) *WorkerService {
	a := s.avail()
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return s
}

func (a *availability) breaker(provider string) *resilience.Breaker {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.breakers[provider]
	if b == nil {
		cfg := a.cfg.Breaker
		if c, ok := a.cfg.Breakers[provider]; ok {
			cfg = c
		}
		b = resilience.NewBreaker(cfg)
		a.breakers[provider] = b
	}
	return b
}

// providerOf returns the project's worker provider name.
func (s *WorkerService) providerOf(ctx context.Context, projectID uuid.UUID) (string, error) {
	ps, err := s.d.ProjectSettings.Get(ctx, projectID.String())
	if err != nil {
		return "", translate(err)
	}
	if ps.WorkerProvider == "" {
		return "", ErrWorkerNotConfigured
	}
	return ps.WorkerProvider, nil
}

// Admit decides whether the engine may start a worker dispatch for the
// project now (WBS 18.26–18.28, 18.92): the provider's circuit must admit
// it (after the cooldown one probe — a connection health check, never a
// work request — decides) and a concurrency slot must be free. The
// returned release frees the slot.
func (s *WorkerService) Admit(ctx context.Context, projectID uuid.UUID) (func(), error) {
	prov, err := s.providerOf(ctx, projectID)
	if err != nil {
		return func() {}, nil // configuration problems surface at dispatch
	}
	a := s.avail()
	b := a.breaker(prov)
	adm := b.Allow(s.d.Now())
	if !adm.Allowed {
		_, _, why := b.Status()
		return nil, fmt.Errorf("%w: %s (%s)", ErrProviderUnavailable, prov, why)
	}
	if adm.Probe {
		if err := s.probe(ctx, projectID); err != nil {
			b.Failure(s.d.Now(), err.Error())
			return nil, fmt.Errorf("%w: %s probe failed: %v", ErrProviderUnavailable, prov, err)
		}
		b.Success(s.d.Now())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.MaxConcurrent > 0 && a.active >= a.cfg.MaxConcurrent {
		return nil, fmt.Errorf("%w: %d dispatches in flight (global limit)", ErrWorkerCapacity, a.active)
	}
	if lim := a.cfg.ProviderMax[prov]; lim > 0 && a.perProv[prov] >= lim {
		return nil, fmt.Errorf("%w: %d dispatches in flight on %s", ErrWorkerCapacity, a.perProv[prov], prov)
	}
	a.active++
	a.perProv[prov]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.active--
			a.perProv[prov]--
			a.mu.Unlock()
		})
	}, nil
}

// probe checks provider connectivity without sending work.
func (s *WorkerService) probe(ctx context.Context, projectID uuid.UUID) error {
	pw, err := s.worker(ctx, projectID)
	if err != nil {
		return err
	}
	if c, ok := pw.w.(worker.Connector); ok {
		return c.Connect(ctx)
	}
	return nil
}

// recordAvailability feeds the provider's circuit from a dispatch outcome:
// a response of any kind proves connectivity; transport failures (not
// sent / timeout / unknown delivery / provider internal error) count
// against it.
func (s *WorkerService) recordAvailability(ctx context.Context, projectID uuid.UUID, we *worker.WorkerError) {
	prov, err := s.providerOf(ctx, projectID)
	if err != nil {
		return
	}
	b := s.avail().breaker(prov)
	now := s.d.Now()
	if we == nil {
		b.Success(now)
		return
	}
	switch {
	case we.Delivery == worker.DeliveryNotSent && (we.Code == worker.CodeUnavailable || we.Code == worker.CodeTimeout),
		we.Delivery == worker.DeliveryUnknown, we.Code == worker.CodeInternal:
		b.Failure(now, string(we.Code)+": "+we.Message)
	case we.Delivery == worker.DeliveryReceived:
		b.Success(now)
	}
}

// ProviderAvailability reports every known provider's availability
// (health model, WBS 18.117).
func (s *WorkerService) ProviderAvailability() map[string]resilience.Availability {
	a := s.avail()
	a.mu.Lock()
	names := make([]string, 0, len(a.breakers))
	for n := range a.breakers {
		names = append(names, n)
	}
	a.mu.Unlock()
	sort.Strings(names)
	out := map[string]resilience.Availability{}
	for _, n := range names {
		st, _, _ := a.breaker(n).Status()
		out[n] = st
	}
	return out
}

// InFlight returns the number of admitted engine dispatches.
func (s *WorkerService) InFlight() int {
	a := s.avail()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}
