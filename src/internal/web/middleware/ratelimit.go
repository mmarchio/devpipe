package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateConfig configures the expensive-endpoint limiter (WBS 17.154).
type RateConfig struct {
	// PerSecond is the sustained rate per principal (default 20).
	PerSecond float64
	// Burst is the bucket size (default 60).
	Burst float64
	// Now is the clock (tests).
	Now func() time.Time
}

// RateLimiter is a token bucket per (principal, route class). Only
// EXPENSIVE routes (previews, diffs, log ranges, cache explain) are
// limited; the engine and scheduler never pass through it.
type RateLimiter struct {
	cfg RateConfig
	ew  ErrorWriter
	mu  sync.Mutex
	b   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a limiter with defaults applied.
func NewRateLimiter(cfg RateConfig, ew ErrorWriter) *RateLimiter {
	if cfg.PerSecond <= 0 {
		cfg.PerSecond = 20
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 60
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RateLimiter{cfg: cfg, ew: writerOr(ew), b: map[string]*bucket{}}
}

// ExpensiveRoute classifies a request path as EXPENSIVE.
func ExpensiveRoute(path string) bool {
	for _, s := range []string{"/preview", "/diff", "/output", "compression-preview", "/explain", "/context-preview"} {
		if strings.Contains(path, s) {
			return true
		}
	}
	return false
}

// Allow takes one token for key.
func (l *RateLimiter) Allow(key string) bool {
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.b[key]
	if b == nil {
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.b[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.cfg.PerSecond
	if b.tokens > l.cfg.Burst {
		b.tokens = l.cfg.Burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware limits EXPENSIVE routes per principal → 429 + Retry-After.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ExpensiveRoute(r.URL.Path) {
			key := PrincipalFromContext(r.Context()).ID + "|expensive"
			if !l.Allow(key) {
				w.Header().Set("Retry-After", "1")
				l.ew(w, r, errRateLimited)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
