package wbs

import (
	"fmt"
	"strings"
)

// TaskScope is the release a task belongs to: MVP (minimum viable
// product) or FULL (full release). The planner categorizes every task;
// a project selects which tasks the orchestrator works on.
type TaskScope string

const (
	TaskScopeMVP  TaskScope = "MVP"
	TaskScopeFULL TaskScope = "FULL"
)

func (s TaskScope) String() string { return string(s) }

// ParseTaskScope converts a token into a TaskScope, rejecting unknown
// values.
func ParseTaskScope(token string) (TaskScope, error) {
	s := TaskScope(strings.ToUpper(strings.TrimSpace(token)))
	if s != TaskScopeMVP && s != TaskScopeFULL {
		return "", fmt.Errorf("wbs: unknown task scope %q (MVP or FULL)", token)
	}
	return s, nil
}

// UnitScope is a unit's task scope; units planned before tasks were
// categorized count as MVP, so selecting MVP never skips them.
func UnitScope(u *WorkUnit) TaskScope {
	if u != nil && u.Specification != nil && u.Specification.Scope == TaskScopeFULL {
		return TaskScopeFULL
	}
	return TaskScopeMVP
}

// Deferred reports whether the orchestrator leaves a unit alone under the
// project's selected scope: with MVP selected, a FULL task that has not
// started. A FULL task already under way or DONE is not deferred (work in
// progress is finished, never abandoned). selected FULL = MVP and FULL.
func Deferred(u *WorkUnit, selected TaskScope) bool {
	if selected != TaskScopeMVP || UnitScope(u) != TaskScopeFULL {
		return false
	}
	switch u.Status {
	case WorkUnitStatusEXECUTING, WorkUnitStatusINCOMPLETE, WorkUnitStatusVALIDATING, WorkUnitStatusDONE:
		return false
	}
	return true
}

// SelectedUnits drops the units deferred under the selected scope, keeping
// order.
func SelectedUnits(units []*WorkUnit, selected TaskScope) []*WorkUnit {
	if selected != TaskScopeMVP {
		return units
	}
	out := make([]*WorkUnit, 0, len(units))
	for _, u := range units {
		if u != nil && !Deferred(u, selected) {
			out = append(out, u)
		}
	}
	return out
}

// Task-scope problem codes.
const (
	ProblemUnknownScope   = "UNKNOWN_SCOPE"
	ProblemMVPDependsFull = "MVP_DEPENDS_ON_FULL"
)

// ValidateTaskScopes reports every task-scope problem: a declared scope is
// MVP or FULL (required declares it on every unit; an undeclared one is
// MVP), and no MVP unit depends on a FULL unit (with only MVP selected it
// could never start). existing maps the keys of units outside the
// candidate (an amendment's carried units) to their scopes.
func ValidateTaskScopes(c *PlanCandidate, existing map[string]TaskScope, required bool) PlanProblems {
	var p PlanProblems
	if c == nil {
		return p
	}
	scope := map[string]TaskScope{}
	for k, s := range existing {
		scope[k] = s
	}
	for _, u := range c.Units {
		if strings.TrimSpace(u.Scope) == "" {
			if required {
				p.add(u.Key, ProblemMissingField, "scope is required: MVP (minimum viable product) or FULL (full release)")
			} else {
				scope[u.Key] = TaskScopeMVP
			}
			continue
		}
		s, err := ParseTaskScope(u.Scope)
		if err != nil {
			p.add(u.Key, ProblemUnknownScope, "unknown scope %q: MVP or FULL", u.Scope)
			continue
		}
		scope[u.Key] = s
	}
	for _, u := range c.Units {
		if scope[u.Key] != TaskScopeMVP {
			continue
		}
		for _, d := range u.DependsOn {
			if scope[d] == TaskScopeFULL {
				p.add(u.Key, ProblemMVPDependsFull, "MVP task depends on FULL task %q: make %q MVP, or this task FULL", d, d)
			}
		}
	}
	return p
}

// SelectedCompletionEligible is PhaseCompletionEligible under the selected
// scope: the plan has units and each is DONE or deferred. With MVP
// selected a phase completes once its MVP work is done; its FULL tasks
// stay PENDING.
func SelectedCompletionEligible(plan *ExecutionPlan, units []*WorkUnit, selected TaskScope) bool {
	if plan == nil || len(units) == 0 {
		return false
	}
	for _, u := range units {
		if u == nil || (u.Status != WorkUnitStatusDONE && !Deferred(u, selected)) {
			return false
		}
	}
	return true
}
