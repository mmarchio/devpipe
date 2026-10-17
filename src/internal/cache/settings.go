package cache

import (
	"fmt"
	"time"
)

// Settings is the per-project prompt-cache policy (WBS 10.34/10.35/10.57/
// 10.69).
type Settings struct {
	Enabled bool

	Retention Retention

	// NegativeWindow suppresses repeated provider attempts after a
	// transient failure (0 disables the negative cache).
	NegativeWindow time.Duration

	// Lease is the BUILDING lease duration.
	Lease time.Duration

	// CacheSameSessionContinuation enables caching of same-session
	// continuation prompts (default false — little reuse value, WBS 10.57).
	CacheSameSessionContinuation bool

	// BypassOnCacheError lets preparation continue without the cache when
	// the cache layer fails recoverably (WBS 10.69).
	BypassOnCacheError bool
}

// DefaultSettings returns the documented defaults.
func DefaultSettings() Settings {
	return Settings{
		Enabled: true, Retention: Retention{Policy: RetainAll},
		NegativeWindow: 30 * time.Second, Lease: 2 * time.Minute,
		CacheSameSessionContinuation: false, BypassOnCacheError: true,
	}
}

// Validate checks the policy.
func (s Settings) Validate() error {
	if err := s.Retention.Validate(); err != nil {
		return fmt.Errorf("retention: %w", err)
	}
	if s.NegativeWindow < 0 {
		return fmt.Errorf("cache: negative window must not be negative")
	}
	if s.Lease <= 0 {
		return fmt.Errorf("cache: lease must be positive")
	}
	return nil
}
