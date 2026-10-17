// Package invariants is the catalog of production invariants (Phase 18,
// WBS 18.39 / 18.171 / 18.177) and the deterministic report the read-only
// scanner produces. Detection never repairs anything (WBS 18.40 / 18.181).
package invariants

import (
	"sort"
	"strings"
)

// ID names one invariant.
type ID string

// Invariants (PLAN P18-06).
const (
	OneActiveExecution         ID = "ONE_ACTIVE_EXECUTION"
	OneCurrentUnit             ID = "ONE_CURRENT_UNIT"
	OneActiveDispatch          ID = "ONE_ACTIVE_DISPATCH"
	OneActiveContinuationChain ID = "ONE_ACTIVE_CONTINUATION_CHAIN"
	OneCurrentPhase            ID = "ONE_CURRENT_PHASE"
	DoneRequiresValidation     ID = "DONE_REQUIRES_VALIDATION"
	DoneRequiresCheckpoint     ID = "DONE_REQUIRES_CHECKPOINT"
	CurrentPlanMatchesWBS      ID = "CURRENT_PLAN_MATCHES_WBS"
	CurrentUnitOnActivePlan    ID = "CURRENT_UNIT_ON_ACTIVE_PLAN"
	RuntimeBindingValid        ID = "RUNTIME_BINDING_VALID"
	NoOrphans                  ID = "NO_ORPHANS"
	DependencyRefsValid        ID = "DEPENDENCY_REFS_VALID"
	CheckpointConsistent       ID = "CHECKPOINT_CONSISTENT"
	OwnershipConsistent        ID = "OWNERSHIP_CONSISTENT"
)

// Severity of a violation.
type Severity string

const (
	// Critical violations block further side effects for the execution.
	Critical Severity = "CRITICAL"
	// Warning violations are reported only.
	Warning Severity = "WARNING"
)

// Definition describes an invariant.
type Definition struct {
	ID          ID
	Severity    Severity
	Description string
}

var catalog = []Definition{
	{OneActiveExecution, Critical, "A project has at most one active execution."},
	{OneCurrentUnit, Critical, "An execution has at most one claimed (EXECUTING / VALIDATING) work unit."},
	{OneActiveDispatch, Critical, "An execution has at most one in-flight worker dispatch."},
	{OneActiveContinuationChain, Critical, "A work unit has at most one active continuation chain."},
	{OneCurrentPhase, Critical, "A project has at most one EXECUTING / VALIDATING phase."},
	{DoneRequiresValidation, Critical, "Every DONE work unit claimed by an execution has a passing deterministic validation run."},
	{DoneRequiresCheckpoint, Critical, "Under the REQUIRED checkpoint policy every DONE work unit has a created checkpoint."},
	{CurrentPlanMatchesWBS, Warning, "The active plan of the current phase was planned from the current WBS version (else replanning is pending)."},
	{CurrentUnitOnActivePlan, Critical, "An execution's current work unit belongs to a non-superseded plan."},
	{RuntimeBindingValid, Critical, "An execution's runtime state, current phase and current unit belong to its project."},
	{NoOrphans, Critical, "Execution-scoped records reference an existing execution / project."},
	{DependencyRefsValid, Critical, "Work-unit dependencies reference units of the same plan."},
	{CheckpointConsistent, Critical, "Checkpoint rows reference existing commits and their own project / execution."},
	{OwnershipConsistent, Critical, "Project-scoped records never reference another project's records."},
}

// Catalog returns every invariant definition (stable order).
func Catalog() []Definition { return append([]Definition(nil), catalog...) }

// Lookup returns the definition of id.
func Lookup(id ID) (Definition, bool) {
	for _, d := range catalog {
		if d.ID == id {
			return d, true
		}
	}
	return Definition{}, false
}

// Violation is one detected breach.
type Violation struct {
	Invariant   ID       `json:"invariant"`
	Severity    Severity `json:"severity"`
	ProjectID   string   `json:"project_id,omitempty"`
	ExecutionID string   `json:"execution_id,omitempty"`
	EntityIDs   []string `json:"entity_ids,omitempty"`
	Detail      string   `json:"detail"`
}

func (v Violation) key() string {
	ids := append([]string(nil), v.EntityIDs...)
	sort.Strings(ids)
	return strings.Join([]string{string(v.Invariant), v.ProjectID, v.ExecutionID, strings.Join(ids, ","), v.Detail}, "|")
}

// Report is a deterministic scan result.
type Report struct {
	Scope      string      `json:"scope"`
	Checked    []ID        `json:"checked"`
	Violations []Violation `json:"violations"`
}

// NewReport builds a report: violations are deduplicated, entity ids
// sorted, severities filled from the catalog, and everything ordered so
// the same database state yields byte-identical JSON.
func NewReport(scope string, checked []ID, vs []Violation) Report {
	seen := map[string]bool{}
	out := make([]Violation, 0, len(vs))
	for _, v := range vs {
		v.EntityIDs = append([]string(nil), v.EntityIDs...)
		sort.Strings(v.EntityIDs)
		if v.Severity == "" {
			if d, ok := Lookup(v.Invariant); ok {
				v.Severity = d.Severity
			}
		}
		k := v.key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	ch := append([]ID(nil), checked...)
	sort.Slice(ch, func(i, j int) bool { return ch[i] < ch[j] })
	return Report{Scope: scope, Checked: ch, Violations: out}
}

// OK reports whether no violation was found.
func (r Report) OK() bool { return len(r.Violations) == 0 }

// Critical returns the critical violations.
func (r Report) Critical() []Violation {
	var out []Violation
	for _, v := range r.Violations {
		if v.Severity == Critical {
			out = append(out, v)
		}
	}
	return out
}
