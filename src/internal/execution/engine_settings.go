package execution

import (
	"errors"
	"fmt"
	"time"

	"overseer/internal/wbs"
)

// CheckpointPolicy decides how repository checkpoint failures affect
// progression (WBS 16.55–16.57).
type CheckpointPolicy string

// Checkpoint policies.
const (
	CheckpointRequired   CheckpointPolicy = "REQUIRED"
	CheckpointBestEffort CheckpointPolicy = "BEST_EFFORT"
	CheckpointDisabled   CheckpointPolicy = "DISABLED"
)

// Valid reports a known policy.
func (p CheckpointPolicy) Valid() bool {
	return p == CheckpointRequired || p == CheckpointBestEffort || p == CheckpointDisabled
}

// UsesGit reports whether checkpoints commit to git (every policy but
// DISABLED).
func (p CheckpointPolicy) UsesGit() bool {
	return p == CheckpointRequired || p == CheckpointBestEffort
}

// CheckpointPolicyFor maps the project's "use git" option to its policy:
// REQUIRED with git, DISABLED without.
func CheckpointPolicyFor(useGit bool) CheckpointPolicy {
	if useGit {
		return CheckpointRequired
	}
	return CheckpointDisabled
}

// ParseCheckpointPolicy accepts exactly one canonical policy.
func ParseCheckpointPolicy(s string) (CheckpointPolicy, error) {
	p := CheckpointPolicy(s)
	if !p.Valid() {
		return "", fmt.Errorf("execution: invalid checkpoint policy %q", s)
	}
	return p, nil
}

// Engine-setting bounds.
const (
	MaxAttemptLimit    = 100
	MaxStepBudget      = 1000
	MinOutputCapBytes  = 1024
	MaxOutputCapBytes  = 16 << 20
	MaxValidationLimit = 24 * time.Hour
)

// EngineSettings is the project's autonomous-execution configuration
// (P16-02). It is captured in the execution configuration snapshot, so an
// execution keeps the settings it started with.
type EngineSettings struct {
	MaxFixAttempts    int                  `json:"max_fix_attempts"`
	MaxPlanAttempts   int                  `json:"max_plan_attempts"`
	CheckpointPolicy  CheckpointPolicy     `json:"checkpoint_policy"`
	UnitValidation    []wbs.ValidationSpec `json:"unit_validation"`
	PhaseValidation   []wbs.ValidationSpec `json:"phase_validation"`
	FinalValidation   []wbs.ValidationSpec `json:"final_validation"`
	ValidationTimeout time.Duration        `json:"validation_timeout"`
	OutputCapBytes    int                  `json:"output_cap_bytes"`
	StepBudget        int                  `json:"step_budget"`
	DriverLease       time.Duration        `json:"driver_lease"`
	AtomicPolicy      wbs.AtomicPolicy     `json:"atomic_policy"`
}

// DefaultEngineSettings returns the WBS defaults.
func DefaultEngineSettings() EngineSettings {
	return EngineSettings{
		MaxFixAttempts:    3,
		MaxPlanAttempts:   3,
		CheckpointPolicy:  CheckpointDisabled, // git is opt-in per project
		UnitValidation:    []wbs.ValidationSpec{},
		PhaseValidation:   []wbs.ValidationSpec{{Name: "go test", Program: "go", Args: []string{"test", "./..."}}},
		FinalValidation:   []wbs.ValidationSpec{{Name: "go vet", Program: "go", Args: []string{"vet", "./..."}}, {Name: "go test", Program: "go", Args: []string{"test", "./..."}}},
		ValidationTimeout: 10 * time.Minute,
		OutputCapBytes:    64 << 10,
		StepBudget:        8,
		DriverLease:       60 * time.Second,
		AtomicPolicy:      wbs.DefaultAtomicPolicy(),
	}
}

// Normalize replaces nil command lists with empty ones (canonical JSON).
func (s *EngineSettings) Normalize() {
	if s.UnitValidation == nil {
		s.UnitValidation = []wbs.ValidationSpec{}
	}
	if s.PhaseValidation == nil {
		s.PhaseValidation = []wbs.ValidationSpec{}
	}
	if s.FinalValidation == nil {
		s.FinalValidation = []wbs.ValidationSpec{}
	}
}

// Validate reports every problem.
func (s EngineSettings) Validate() error {
	var errs []error
	if s.MaxFixAttempts < 1 || s.MaxFixAttempts > MaxAttemptLimit {
		errs = append(errs, fmt.Errorf("max_fix_attempts must be in [1, %d]", MaxAttemptLimit))
	}
	if s.MaxPlanAttempts < 1 || s.MaxPlanAttempts > MaxAttemptLimit {
		errs = append(errs, fmt.Errorf("max_plan_attempts must be in [1, %d]", MaxAttemptLimit))
	}
	if !s.CheckpointPolicy.Valid() {
		errs = append(errs, fmt.Errorf("checkpoint_policy must be REQUIRED, BEST_EFFORT or DISABLED"))
	}
	for name, list := range map[string][]wbs.ValidationSpec{"unit_validation": s.UnitValidation, "phase_validation": s.PhaseValidation, "final_validation": s.FinalValidation} {
		for i, v := range list {
			if msg := wbs.ValidationSpecProblem(v); msg != "" {
				errs = append(errs, fmt.Errorf("%s[%d]: %s", name, i, msg))
			}
		}
	}
	if s.ValidationTimeout < time.Second || s.ValidationTimeout > MaxValidationLimit {
		errs = append(errs, fmt.Errorf("validation_timeout must be in [1s, 24h]"))
	}
	if s.OutputCapBytes < MinOutputCapBytes || s.OutputCapBytes > MaxOutputCapBytes {
		errs = append(errs, fmt.Errorf("output_cap_bytes must be in [%d, %d]", MinOutputCapBytes, MaxOutputCapBytes))
	}
	if s.StepBudget < 1 || s.StepBudget > MaxStepBudget {
		errs = append(errs, fmt.Errorf("step_budget must be in [1, %d]", MaxStepBudget))
	}
	if s.DriverLease < time.Second || s.DriverLease > time.Hour {
		errs = append(errs, fmt.Errorf("driver_lease must be in [1s, 1h]"))
	}
	sortErrs(errs)
	return errors.Join(errs...)
}

func sortErrs(errs []error) {
	for i := 1; i < len(errs); i++ {
		for j := i; j > 0 && errs[j].Error() < errs[j-1].Error(); j-- {
			errs[j], errs[j-1] = errs[j-1], errs[j]
		}
	}
}
