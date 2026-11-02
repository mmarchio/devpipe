package wbs

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func scopedUnit(seq int, status WorkUnitStatus, scope TaskScope) *WorkUnit {
	return &WorkUnit{ID: uuid.New(), Sequence: seq, Name: "unit", Kind: WorkUnitKindATOMIC, Status: status,
		Specification: &WorkUnitSpecification{Objective: "o", Scope: scope}}
}

func TestParseTaskScope(t *testing.T) {
	for in, want := range map[string]TaskScope{"MVP": TaskScopeMVP, " mvp ": TaskScopeMVP, "full": TaskScopeFULL} {
		if got, err := ParseTaskScope(in); err != nil || got != want {
			t.Errorf("ParseTaskScope(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "BOTH", "core"} {
		if _, err := ParseTaskScope(in); err == nil {
			t.Errorf("ParseTaskScope(%q) accepted", in)
		}
	}
}

func TestUnitScopeDefaultsToMVP(t *testing.T) {
	if s := UnitScope(&WorkUnit{}); s != TaskScopeMVP {
		t.Fatalf("unit without specification: %q", s)
	}
	if s := UnitScope(scopedUnit(1, WorkUnitStatusPENDING, "")); s != TaskScopeMVP {
		t.Fatalf("uncategorized unit: %q", s)
	}
	if s := UnitScope(scopedUnit(1, WorkUnitStatusPENDING, TaskScopeFULL)); s != TaskScopeFULL {
		t.Fatalf("FULL unit: %q", s)
	}
}

func TestDeferred(t *testing.T) {
	cases := []struct {
		status   WorkUnitStatus
		scope    TaskScope
		selected TaskScope
		want     bool
	}{
		{WorkUnitStatusPENDING, TaskScopeFULL, TaskScopeMVP, true},
		{WorkUnitStatusREADY, TaskScopeFULL, TaskScopeMVP, true},
		{WorkUnitStatusFAILED, TaskScopeFULL, TaskScopeMVP, true},
		{WorkUnitStatusBLOCKED, TaskScopeFULL, TaskScopeMVP, true},
		// Work under way or DONE is never deferred.
		{WorkUnitStatusEXECUTING, TaskScopeFULL, TaskScopeMVP, false},
		{WorkUnitStatusINCOMPLETE, TaskScopeFULL, TaskScopeMVP, false},
		{WorkUnitStatusVALIDATING, TaskScopeFULL, TaskScopeMVP, false},
		{WorkUnitStatusDONE, TaskScopeFULL, TaskScopeMVP, false},
		{WorkUnitStatusPENDING, TaskScopeMVP, TaskScopeMVP, false},
		{WorkUnitStatusPENDING, "", TaskScopeMVP, false},
		{WorkUnitStatusPENDING, TaskScopeFULL, TaskScopeFULL, false},
	}
	for _, c := range cases {
		if got := Deferred(scopedUnit(1, c.status, c.scope), c.selected); got != c.want {
			t.Errorf("Deferred(%s %s, selected %s) = %v, want %v", c.scope, c.status, c.selected, got, c.want)
		}
	}
}

func TestSelectedUnits(t *testing.T) {
	a := scopedUnit(1, WorkUnitStatusPENDING, TaskScopeMVP)
	b := scopedUnit(2, WorkUnitStatusPENDING, TaskScopeFULL)
	c := scopedUnit(3, WorkUnitStatusEXECUTING, TaskScopeFULL)
	all := []*WorkUnit{a, b, c}
	if got := SelectedUnits(all, TaskScopeFULL); len(got) != 3 {
		t.Fatalf("FULL selects every unit, got %d", len(got))
	}
	got := SelectedUnits(all, TaskScopeMVP)
	if len(got) != 2 || got[0] != a || got[1] != c {
		t.Fatalf("MVP selection = %v", got)
	}
}

func TestSelectedCompletionEligible(t *testing.T) {
	plan := &ExecutionPlan{ID: uuid.New()}
	done := scopedUnit(1, WorkUnitStatusDONE, TaskScopeMVP)
	full := scopedUnit(2, WorkUnitStatusPENDING, TaskScopeFULL)
	units := []*WorkUnit{done, full}
	if SelectedCompletionEligible(plan, units, TaskScopeFULL) {
		t.Fatal("FULL: a pending FULL unit holds the phase open")
	}
	if !SelectedCompletionEligible(plan, units, TaskScopeMVP) {
		t.Fatal("MVP: a pending FULL unit is deferred")
	}
	if !SelectedCompletionEligible(plan, []*WorkUnit{full}, TaskScopeMVP) {
		t.Fatal("MVP: a plan of deferred units only is complete")
	}
	running := scopedUnit(3, WorkUnitStatusEXECUTING, TaskScopeFULL)
	if SelectedCompletionEligible(plan, []*WorkUnit{done, running}, TaskScopeMVP) {
		t.Fatal("MVP: a FULL unit under way holds the phase open")
	}
	if SelectedCompletionEligible(nil, units, TaskScopeMVP) || SelectedCompletionEligible(plan, nil, TaskScopeMVP) {
		t.Fatal("no plan / no units is never complete")
	}
}

func TestValidateTaskScopes(t *testing.T) {
	c := &PlanCandidate{Units: []CandidateUnit{
		{Key: "A", Scope: "MVP"},
		{Key: "B", Scope: "FULL", DependsOn: []string{"A"}},
		{Key: "C", Scope: "MVP", DependsOn: []string{"B", "X"}},
		{Key: "D"},
		{Key: "E", Scope: "maybe"},
	}}
	p := ValidateTaskScopes(c, map[string]TaskScope{"X": TaskScopeFULL}, true)
	codes := map[string]string{}
	for _, x := range p {
		codes[x.Unit+":"+x.Code] = x.Message
	}
	for _, want := range []string{"C:" + ProblemMVPDependsFull, "D:" + ProblemMissingField, "E:" + ProblemUnknownScope} {
		if _, ok := codes[want]; !ok {
			t.Errorf("missing problem %s in %v", want, p)
		}
	}
	if n := strings.Count(p.Error(), ProblemMVPDependsFull); n != 2 {
		t.Errorf("C depends on FULL B and FULL X: %d problems in %v", n, p)
	}
	if len(p) != 4 {
		t.Errorf("problems = %v", p)
	}
	ok := &PlanCandidate{Units: []CandidateUnit{{Key: "A", Scope: "mvp"}, {Key: "B", Scope: "FULL", DependsOn: []string{"A"}}}}
	if p := ValidateTaskScopes(ok, nil, true); len(p) != 0 {
		t.Fatalf("valid candidate: %v", p)
	}
	// Not required: an undeclared scope is MVP, so it may not depend on FULL.
	opt := &PlanCandidate{Units: []CandidateUnit{{Key: "A", Scope: "FULL"}, {Key: "B", DependsOn: []string{"A"}}, {Key: "C"}}}
	p = ValidateTaskScopes(opt, nil, false)
	if len(p) != 1 || p[0].Unit != "B" || p[0].Code != ProblemMVPDependsFull {
		t.Fatalf("optional scopes: %v", p)
	}
}

func TestPlannedUnitsCarryScope(t *testing.T) {
	c := &PlanCandidate{Units: []CandidateUnit{{Key: "A", Name: "a", Kind: "ATOMIC", Objective: "o", Scope: "full"}}}
	pu := PlannedUnitsFromCandidate(c)
	if len(pu) != 1 || pu[0].Spec.Scope != TaskScopeFULL {
		t.Fatalf("planned = %+v", pu)
	}
	cur := scopedUnit(1, WorkUnitStatusPENDING, TaskScopeFULL)
	am, p := BuildAmendment([]*WorkUnit{cur}, nil, &PlanCandidate{Units: []CandidateUnit{{Key: "R", Name: "r", Kind: "ATOMIC", Objective: "o", Scope: "MVP"}}})
	if len(p) != 0 || len(am) != 2 || am[0].Spec.Scope != TaskScopeFULL || am[1].Spec.Scope != TaskScopeMVP {
		t.Fatalf("amendment = %+v, %v", am, p)
	}
}

func TestTaskFilesParseScope(t *testing.T) {
	md := "## T1 — One\nKind: ATOMIC\nScope: full\n\n### Objective\nDo it.\n"
	d, err := ParseTasks(md)
	if err != nil || len(d.Tasks) != 1 || d.Tasks[0].Unit.Scope != "FULL" {
		t.Fatalf("markdown: %+v, %v", d, err)
	}
	kv := "task: T1 — One\nkind: ATOMIC\nscope: mvp\nobjective: Do it.\n"
	d, err = ParseTasks(kv)
	if err != nil || len(d.Tasks) != 1 || d.Tasks[0].Unit.Scope != "MVP" {
		t.Fatalf("key:value: %+v, %v", d, err)
	}
}
