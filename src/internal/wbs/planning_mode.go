package wbs

import (
	"fmt"
	"strings"
)

// PlanningMode is the planning policy choice of the console (WBS 17.61).
type PlanningMode string

const (
	PlanningFeature   PlanningMode = "FEATURE"
	PlanningComponent PlanningMode = "COMPONENT"
	PlanningAtomic    PlanningMode = "ATOMIC"
	PlanningCustom    PlanningMode = "CUSTOM"
)

// PlanningModes lists the modes in display order.
func PlanningModes() []PlanningMode {
	return []PlanningMode{PlanningFeature, PlanningComponent, PlanningAtomic, PlanningCustom}
}

// GranularitySmallFunctions is the project planning-granularity value of
// the "Prefer small functions/methods" option.
const GranularitySmallFunctions = "PREFER_SMALL_FUNCTIONS"

// customPrefix marks a custom policy text in project planning_policy.
const customPrefix = "CUSTOM: "

// MaxCustomPolicy bounds custom policy text.
const MaxCustomPolicy = 2000

// PlanningConfiguration is the console's planning policy form (WBS 17.61):
// the mode, the Atomic options and custom policy text. It persists into
// project planning_policy / planning_granularity (Phase 8 prompt inputs)
// and the engine AtomicPolicy (Phase 16 plan validation).
type PlanningConfiguration struct {
	Mode                 PlanningMode `json:"mode"`
	Atomic               AtomicPolicy `json:"atomic"`
	PreferSmallFunctions bool         `json:"prefer_small_functions"`
	CustomPolicy         string       `json:"custom_policy,omitempty"`
}

// Validate reports every problem (field → message).
func (c PlanningConfiguration) Validate() map[string]string {
	probs := map[string]string{}
	valid := false
	for _, m := range PlanningModes() {
		if c.Mode == m {
			valid = true
		}
	}
	if !valid {
		probs["mode"] = "must be one of FEATURE, COMPONENT, ATOMIC, CUSTOM"
	}
	switch {
	case c.Mode == PlanningCustom && strings.TrimSpace(c.CustomPolicy) == "":
		probs["custom_policy"] = "required for CUSTOM"
	case c.Mode != PlanningCustom && c.CustomPolicy != "":
		probs["custom_policy"] = "only allowed for CUSTOM"
	case len(c.CustomPolicy) > MaxCustomPolicy:
		probs["custom_policy"] = fmt.Sprintf("at most %d characters", MaxCustomPolicy)
	}
	if c.Mode != PlanningAtomic && c.Mode != PlanningCustom && c.Atomic != (AtomicPolicy{}) {
		probs["atomic"] = "atomic options apply only to ATOMIC or CUSTOM"
	}
	return probs
}

// Encode returns the persisted forms: project planning_policy,
// planning_granularity and the engine atomic policy.
func (c PlanningConfiguration) Encode() (policy, granularity string, atomic AtomicPolicy) {
	policy = string(c.Mode)
	if c.Mode == PlanningCustom {
		policy = customPrefix + c.CustomPolicy
	}
	if c.PreferSmallFunctions {
		granularity = GranularitySmallFunctions
	}
	return policy, granularity, c.Atomic
}

// DecodePlanningConfiguration reverses Encode. An empty policy (projects
// configured before Phase 17) decodes as ATOMIC when every atomic option
// is set, else CUSTOM-less FEATURE.
func DecodePlanningConfiguration(policy, granularity string, atomic AtomicPolicy) PlanningConfiguration {
	c := PlanningConfiguration{Atomic: atomic, PreferSmallFunctions: granularity == GranularitySmallFunctions}
	switch {
	case strings.HasPrefix(policy, customPrefix):
		c.Mode, c.CustomPolicy = PlanningCustom, strings.TrimPrefix(policy, customPrefix)
	case policy == string(PlanningFeature), policy == string(PlanningComponent), policy == string(PlanningAtomic):
		c.Mode = PlanningMode(policy)
	case policy == "" && atomic == DefaultAtomicPolicy():
		c.Mode = PlanningAtomic
	default:
		c.Mode = PlanningCustom
		c.CustomPolicy = policy
	}
	return c
}
