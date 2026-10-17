package state

import (
	"fmt"

	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// TransitionContextInput carries the caller-supplied, declared fields for a
// TransitionContext (WBS 6.25). ValidationPassed is a pointer so that a
// missing validation outcome is a representable, valid state (T6.25.2).
type TransitionContextInput struct {
	Outcome string

	ContextUsage     float64
	ContextThreshold float64

	RemainingWorkUnits int
	RemainingPhases    int

	ValidationPassed *bool

	ProjectStatus  project.ProjectStatus
	PhaseStatus    phase.PhaseStatus
	WorkUnitStatus wbs.WorkUnitStatus
}

// TransitionContext is the declared-fields-only data structure supplied to
// deterministic transition evaluation (WBS 6.25). The condition engine may
// read ONLY the fields surfaced by ConditionValues — no catch-all map, no
// variadic accessor (T6.25.3).
type TransitionContext struct {
	Outcome string

	ContextUsage     float64
	ContextThreshold float64

	RemainingWorkUnits int
	RemainingPhases    int

	ValidationPassed *bool

	ProjectStatus  project.ProjectStatus
	PhaseStatus    phase.PhaseStatus
	WorkUnitStatus wbs.WorkUnitStatus
}

// NewTransitionContext constructs a TransitionContext from declared inputs
// (T6.25.1). The value is copied verbatim; the domain assigns nothing — the
// context is pure data.
func NewTransitionContext(in TransitionContextInput) (*TransitionContext, error) {
	if in.Outcome == "" {
		return nil, fmt.Errorf("state: transition context outcome must not be empty")
	}
	return &TransitionContext{
		Outcome:            in.Outcome,
		ContextUsage:       in.ContextUsage,
		ContextThreshold:   in.ContextThreshold,
		RemainingWorkUnits: in.RemainingWorkUnits,
		RemainingPhases:    in.RemainingPhases,
		ValidationPassed:   in.ValidationPassed,
		ProjectStatus:      in.ProjectStatus,
		PhaseStatus:        in.PhaseStatus,
		WorkUnitStatus:     in.WorkUnitStatus,
	}, nil
}

// ConditionValues returns the declared field set consumed by the condition
// engine (WBS 6.10). The key set is exactly DeclaredConditionFields — no
// undeclared value is reachable (T6.25.3). All-work-units-done is derived
// from RemainingWorkUnits so the two stay consistent. A missing
// ValidationPassed (nil) is OMITTED from the map so a condition on it
// deterministically evaluates to false (T6.10.7), never a panic.
func (c *TransitionContext) ConditionValues() map[string]any {
	out := map[string]any{
		"context_usage":             c.ContextUsage,
		"project.context_threshold": c.ContextThreshold,
		"remaining_work_units":      c.RemainingWorkUnits,
		"remaining_phases":          c.RemainingPhases,
		"all_work_units_done":       c.RemainingWorkUnits == 0,
	}
	if c.ValidationPassed != nil {
		out["validation_passed"] = *c.ValidationPassed
	}
	return out
}

// Serialize returns a deterministic canonical text encoding of the context
// (T6.25.4): byte-for-byte stable across calls for an unchanged context, with
// a fixed field order independent of map iteration. String/bool values are
// quoted with %q; a nil optional is rendered as the explicit absent marker "-".
func (c *TransitionContext) Serialize() string {
	v := "-"
	if c.ValidationPassed != nil {
		if *c.ValidationPassed {
			v = `"true"`
		} else {
			v = `"false"`
		}
	}
	return fmt.Sprintf(
		"transition_context:outcome=%q;usage=%v;threshold=%v;remaining_units=%d;remaining_phases=%d;validation=%s;project=%s;phase=%s;work_unit=%s",
		c.Outcome, c.ContextUsage, c.ContextThreshold,
		c.RemainingWorkUnits, c.RemainingPhases,
		v, c.ProjectStatus.String(), c.PhaseStatus.String(), c.WorkUnitStatus.String(),
	)
}
