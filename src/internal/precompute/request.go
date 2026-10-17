// Package precompute is the Phase 11 domain: future-state prompt
// precomputation. It discovers the deterministically reachable future
// contexts of a project, classifies what can be prepared ahead of time, and
// describes projections, jobs, budgets and metrics. It never composes,
// compresses, validates or keys prompts itself (WBS 11.1) and never
// performs workflow actions (WBS 11.35).
package precompute

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Trigger names what started a precompute job (WBS 11.2).
type Trigger string

const (
	TriggerProjectStart    Trigger = "PROJECT_START"
	TriggerPhaseStart      Trigger = "PHASE_START"
	TriggerWorkUnitStart   Trigger = "WORK_UNIT_START"
	TriggerWorkerDispatch  Trigger = "WORKER_DISPATCH"
	TriggerStateTransition Trigger = "STATE_TRANSITION"
	TriggerManual          Trigger = "MANUAL"
	TriggerPlanAccepted    Trigger = "PLAN_ACCEPTED"
)

// Known reports whether t is a valid trigger.
func (t Trigger) Known() bool {
	switch t {
	case TriggerProjectStart, TriggerPhaseStart, TriggerWorkUnitStart, TriggerWorkerDispatch,
		TriggerStateTransition, TriggerManual, TriggerPlanAccepted:
		return true
	}
	return false
}

// Depth / candidate bounds.
const (
	MaxDepthLimit      = 5
	MaxCandidatesLimit = 64
	MaxConcurrency     = 16
)

// Request is a precompute request (WBS 11.2). Zero MaxDepth /
// MaxCandidates mean "use the project settings".
type Request struct {
	ProjectID      uuid.UUID
	RuntimeStateID uuid.UUID
	Trigger        Trigger

	MaxDepth      int
	MaxCandidates int

	// BaseSnapshotID optionally pins the base snapshot (else the latest
	// snapshot of the current runtime position).
	BaseSnapshotID *uuid.UUID
}

// ErrInvalidRequest wraps request validation failures.
var ErrInvalidRequest = errors.New("precompute: invalid request")

// Validate checks the request (T11.2.1-T11.2.5).
func (r Request) Validate() error {
	var bad []string
	if r.ProjectID == uuid.Nil {
		bad = append(bad, "project_id required")
	}
	if r.RuntimeStateID == uuid.Nil {
		bad = append(bad, "runtime_state_id required")
	}
	if !r.Trigger.Known() {
		bad = append(bad, "unknown trigger")
	}
	if r.MaxDepth < 0 || r.MaxDepth > MaxDepthLimit {
		bad = append(bad, fmt.Sprintf("max_depth must be in [1, %d]", MaxDepthLimit))
	}
	if r.MaxCandidates < 0 || r.MaxCandidates > MaxCandidatesLimit {
		bad = append(bad, fmt.Sprintf("max_candidates must be in [1, %d]", MaxCandidatesLimit))
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(bad, "; "))
	}
	return nil
}

// ResourcePolicy controls precomputation under local resource contention
// (WBS 11.40).
type ResourcePolicy string

const (
	PolicyAlways                ResourcePolicy = "ALWAYS"
	PolicyIdleOnly              ResourcePolicy = "IDLE_ONLY"
	PolicyRemoteOnlyWhileActive ResourcePolicy = "REMOTE_ONLY_WHILE_WORKER_ACTIVE"
	PolicyDisabledDuringLocal   ResourcePolicy = "DISABLED_DURING_LOCAL_WORKER"
)

// Known reports whether p is a valid policy.
func (p ResourcePolicy) Known() bool {
	switch p {
	case PolicyAlways, PolicyIdleOnly, PolicyRemoteOnlyWhileActive, PolicyDisabledDuringLocal:
		return true
	}
	return false
}

// Settings is the per-project precompute configuration (WBS 11.3, 11.34,
// 11.39, 11.40).
type Settings struct {
	Enabled               bool
	Depth                 int
	MaxCandidates         int
	Concurrency           int
	CompressionEnabled    bool
	StaticFragments       bool
	DuringWorkerExecution bool
	// SameSessionCompression enables compression for same-session
	// continuation material (default false, WBS 11.27).
	SameSessionCompression bool
	// PlanNextPhase allows precomputing the next phase's planning prompt
	// (never executing the planning LLM, WBS 11.34).
	PlanNextPhase bool

	// Budgets (WBS 11.39); 0 = unlimited.
	MaxTokens   int
	MaxDuration time.Duration

	ResourcePolicy ResourcePolicy
	// WorkerIsLocal declares the development worker shares local
	// resources (DISABLED_DURING_LOCAL_WORKER).
	WorkerIsLocal bool
}

// DefaultSettings returns the WBS 11.3 defaults.
func DefaultSettings() Settings {
	return Settings{
		Enabled: true, Depth: 2, MaxCandidates: 8, Concurrency: 2,
		CompressionEnabled: true, StaticFragments: true, DuringWorkerExecution: true,
		SameSessionCompression: false, PlanNextPhase: true,
		MaxDuration: 5 * time.Minute, ResourcePolicy: PolicyAlways,
	}
}

// SettingsError lists invalid fields.
type SettingsError struct{ Fields map[string]string }

func (e *SettingsError) Error() string {
	var parts []string
	for k, v := range e.Fields {
		parts = append(parts, k+": "+v)
	}
	return "precompute: invalid settings: " + strings.Join(parts, "; ")
}

// Validate checks the settings (T11.3.3/T11.3.4).
func (s Settings) Validate() error {
	f := map[string]string{}
	if s.Depth < 1 || s.Depth > MaxDepthLimit {
		f["depth"] = fmt.Sprintf("must be in [1, %d]", MaxDepthLimit)
	}
	if s.MaxCandidates < 1 || s.MaxCandidates > MaxCandidatesLimit {
		f["max_candidates"] = fmt.Sprintf("must be in [1, %d]", MaxCandidatesLimit)
	}
	if s.Concurrency < 1 || s.Concurrency > MaxConcurrency {
		f["concurrency"] = fmt.Sprintf("must be in [1, %d]", MaxConcurrency)
	}
	if s.MaxTokens < 0 {
		f["max_tokens"] = "must not be negative"
	}
	if s.MaxDuration < 0 {
		f["max_duration"] = "must not be negative"
	}
	if !s.ResourcePolicy.Known() {
		f["resource_policy"] = "must be ALWAYS, IDLE_ONLY, REMOTE_ONLY_WHILE_WORKER_ACTIVE or DISABLED_DURING_LOCAL_WORKER"
	}
	if len(f) > 0 {
		return &SettingsError{Fields: f}
	}
	return nil
}
