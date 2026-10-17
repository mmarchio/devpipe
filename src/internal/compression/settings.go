package compression

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Mode is the forced-compression mode (WBS 9.31); it also carries the WBS
// 9.2 "enabled" flag (NEVER = disabled).
type Mode string

const (
	ModeAuto   Mode = "AUTO"
	ModeAlways Mode = "ALWAYS"
	ModeNever  Mode = "NEVER"
)

// Known reports whether m is a valid mode.
func (m Mode) Known() bool { return m == ModeAuto || m == ModeAlways || m == ModeNever }

// ValidationMode selects the semantic validator (WBS 9.23): DETERMINISTIC
// uses the deterministic structured checks only; HYBRID additionally runs
// the configured LLM semantic validator.
type ValidationMode string

const (
	ValidationDeterministic ValidationMode = "DETERMINISTIC"
	ValidationHybrid        ValidationMode = "HYBRID"
)

// Known reports whether v is a valid validation mode.
func (v ValidationMode) Known() bool { return v == ValidationDeterministic || v == ValidationHybrid }

// Settings is the effective compression configuration of a project (WBS
// 9.2): the project configuration established earlier (provider, model,
// context window, threshold-derived target) extended with the
// compression-specific settings.
type Settings struct {
	Mode Mode

	Provider string
	Model    string

	// ContextWindow is the worker/model context window (project setting).
	ContextWindow int
	// ContextThreshold is the project's context threshold (fallback ratio).
	ContextThreshold float64

	// TargetRatio, when > 0, sets the target as ContextWindow*TargetRatio.
	TargetRatio float64
	// TargetTokens, when > 0, is the absolute target (wins over ratio).
	TargetTokens int

	// MinimumSavings is the minimum fractional reduction (0.15 = 15%).
	MinimumSavings float64

	MaxAttempts int

	ValidationMode ValidationMode

	Timeout     time.Duration
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// Default compression-specific values (applied when a project has no stored
// compression settings).
const (
	DefaultMinimumSavings = 0.15
	DefaultMaxAttempts    = 2
	DefaultTimeout        = 60 * time.Second
	DefaultBackoffBase    = 500 * time.Millisecond
	DefaultBackoffMax     = 5 * time.Second
	// MaxAttemptsLimit bounds the configurable retry count (no unbounded
	// retry loop, T9.28.3).
	MaxAttemptsLimit = 10
)

// DefaultSettings returns the compression-specific defaults (mode AUTO,
// threshold-derived target, 15% minimum savings, 2 attempts,
// deterministic validation).
func DefaultSettings() Settings {
	return Settings{
		Mode:           ModeAuto,
		MinimumSavings: DefaultMinimumSavings,
		MaxAttempts:    DefaultMaxAttempts,
		ValidationMode: ValidationDeterministic,
		Timeout:        DefaultTimeout,
		BackoffBase:    DefaultBackoffBase,
		BackoffMax:     DefaultBackoffMax,
	}
}

// Enabled reports whether compression may run at all (WBS 9.2 "enabled").
func (s Settings) Enabled() bool { return s.Mode != ModeNever }

// SettingsError lists every invalid field (field → message).
type SettingsError struct {
	Fields map[string]string
}

func (e *SettingsError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k, v := range e.Fields {
		keys = append(keys, k+": "+v)
	}
	sort.Strings(keys)
	return "compression: invalid settings: " + strings.Join(keys, "; ")
}

// ValidateCompressionFields validates only the compression-specific fields
// (the stored compression settings; the inherited project fields are
// validated by the project settings model).
func (s Settings) ValidateCompressionFields() error {
	f := map[string]string{}
	if !s.Mode.Known() {
		f["mode"] = "must be AUTO, ALWAYS or NEVER"
	}
	if s.TargetRatio < 0 || s.TargetRatio > 1 {
		f["target_ratio"] = "must be in [0, 1]"
	}
	if s.TargetTokens < 0 {
		f["target_tokens"] = "must not be negative"
	}
	if s.MinimumSavings < 0 || s.MinimumSavings >= 1 {
		f["minimum_savings"] = "must be in [0, 1)"
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > MaxAttemptsLimit {
		f["max_attempts"] = fmt.Sprintf("must be in [1, %d]", MaxAttemptsLimit)
	}
	if !s.ValidationMode.Known() {
		f["validation_mode"] = "must be DETERMINISTIC or HYBRID"
	}
	if s.Timeout <= 0 {
		f["timeout"] = "must be positive"
	}
	if s.BackoffBase < 0 {
		f["backoff_base"] = "must not be negative"
	}
	if s.BackoffMax < s.BackoffBase {
		f["backoff_max"] = "must be >= backoff_base"
	}
	if len(f) > 0 {
		return &SettingsError{Fields: f}
	}
	return nil
}

// Validate checks the whole effective configuration (T9.2.1-T9.2.4).
func (s Settings) Validate() error {
	err := s.ValidateCompressionFields()
	f := map[string]string{}
	if err != nil {
		for k, v := range err.(*SettingsError).Fields {
			f[k] = v
		}
	}
	if s.ContextWindow <= 0 {
		f["context_window"] = "must be positive"
	}
	if s.TargetRatio == 0 && s.TargetTokens == 0 && (s.ContextThreshold <= 0 || s.ContextThreshold > 1) {
		f["target"] = "no target: set target_tokens, target_ratio, or a valid context threshold"
	}
	if s.TargetTokens > 0 && s.ContextWindow > 0 && s.TargetTokens > s.ContextWindow {
		f["target_tokens"] = "must not exceed the context window"
	}
	if len(f) > 0 {
		return &SettingsError{Fields: f}
	}
	return nil
}

// EffectiveTarget returns the compression target in tokens: TargetTokens
// when set, else ContextWindow*TargetRatio, else ContextWindow*
// ContextThreshold (the Phase 8 size-validation target).
func (s Settings) EffectiveTarget() int {
	switch {
	case s.TargetTokens > 0:
		return s.TargetTokens
	case s.TargetRatio > 0:
		return int(float64(s.ContextWindow) * s.TargetRatio)
	default:
		return int(float64(s.ContextWindow) * s.ContextThreshold)
	}
}
