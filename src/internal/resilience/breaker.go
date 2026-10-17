package resilience

import (
	"sync"
	"time"
)

// Availability is a provider's availability (WBS 18.26).
type Availability string

const (
	Healthy     Availability = "HEALTHY"
	Degraded    Availability = "DEGRADED"
	Unavailable Availability = "UNAVAILABLE"
)

// BreakerState is the circuit state.
type BreakerState string

const (
	BreakerClosed   BreakerState = "CLOSED"
	BreakerOpen     BreakerState = "OPEN"
	BreakerHalfOpen BreakerState = "HALF_OPEN"
)

// BreakerConfig bounds a circuit breaker (WBS 18.27 / 18.28).
type BreakerConfig struct {
	// Failures consecutive failures (within Window) open the circuit.
	Failures int
	Window   time.Duration
	// Cooldown before a probe is allowed.
	Cooldown time.Duration
	// Successes probe successes close the circuit again.
	Successes int
}

// DefaultBreakerConfig: 5 failures within a minute open; probe after 30s;
// one successful probe closes.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{Failures: 5, Window: time.Minute, Cooldown: 30 * time.Second, Successes: 1}
}

// Breaker is a provider circuit breaker. While OPEN no regular request is
// admitted; after the cooldown exactly one probe at a time is admitted
// (HALF_OPEN); probe successes close it, a probe failure re-opens it.
type Breaker struct {
	mu        sync.Mutex
	cfg       BreakerConfig
	state     BreakerState
	failures  int
	firstFail time.Time
	openedAt  time.Time
	probing   bool
	successes int
	lastErr   string
}

// NewBreaker builds a closed breaker.
func NewBreaker(cfg BreakerConfig) *Breaker {
	d := DefaultBreakerConfig()
	if cfg.Failures <= 0 {
		cfg.Failures = d.Failures
	}
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = d.Cooldown
	}
	if cfg.Successes <= 0 {
		cfg.Successes = d.Successes
	}
	return &Breaker{cfg: cfg, state: BreakerClosed}
}

// Admission is the breaker's decision.
type Admission struct {
	Allowed bool
	// Probe marks the single request admitted while HALF_OPEN.
	Probe bool
	State BreakerState
}

// Allow decides whether a request may go to the provider now.
func (b *Breaker) Allow(now time.Time) Admission {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		if now.Sub(b.openedAt) < b.cfg.Cooldown {
			return Admission{State: b.state}
		}
		b.state, b.successes = BreakerHalfOpen, 0
		fallthrough
	case BreakerHalfOpen:
		if b.probing {
			return Admission{State: b.state}
		}
		b.probing = true
		return Admission{Allowed: true, Probe: true, State: b.state}
	}
	return Admission{Allowed: true, State: b.state}
}

// Success records a successful provider interaction.
func (b *Breaker) Success(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerHalfOpen:
		b.probing = false
		b.successes++
		if b.successes >= b.cfg.Successes {
			b.state, b.failures, b.lastErr = BreakerClosed, 0, ""
		}
	default:
		b.failures = 0
	}
}

// Failure records a provider connectivity failure.
func (b *Breaker) Failure(now time.Time, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastErr = reason
	switch b.state {
	case BreakerHalfOpen:
		b.probing = false
		b.state, b.openedAt = BreakerOpen, now
		return
	case BreakerOpen:
		return
	}
	if b.failures == 0 || now.Sub(b.firstFail) > b.cfg.Window {
		b.failures, b.firstFail = 0, now
	}
	b.failures++
	if b.failures >= b.cfg.Failures {
		b.state, b.openedAt = BreakerOpen, now
	}
}

// Release frees an admitted probe that ended without a verdict.
func (b *Breaker) Release() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

// Status reports availability: OPEN → UNAVAILABLE; HALF_OPEN or recent
// failures → DEGRADED; else HEALTHY.
func (b *Breaker) Status() (Availability, BreakerState, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.state == BreakerOpen:
		return Unavailable, b.state, b.lastErr
	case b.state == BreakerHalfOpen || b.failures > 0:
		return Degraded, b.state, b.lastErr
	}
	return Healthy, b.state, ""
}
