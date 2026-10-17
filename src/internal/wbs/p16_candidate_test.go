package wbs

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func hasCode(p PlanProblems, unit, code string) bool {
	for _, x := range p {
		if x.Code == code && (unit == "" || x.Unit == unit) {
			return true
		}
	}
	return false
}

func goodUnit(key, kind string, deps ...string) CandidateUnit {
	return CandidateUnit{
		Key: key, Name: "unit " + key, Kind: kind, Objective: "do " + key,
		AcceptanceChecks: []AcceptanceCheckSpec{{Text: "builds", Verification: VerifyCommand, Command: &ValidationSpec{Program: "go", Args: []string{"build", "./..."}}}},
		Validation:       []ValidationSpec{{Program: "go", Args: []string{"test", "./..."}}},
		DependsOn:        deps, Paths: []string{"internal/" + key}, SingleInvocation: true,
	}
}

// P16-16: Phase 16 specification fields are optional; old specs decode.
func TestP1616SpecificationBackwardCompatible(t *testing.T) {
	var s WorkUnitSpecification
	if err := json.Unmarshal([]byte(`{"objective":"x","acceptance_criteria":["a"],"required_tests":["T"]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Key != "" || s.Validation != nil || s.AcceptanceChecks != nil || s.Paths != nil {
		t.Fatalf("unexpected phase 16 fields: %+v", s)
	}
	s.Validation = []ValidationSpec{{Program: "go", Args: []string{"test"}}}
	b, _ := json.Marshal(s)
	var back WorkUnitSpecification
	if err := json.Unmarshal(b, &back); err != nil || back.Validation[0].Program != "go" {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}

// P16-17 (16.15): malformed planner output is rejected.
func TestP1617DecodePlanCandidate(t *testing.T) {
	ok := `{"units":[{"key":"a","name":"A","kind":"ATOMIC","objective":"o"}]}`
	c, err := DecodePlanCandidate([]byte(ok))
	if err != nil || len(c.Units) != 1 {
		t.Fatalf("valid: %v", err)
	}
	for _, bad := range []string{``, `{`, `[]`, `{"units":[],"extra":1}`, ok + `{}`, `{"units":[{"key":"a","bogus":1}]}`} {
		if _, err := DecodePlanCandidate([]byte(bad)); !errors.Is(err, ErrMalformedCandidate) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

// P16-18 (16.16): schema / dependencies / cycles / kinds.
func TestP1618ValidatePlanStructure(t *testing.T) {
	if p := ValidatePlanStructure(&PlanCandidate{}); !hasCode(p, "", ProblemEmpty) {
		t.Fatal("empty plan accepted")
	}
	good := &PlanCandidate{Units: []CandidateUnit{goodUnit("a", "ATOMIC"), goodUnit("b", "ATOMIC"), goodUnit("c", "COMPOSITION", "a", "b")}}
	if p := ValidatePlanStructure(good); len(p) != 0 {
		t.Fatalf("good plan rejected: %v", p)
	}
	bad := &PlanCandidate{Units: []CandidateUnit{
		{Key: "a", Name: "A", Kind: "ATOMIC", Objective: "o", DependsOn: []string{"a", "zzz"}},
		{Key: "a", Name: "A2", Kind: "WHATEVER", Objective: "o"},
		{Key: "b", Kind: "ATOMIC", AcceptanceChecks: []AcceptanceCheckSpec{{Text: "t", Verification: "VIBES"}, {Text: "c", Verification: VerifyCommand}}},
		{Name: "nokey"},
	}}
	p := ValidatePlanStructure(bad)
	for _, want := range [][2]string{{"a", ProblemSelfDep}, {"a", ProblemUnknownDep}, {"a", ProblemDuplicateKey}, {"a", ProblemUnknownKind},
		{"b", ProblemMissingField}, {"b", ProblemBadCheck}, {"#4", ProblemMissingField}} {
		if !hasCode(p, want[0], want[1]) {
			t.Fatalf("missing %v in %v", want, p)
		}
	}
	cyc := &PlanCandidate{Units: []CandidateUnit{goodUnit("a", "ATOMIC", "c"), goodUnit("b", "ATOMIC", "a"), goodUnit("c", "ATOMIC", "b")}}
	p = ValidatePlanStructure(cyc)
	if !hasCode(p, "", ProblemCycle) {
		t.Fatalf("cycle not detected: %v", p)
	}
	if p2 := ValidatePlanStructure(cyc); p2.Error() != p.Error() {
		t.Fatal("non-deterministic report")
	}
}

// P16-19 (16.17): each missing declaration fails.
func TestP1619AtomicPolicy(t *testing.T) {
	pol := DefaultAtomicPolicy()
	good := &PlanCandidate{Units: []CandidateUnit{goodUnit("a", "ATOMIC"), goodUnit("c", "COMPOSITION", "a"), goodUnit("i", "INTEGRATION", "c"), goodUnit("v", "VALIDATION", "i")}}
	if p := ValidateAtomicPolicy(good, pol); len(p) != 0 {
		t.Fatalf("good rejected: %v", p)
	}
	bare := CandidateUnit{Key: "x", Name: "X", Kind: "ATOMIC", Objective: "o"}
	p := ValidateAtomicPolicy(&PlanCandidate{Units: []CandidateUnit{bare}}, pol)
	for _, code := range []string{ProblemNotTestable, ProblemNoAcceptance, ProblemNotSingle} {
		if !hasCode(p, "x", code) {
			t.Fatalf("missing %s: %v", code, p)
		}
	}
	comp := goodUnit("c", "COMPOSITION")
	p = ValidateAtomicPolicy(&PlanCandidate{Units: []CandidateUnit{comp}}, pol)
	if !hasCode(p, "c", ProblemNoDependencies) {
		t.Fatalf("composition without deps accepted: %v", p)
	}
	layered := &PlanCandidate{Units: []CandidateUnit{goodUnit("c", "COMPOSITION", "a"), goodUnit("a", "ATOMIC", "c"), goodUnit("i", "INTEGRATION", "a"), goodUnit("c2", "COMPOSITION", "i")}}
	p = ValidateAtomicPolicy(layered, pol)
	if !hasCode(p, "a", ProblemCompositionOrder) || !hasCode(p, "c2", ProblemIntegrationOrder) {
		t.Fatalf("layering not enforced: %v", p)
	}
	if p := ValidateAtomicPolicy(&PlanCandidate{Units: []CandidateUnit{bare}}, AtomicPolicy{}); len(p) != 0 {
		t.Fatalf("disabled policy reported: %v", p)
	}
}

// P16-20 (16.16 scope).
func TestP1620PlanScope(t *testing.T) {
	u := goodUnit("a", "ATOMIC")
	u.Paths = []string{"ok/file.go", "/etc/passwd", "../x", "a/../../b", "", `a\b`}
	u.Validation = append(u.Validation, ValidationSpec{Program: "go test ./..."}, ValidationSpec{})
	p := ValidatePlanScope(&PlanCandidate{Units: []CandidateUnit{u}})
	n := 0
	for _, x := range p {
		if x.Code == ProblemBadPath {
			n++
		}
	}
	if n != 5 || !hasCode(p, "a", ProblemBadCommand) {
		t.Fatalf("scope problems: %v", p)
	}
	if p := ValidatePlanScope(&PlanCandidate{Units: []CandidateUnit{goodUnit("a", "ATOMIC")}}); len(p) != 0 {
		t.Fatal(p)
	}
}

// P16-21 (16.66–16.68): carried units + repair units; 10/10 → 10/12.
func TestP1621BuildAmendment(t *testing.T) {
	plan := uuid.New()
	var cur []*WorkUnit
	deps := map[uuid.UUID][]uuid.UUID{}
	for i := 1; i <= 10; i++ {
		u, _ := NewWorkUnit(plan, i, "u", WorkUnitKindATOMIC)
		u.Status = WorkUnitStatusDONE
		u.Specification = &WorkUnitSpecification{Objective: "o", Key: ""}
		if i > 1 {
			deps[u.ID] = []uuid.UUID{cur[i-2].ID}
		}
		cur = append(cur, u)
	}
	if pr := PlanProgress(cur); pr.Done != 10 || pr.Total != 10 || pr.Percent != 100 {
		t.Fatalf("before: %+v", pr)
	}
	repair := &PlanCandidate{Units: []CandidateUnit{goodUnit("r1", "ATOMIC", "unit-10"), goodUnit("r2", "VALIDATION", "r1")}}
	units, p := BuildAmendment(cur, deps, repair)
	if len(p) != 0 {
		t.Fatal(p)
	}
	if len(units) != 12 || units[10].Sequence != 11 || units[11].Sequence != 12 {
		t.Fatalf("units: %d", len(units))
	}
	var after []*WorkUnit
	for _, pu := range units[:10] {
		if pu.CarriedFrom == nil || pu.Status != WorkUnitStatusDONE {
			t.Fatalf("carried unit wrong: %+v", pu)
		}
		after = append(after, &WorkUnit{Status: pu.Status})
	}
	if units[1].DependsOnKeys[0] != "unit-1" {
		t.Fatalf("carried deps lost: %v", units[1].DependsOnKeys)
	}
	for _, pu := range units[10:] {
		if pu.CarriedFrom != nil || pu.Status != WorkUnitStatusPENDING {
			t.Fatalf("repair unit wrong: %+v", pu)
		}
		after = append(after, &WorkUnit{Status: pu.Status})
	}
	pr := PlanProgress(after)
	if pr.Done != 10 || pr.Total != 12 || math.Abs(pr.Percent-83.333) > 0.01 {
		t.Fatalf("after: %+v", pr)
	}
	_, p = BuildAmendment(cur, deps, &PlanCandidate{Units: []CandidateUnit{goodUnit("unit-3", "ATOMIC", "nope")}})
	if !hasCode(p, "unit-3", ProblemDuplicateKey) || !hasCode(p, "unit-3", ProblemUnknownDep) {
		t.Fatalf("amendment problems: %v", p)
	}
	if _, p = BuildAmendment(cur, deps, nil); !hasCode(p, "", ProblemEmpty) {
		t.Fatal("empty repair accepted")
	}
	// VALIDATING is not progress (16.59).
	if pr := PlanProgress([]*WorkUnit{{Status: WorkUnitStatusVALIDATING}, {Status: WorkUnitStatusDONE}}); pr.Done != 1 {
		t.Fatal("VALIDATING counted")
	}
}

// A validation or COMMAND-check program the overseer cannot find rejects
// the plan; programs given as a path (a task may create them) are skipped.
func TestValidatePrograms(t *testing.T) {
	c := &PlanCandidate{Units: []CandidateUnit{{
		Key:        "A",
		Validation: []ValidationSpec{{Program: "go", Args: []string{"test", "./..."}}, {Program: "docker", Args: []string{"info"}}, {Program: "./scripts/check.sh"}},
		AcceptanceChecks: []AcceptanceCheckSpec{
			{Text: "probe", Verification: VerifyCommand, Command: &ValidationSpec{Program: "docker", Args: []string{"ps"}}},
			{Text: "lint", Verification: VerifyCommand, Command: &ValidationSpec{Program: "golint"}},
		},
	}}}
	have := map[string]bool{"go": true}
	p := ValidatePrograms(c, func(prog string) bool { return have[prog] })
	if len(p) != 2 || p[0].Code != ProblemNoProgram || p[0].Unit != "A" || p[1].Code != ProblemNoProgram {
		t.Fatalf("problems %+v, want docker and golint once each", p)
	}
	if got := ValidatePrograms(c, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("all programs found: %+v", got)
	}
}

// Test names in [TEST] criteria and required tests must be Go test names;
// a description in their place rejects the plan.
func TestValidTestName(t *testing.T) {
	for name, want := range map[string]bool{
		"TestT001": true, "TestFoo_Bar": true, "TestFoo/sub_case": true, "BenchmarkX": true, "FuzzParse": true, "ExampleRun": true,
		"TestT001: Build CLI entry point; expected successful build": false, "T001": false, "Test Foo": false, "": false, "go test": false,
	} {
		if got := ValidTestName(name); got != want {
			t.Errorf("ValidTestName(%q) = %v, want %v", name, got, want)
		}
	}
	bad := "TestT001: Build CLI entry point; expected successful build"
	c := &PlanCandidate{Units: []CandidateUnit{{Key: "1.1.1", Name: "Init", Objective: "o", Kind: "ATOMIC", RequiredTests: []string{bad},
		AcceptanceChecks: []AcceptanceCheckSpec{{Text: "builds", Verification: VerifyTest, Tests: []string{bad}}}}}}
	p := ValidateAmendmentStructure(c, nil)
	codes := map[string]bool{}
	for _, x := range p {
		codes[x.Code] = true
	}
	if !codes[ProblemBadCheck] || !codes[ProblemBadTestName] {
		t.Fatalf("problems %+v, want %s and %s", p, ProblemBadCheck, ProblemBadTestName)
	}
}
