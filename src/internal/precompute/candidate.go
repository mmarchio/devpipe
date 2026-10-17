package precompute

import (
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Readiness classifies how much of a future prompt is known (WBS 11.7/11.9).
type Readiness string

const (
	FullyKnown     Readiness = "FULLY_KNOWN"
	PartiallyKnown Readiness = "PARTIALLY_KNOWN"
	Unknown        Readiness = "UNKNOWN"
)

// Known reports whether r is a valid readiness.
func (r Readiness) Known() bool { return r == FullyKnown || r == PartiallyKnown || r == Unknown }

// DynamicInputType is an input that only exists after future work (WBS
// 11.10/11.13).
type DynamicInputType string

const (
	InputWorkerStatus         DynamicInputType = "WORKER_STATUS"
	InputImplementationResult DynamicInputType = "IMPLEMENTATION_RESULT"
	InputTestResults          DynamicInputType = "TEST_RESULTS"
	InputValidationFailures   DynamicInputType = "VALIDATION_FAILURES"
	InputGeneratedPlan        DynamicInputType = "GENERATED_PLAN"
	InputRuntimeFailure       DynamicInputType = "RUNTIME_FAILURE"
	// InputActiveTask: the projected unit has no ACTIVE_TASK yet — resolved
	// by the placeholder mechanism (PLAN Decision 5), not by a fragment.
	InputActiveTask DynamicInputType = "ACTIVE_TASK"
	// InputCustom: a custom template whose dynamic needs are unknown.
	InputCustom DynamicInputType = "CUSTOM_INPUT"
)

var knownInputs = map[DynamicInputType]bool{
	InputWorkerStatus: true, InputImplementationResult: true, InputTestResults: true,
	InputValidationFailures: true, InputGeneratedPlan: true, InputRuntimeFailure: true,
	InputActiveTask: true, InputCustom: true,
}

// Known reports whether t is a valid dynamic input type.
func (t DynamicInputType) Known() bool { return knownInputs[t] }

// Registry maps a target state's prompt template (key, else purpose) to the
// dynamic inputs its final prompt needs (WBS 11.10). It is data: projects
// may override entries.
type Registry struct {
	ByTemplateKey map[string][]DynamicInputType
	ByPurpose     map[string][]DynamicInputType
	// UnitBound lists purposes whose prompts belong to one work unit.
	UnitBound map[string]bool
}

// DefaultRegistry is the WBS 11.10 default classification over the Phase 8
// default templates and purposes.
func DefaultRegistry() Registry {
	return Registry{
		ByTemplateKey: map[string][]DynamicInputType{
			"PLAN_PHASE":          nil,
			"PREPARE_WORK_UNIT":   nil,
			"IMPLEMENT_WORK_UNIT": nil,
			"CONTINUE_WORK_UNIT":  {InputWorkerStatus},
			"VALIDATE_WORK_UNIT":  {InputImplementationResult, InputTestResults},
			"FIX_WORK_UNIT":       {InputValidationFailures},
			"VALIDATE_PLAN":       {InputGeneratedPlan},
			"VALIDATE_PHASE":      {InputTestResults},
			"REVIEW_RESULT":       {InputImplementationResult},
		},
		ByPurpose: map[string][]DynamicInputType{
			"PHASE_PLANNING": nil,
			"IMPLEMENTATION": nil,
			"CONTINUATION":   {InputWorkerStatus},
			"VALIDATION":     {InputImplementationResult, InputTestResults},
			"REVIEW":         {InputImplementationResult},
			"CUSTOM":         {InputCustom},
		},
		UnitBound: map[string]bool{"IMPLEMENTATION": true, "CONTINUATION": true, "VALIDATION": true, "REVIEW": true},
	}
}

// DynamicInputs returns the dynamic inputs a template needs; ok=false when
// neither its key nor its purpose is registered (treated as needing an
// unknown input — never assumed static).
func (r Registry) DynamicInputs(templateKey, purpose string) ([]DynamicInputType, bool) {
	if in, ok := r.ByTemplateKey[templateKey]; ok {
		return append([]DynamicInputType(nil), in...), true
	}
	if in, ok := r.ByPurpose[purpose]; ok {
		return append([]DynamicInputType(nil), in...), true
	}
	return []DynamicInputType{InputCustom}, false
}

// IsStatic reports whether a named dependency is known ahead of time (WBS
// 11.10): role / system prompt, role & state instructions, template,
// response protocol, architecture / project state, planning policy, and the
// work-unit specification (acceptance criteria, tests) once the plan is
// persisted. Worker output, test output, generated plans, the next
// ACTIVE_TASK selection, failures and anything unknown are dynamic.
func IsStatic(dep string) bool {
	switch dep {
	case "system_prompt", "role", "role_instructions", "state_instructions", "template", "response_protocol",
		"file:ARCHITECTURE", "file:PROJECT_STATE", "file:PHASE_STATE", "planning_policy",
		"work_unit_spec", "acceptance_criteria", "required_tests":
		return true
	}
	return false
}

// Priority classes (WBS 11.15) — scheduling order, not prediction.
const (
	PriorityImmediate    = 1
	PrioritySameUnit     = 2
	PriorityNextUnit     = 3
	PriorityPhaseBoundry = 4
	PriorityDistant      = 5
)

// Candidate is one potential future prompt (WBS 11.7).
type Candidate struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	StateKey string
	RoleKey  string

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	TemplateKey     string
	TemplatePurpose string

	// Paths retains every transition path that led here (dedup keeps all,
	// T11.44.3); Paths[0] is the representative path.
	Paths [][]string
	Depth int
	// NextUnit marks a projection onto the next executable work unit.
	NextUnit bool

	Readiness Readiness
	Priority  int

	Known   []string
	Missing []DynamicInputType
	// Structural lists missing structural inputs (UNKNOWN reasons).
	Structural []string
	// NeedsPlaceholder: the only missing input is the projected unit's
	// ACTIVE_TASK (Decision 5).
	NeedsPlaceholder bool

	// IdentityHash is the projected semantic identity used for dedup.
	IdentityHash string
}

// ErrInvalidCandidate rejects an invalid candidate.
var ErrInvalidCandidate = errors.New("precompute: invalid candidate")

// NewCandidate validates and builds a candidate (T11.7.1-T11.7.4).
func NewCandidate(c Candidate) (Candidate, error) {
	if c.ProjectID == uuid.Nil || strings.TrimSpace(c.StateKey) == "" || !c.Readiness.Known() {
		return Candidate{}, ErrInvalidCandidate
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	out := c
	out.Paths = nil
	for _, p := range c.Paths {
		out.Paths = append(out.Paths, append([]string(nil), p...))
	}
	return out, nil
}

// Analysis is the dependency analysis input of one candidate.
type Analysis struct {
	HasRole          bool
	HasTemplate      bool
	TemplateKey      string
	Purpose          string
	UnitBound        bool
	HasUnit          bool
	UnitEligible     bool
	ActiveTaskExists bool
	StaticFilesOK    bool
	// OptionalMissing lists optional dependencies absent (never demote).
	OptionalMissing []string
}

// Classify analyses the dependencies of a candidate and returns its
// readiness, the known static dependencies, the missing dynamic inputs and
// the structural gaps (WBS 11.8/11.9/11.25). Deterministic.
func Classify(a Analysis, reg Registry) (Readiness, []string, []DynamicInputType, []string, bool) {
	var structural []string
	if !a.HasRole {
		structural = append(structural, "no role assigned to the target state")
	}
	if !a.HasTemplate {
		structural = append(structural, "no prompt template resolvable for the target state")
	}
	if a.UnitBound && !a.HasUnit {
		structural = append(structural, "no work unit for a unit-bound prompt")
	}
	if a.UnitBound && a.HasUnit && !a.UnitEligible {
		structural = append(structural, "projected work unit is dependency-blocked")
	}
	if !a.StaticFilesOK {
		structural = append(structural, "required static state files not resolvable")
	}
	known := []string{"role", "system_prompt", "role_instructions", "state_instructions", "template", "response_protocol"}
	if a.HasUnit {
		known = append(known, "work_unit_spec", "acceptance_criteria", "required_tests")
	}
	if len(structural) > 0 {
		return Unknown, nil, nil, structural, false
	}
	missing, _ := reg.DynamicInputs(a.TemplateKey, a.Purpose)
	placeholder := false
	if a.UnitBound && a.HasUnit && !a.ActiveTaskExists {
		missing = append(missing, InputActiveTask)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	switch {
	case len(missing) == 0:
		return FullyKnown, known, nil, nil, false
	case len(missing) == 1 && missing[0] == InputActiveTask:
		// Everything but the projected ACTIVE_TASK is known: prepared with a
		// placeholder (Decision 5) — still not a final prompt.
		placeholder = true
		return PartiallyKnown, known, missing, nil, placeholder
	default:
		return PartiallyKnown, known, missing, nil, false
	}
}

// Rank orders candidates by priority, then depth, then state key, then
// work unit (stable, deterministic — T11.15.3), and keeps the first limit
// (T11.16.1/T11.16.2).
func Rank(cands []Candidate, limit int) []Candidate {
	out := append([]Candidate(nil), cands...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.StateKey != b.StateKey {
			return a.StateKey < b.StateKey
		}
		return unitStr(a.WorkUnitID) < unitStr(b.WorkUnitID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func unitStr(u *uuid.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// Dedup merges candidates with the same projected semantic identity and
// the same dynamic requirements (WBS 11.44): the highest-priority one is
// kept and every transition path is retained for audit; candidates with
// different dynamic requirements are never merged.
func Dedup(cands []Candidate) []Candidate {
	ranked := Rank(cands, 0)
	idx := map[string]int{}
	var out []Candidate
	for _, c := range ranked {
		k := c.IdentityHash + "|" + missingKey(c.Missing)
		if c.IdentityHash == "" {
			out = append(out, c)
			continue
		}
		if i, ok := idx[k]; ok {
			out[i].Paths = append(out[i].Paths, c.Paths...)
			continue
		}
		idx[k] = len(out)
		out = append(out, c)
	}
	return out
}

func missingKey(m []DynamicInputType) string {
	s := make([]string, len(m))
	for i, x := range m {
		s[i] = string(x)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

// SelectNextUnit returns the next deterministically executable unit from
// the dependency-eligible units (already filtered by status/dependencies)
// by lowest sequence, ties by id (WBS 11.17/11.18); nil when none.
func SelectNextUnit(eligible []EligibleUnit) *EligibleUnit {
	if len(eligible) == 0 {
		return nil
	}
	c := append([]EligibleUnit(nil), eligible...)
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Sequence != c[j].Sequence {
			return c[i].Sequence < c[j].Sequence
		}
		return c[i].ID.String() < c[j].ID.String()
	})
	return &c[0]
}

// EligibleUnit is a dependency-ready unit (not DONE/EXECUTING).
type EligibleUnit struct {
	ID       uuid.UUID
	Sequence int
	Name     string
}
