package execution

import (
	"errors"
	"fmt"
	"time"
)

// DirtyPolicy is the dirty-working-tree policy (WBS 15.20).
type DirtyPolicy string

// Dirty policies.
const (
	DirtyAllow  DirtyPolicy = "ALLOW"
	DirtyWarn   DirtyPolicy = "WARN"
	DirtyReject DirtyPolicy = "REJECT"
)

// Valid reports a known policy.
func (p DirtyPolicy) Valid() bool { return p == DirtyAllow || p == DirtyWarn || p == DirtyReject }

// ParseDirtyPolicy accepts exactly one canonical policy.
func ParseDirtyPolicy(s string) (DirtyPolicy, error) {
	p := DirtyPolicy(s)
	if !p.Valid() {
		return "", fmt.Errorf("execution: invalid dirty policy %q", s)
	}
	return p, nil
}

// Bounds for the control-transition limit (WBS 15.60).
const (
	MinControlTransitions = 1
	MaxControlTransitions = 1000
)

// Settings is the project's execution/bootstrap configuration (PLAN
// Decision 14).
type Settings struct {
	RequireGitRepository           bool          `json:"require_git_repository"`
	DirtyPolicy                    DirtyPolicy   `json:"dirty_policy"`
	MaterializeStateFiles          bool          `json:"materialize_state_files"`
	MaxBootstrapControlTransitions int           `json:"max_bootstrap_control_transitions"`
	BootstrapLease                 time.Duration `json:"bootstrap_lease"`
}

// DefaultSettings returns the WBS defaults.
func DefaultSettings() Settings {
	return Settings{
		DirtyPolicy:                    DirtyWarn,
		MaxBootstrapControlTransitions: 100,
		BootstrapLease:                 60 * time.Second,
	}
}

// Validate reports every problem.
func (s Settings) Validate() error {
	var errs []error
	if !s.DirtyPolicy.Valid() {
		errs = append(errs, fmt.Errorf("dirty_policy must be ALLOW, WARN or REJECT"))
	}
	if s.MaxBootstrapControlTransitions < MinControlTransitions || s.MaxBootstrapControlTransitions > MaxControlTransitions {
		errs = append(errs, fmt.Errorf("max_bootstrap_control_transitions must be in [%d, %d]", MinControlTransitions, MaxControlTransitions))
	}
	if s.BootstrapLease < time.Second || s.BootstrapLease > time.Hour {
		errs = append(errs, fmt.Errorf("bootstrap_lease must be in [1s, 1h]"))
	}
	return errors.Join(errs...)
}
