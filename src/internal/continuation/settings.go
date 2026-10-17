package continuation

import (
	"errors"
	"fmt"
	"time"
)

// Settings are the per-project continuation knobs (WBS 14.12, 14.13,
// 14.37, 14.38, 14.41, 14.92). The context threshold itself is the
// project setting context_threshold (PLAN Decision 1).
type Settings struct {
	// UnknownContextPolicy is the strategy when telemetry is unavailable.
	UnknownContextPolicy Strategy
	// EstimatedMayReset lets an overseer estimate trigger a reset; when
	// false an estimate is treated as unknown context.
	EstimatedMayReset bool

	// MaxContinuationsPerWorkUnit / MaxSessionResetsPerWorkUnit may be
	// Unlimited (-1): no maximum is enforced.
	MaxContinuationsPerWorkUnit int
	MaxSessionResetsPerWorkUnit int
	MaxNoProgressContinuations  int
	// SessionCreateRetries is how many failed NewSession calls are retried
	// before the continuation needs recovery.
	SessionCreateRetries int
	// LeaseDuration bounds how long one instance owns a continuation.
	LeaseDuration time.Duration
}

// Defaults (WBS recommendations).
const (
	DefaultMaxContinuations = 8
	DefaultMaxSessionResets = 4
	DefaultMaxNoProgress    = 3
	DefaultLeaseDuration    = 60 * time.Second
	maxSessionCreateRetries = 10
	// Unlimited disables the continuation / session-reset maximum.
	Unlimited = -1
)

// DefaultSettings returns the recommended configuration.
func DefaultSettings() Settings {
	return Settings{UnknownContextPolicy: ContinueFreshSession, EstimatedMayReset: true,
		MaxContinuationsPerWorkUnit: DefaultMaxContinuations, MaxSessionResetsPerWorkUnit: DefaultMaxSessionResets,
		MaxNoProgressContinuations: DefaultMaxNoProgress, LeaseDuration: DefaultLeaseDuration}
}

// ErrInvalidSettings wraps every settings problem.
var ErrInvalidSettings = errors.New("continuation: invalid settings")

// Validate reports every problem at once.
func (s Settings) Validate() error {
	var errs []error
	if !s.UnknownContextPolicy.Valid() {
		errs = append(errs, fmt.Errorf("%w: unknown_context_policy must be same_session or fresh_session", ErrInvalidSettings))
	}
	if s.MaxContinuationsPerWorkUnit < 1 && s.MaxContinuationsPerWorkUnit != Unlimited {
		errs = append(errs, fmt.Errorf("%w: max_continuations_per_work_unit must be >= 1 or -1 (no limit)", ErrInvalidSettings))
	}
	if s.MaxSessionResetsPerWorkUnit < 0 && s.MaxSessionResetsPerWorkUnit != Unlimited {
		errs = append(errs, fmt.Errorf("%w: max_session_resets_per_work_unit must be >= 0 or -1 (no limit)", ErrInvalidSettings))
	}
	if s.MaxNoProgressContinuations < 1 {
		errs = append(errs, fmt.Errorf("%w: max_no_progress_continuations must be >= 1", ErrInvalidSettings))
	}
	if s.SessionCreateRetries < 0 || s.SessionCreateRetries > maxSessionCreateRetries {
		errs = append(errs, fmt.Errorf("%w: session_create_retries must be between 0 and %d", ErrInvalidSettings, maxSessionCreateRetries))
	}
	if s.LeaseDuration < time.Second {
		errs = append(errs, fmt.Errorf("%w: lease_duration must be at least 1s", ErrInvalidSettings))
	}
	return errors.Join(errs...)
}

// ValidThreshold reports 0 < t <= 1 (WBS 14.5).
func ValidThreshold(t float64) bool { return t > 0 && t <= 1 }
