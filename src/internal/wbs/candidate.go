package wbs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// CandidateUnit is one work unit proposed by a planner (WBS 16.15/16.16).
// Planner output is a report: it becomes a plan only after deterministic
// validation and transactional activation.
type CandidateUnit struct {
	Key              string                `json:"key"`
	Name             string                `json:"name"`
	Kind             string                `json:"kind"`
	Objective        string                `json:"objective"`
	Instructions     string                `json:"instructions,omitempty"`
	AcceptanceChecks []AcceptanceCheckSpec `json:"acceptance_checks,omitempty"`
	RequiredTests    []string              `json:"required_tests,omitempty"`
	Validation       []ValidationSpec      `json:"validation,omitempty"`
	DependsOn        []string              `json:"depends_on,omitempty"`
	Paths            []string              `json:"paths,omitempty"`
	SingleInvocation bool                  `json:"single_invocation,omitempty"`
}

// PlanCandidate is a planner-proposed decomposition of a phase.
type PlanCandidate struct {
	Summary string          `json:"summary,omitempty"`
	Units   []CandidateUnit `json:"units"`
}

// ErrMalformedCandidate reports planner output that is not a plan
// candidate (WBS 16.15).
var ErrMalformedCandidate = errors.New("wbs: malformed plan candidate")

// DecodePlanCandidate strictly decodes a plan candidate: unknown fields and
// trailing data are rejected (WBS 16.15).
func DecodePlanCandidate(raw []byte) (*PlanCandidate, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrMalformedCandidate)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c PlanCandidate
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedCandidate, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data", ErrMalformedCandidate)
	}
	return &c, nil
}

// PlanProblem is one validation finding against a candidate.
type PlanProblem struct {
	Unit    string `json:"unit,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Plan problem codes.
const (
	ProblemEmpty            = "EMPTY_PLAN"
	ProblemMissingField     = "MISSING_FIELD"
	ProblemDuplicateKey     = "DUPLICATE_KEY"
	ProblemUnknownKind      = "UNKNOWN_KIND"
	ProblemUnknownDep       = "UNKNOWN_DEPENDENCY"
	ProblemSelfDep          = "SELF_DEPENDENCY"
	ProblemCycle            = "DEPENDENCY_CYCLE"
	ProblemBadCheck         = "INVALID_ACCEPTANCE_CHECK"
	ProblemNotTestable      = "NOT_INDEPENDENTLY_TESTABLE"
	ProblemNoDependencies   = "MISSING_DEPENDENCIES"
	ProblemNoAcceptance     = "MISSING_ACCEPTANCE_CRITERIA"
	ProblemCompositionOrder = "COMPOSITION_NOT_SEPARATE"
	ProblemIntegrationOrder = "INTEGRATION_NOT_SEPARATE"
	ProblemNotSingle        = "NOT_SINGLE_INVOCATION"
	ProblemBadPath          = "PATH_OUT_OF_SCOPE"
	ProblemBadCommand       = "INVALID_VALIDATION_COMMAND"
	ProblemNoProgram        = "PROGRAM_NOT_FOUND"
	ProblemBadTestName      = "INVALID_TEST_NAME"
)

// PlanProblems is a validation report; nil/empty = valid.
type PlanProblems []PlanProblem

// Error joins the problems (deterministic order).
func (p PlanProblems) Error() string {
	parts := make([]string, len(p))
	for i, x := range p {
		if x.Unit != "" {
			parts[i] = fmt.Sprintf("%s: %s: %s", x.Unit, x.Code, x.Message)
		} else {
			parts[i] = fmt.Sprintf("%s: %s", x.Code, x.Message)
		}
	}
	return strings.Join(parts, "; ")
}

func (p *PlanProblems) add(unit, code, format string, a ...any) {
	*p = append(*p, PlanProblem{Unit: unit, Code: code, Message: fmt.Sprintf(format, a...)})
}

// ValidatePlanStructure reports every structural problem (WBS 16.16):
// at least one unit, required fields, unique keys, known kinds, known
// dependency references, no self dependency, no cycle, well-formed
// acceptance checks.
func ValidatePlanStructure(c *PlanCandidate) PlanProblems {
	return ValidateAmendmentStructure(c, nil)
}

// ValidateAmendmentStructure is ValidatePlanStructure for a repair
// candidate: dependencies may also reference the carried units' keys
// (WBS 16.66).
func ValidateAmendmentStructure(c *PlanCandidate, carried []string) PlanProblems {
	external := map[string]bool{}
	for _, k := range carried {
		external[k] = true
	}
	var p PlanProblems
	if c == nil || len(c.Units) == 0 {
		p.add("", ProblemEmpty, "plan has no units")
		return p
	}
	keys := map[string]int{}
	for i, u := range c.Units {
		if strings.TrimSpace(u.Key) == "" {
			p.add(fmt.Sprintf("#%d", i+1), ProblemMissingField, "key is required")
			continue
		}
		keys[u.Key]++
	}
	for _, k := range sortedKeys(keys) {
		if keys[k] > 1 {
			p.add(k, ProblemDuplicateKey, "key used by %d units", keys[k])
		}
	}
	for _, u := range c.Units {
		if strings.TrimSpace(u.Key) == "" {
			continue
		}
		if strings.TrimSpace(u.Name) == "" {
			p.add(u.Key, ProblemMissingField, "name is required")
		}
		if strings.TrimSpace(u.Objective) == "" {
			p.add(u.Key, ProblemMissingField, "objective is required")
		}
		if _, err := ParseWorkUnitKind(u.Kind); err != nil {
			p.add(u.Key, ProblemUnknownKind, "unknown kind %q", u.Kind)
		}
		for _, d := range u.DependsOn {
			switch {
			case d == u.Key:
				p.add(u.Key, ProblemSelfDep, "unit depends on itself")
			case keys[d] == 0 && !external[d]:
				p.add(u.Key, ProblemUnknownDep, "unknown dependency %q", d)
			}
		}
		for i, ch := range u.AcceptanceChecks {
			if msg := checkProblem(ch); msg != "" {
				p.add(u.Key, ProblemBadCheck, "acceptance check %d: %s", i+1, msg)
			}
		}
		for _, name := range u.RequiredTests {
			if !ValidTestName(name) {
				p.add(u.Key, ProblemBadTestName, "required test %q is not a Go test name (TestName or TestName/subtest, nothing else on the line)", name)
			}
		}
	}
	if cyc := candidateCycle(c); cyc != nil {
		p.add(cyc[0], ProblemCycle, "dependency cycle %s", strings.Join(cyc, " -> "))
	}
	return p
}

// testNameRE matches a Go test, benchmark, fuzz test or example name,
// optionally with subtest parts.
var testNameRE = regexp.MustCompile(`^(Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*(/[A-Za-z0-9_.\-#]+)*$`)

// ValidTestName reports whether name can name a Go test (the tests a
// [TEST] criterion or a task's required tests list).
func ValidTestName(name string) bool { return testNameRE.MatchString(name) }

func checkProblem(ch AcceptanceCheckSpec) string {
	if strings.TrimSpace(ch.Text) == "" {
		return "text is required"
	}
	switch ch.Verification {
	case VerifyCommand:
		if ch.Command == nil || strings.TrimSpace(ch.Command.Program) == "" {
			return "COMMAND verification requires a command"
		}
	case VerifyTest:
		if len(ch.Tests) == 0 {
			return "TEST verification requires tests"
		}
		for _, name := range ch.Tests {
			if !ValidTestName(name) {
				return fmt.Sprintf("test name %q is not a Go test name (TestName or TestName/subtest; describe the test in the criterion, not in the tests list)", name)
			}
		}
	case VerifyManual:
	default:
		return fmt.Sprintf("unknown verification %q", ch.Verification)
	}
	return ""
}

// candidateCycle returns one dependency cycle as a key path (first key
// repeated at the end), or nil. Deterministic (sorted traversal).
func candidateCycle(c *PlanCandidate) []string {
	deps := map[string][]string{}
	for _, u := range c.Units {
		if u.Key == "" {
			continue
		}
		ds := append([]string(nil), u.DependsOn...)
		sort.Strings(ds)
		deps[u.Key] = ds
	}
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(k string) bool
	visit = func(k string) bool {
		color[k] = 1
		stack = append(stack, k)
		for _, d := range deps[k] {
			if _, ok := deps[d]; !ok || d == k {
				continue
			}
			switch color[d] {
			case 0:
				if visit(d) {
					return true
				}
			case 1:
				for i, s := range stack {
					if s == d {
						found = append(append([]string(nil), stack[i:]...), d)
						return true
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = 2
		return false
	}
	for _, k := range sortedKeys(deps) {
		if color[k] == 0 && visit(k) {
			return found
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AtomicPolicy is the configured Atomic planning policy (WBS 16.17).
type AtomicPolicy struct {
	RequireIndependentlyTestable bool `json:"require_independently_testable"`
	RequireExplicitDependencies  bool `json:"require_explicit_dependencies"`
	RequireAcceptanceCriteria    bool `json:"require_acceptance_criteria"`
	SeparateComposition          bool `json:"separate_composition"`
	SeparateIntegration          bool `json:"separate_integration"`
	RequireSingleInvocation      bool `json:"require_single_invocation"`
}

// DefaultAtomicPolicy enables every requirement.
func DefaultAtomicPolicy() AtomicPolicy {
	return AtomicPolicy{true, true, true, true, true, true}
}

// ValidateAtomicPolicy reports every Atomic-policy violation (WBS 16.17,
// PLAN Decision 6):
//   - independently testable: at least one validation command, required
//     test, or COMMAND/TEST acceptance check;
//   - explicit dependencies: COMPOSITION / INTEGRATION / VALIDATION units
//     declare at least one dependency (they compose existing work);
//   - acceptance criteria: at least one acceptance check;
//   - separate composition: no ATOMIC unit depends on a COMPOSITION unit;
//   - separate integration: no ATOMIC / COMPOSITION unit depends on an
//     INTEGRATION unit;
//   - one worker invocation: single_invocation declared true.
func ValidateAtomicPolicy(c *PlanCandidate, pol AtomicPolicy) PlanProblems {
	var p PlanProblems
	if c == nil {
		return p
	}
	kind := map[string]WorkUnitKind{}
	for _, u := range c.Units {
		k, _ := ParseWorkUnitKind(u.Kind)
		kind[u.Key] = k
	}
	for _, u := range c.Units {
		k := kind[u.Key]
		if pol.RequireIndependentlyTestable && !testable(u) {
			p.add(u.Key, ProblemNotTestable, "unit declares no validation command, required test or verifiable acceptance check")
		}
		if pol.RequireExplicitDependencies && len(u.DependsOn) == 0 &&
			(k == WorkUnitKindCOMPOSITION || k == WorkUnitKindINTEGRATION || k == WorkUnitKindVALIDATION) {
			p.add(u.Key, ProblemNoDependencies, "%s unit must declare the units it depends on", k)
		}
		if pol.RequireAcceptanceCriteria && len(u.AcceptanceChecks) == 0 {
			p.add(u.Key, ProblemNoAcceptance, "unit declares no acceptance criteria")
		}
		for _, d := range u.DependsOn {
			dk := kind[d]
			if pol.SeparateComposition && k == WorkUnitKindATOMIC && dk == WorkUnitKindCOMPOSITION {
				p.add(u.Key, ProblemCompositionOrder, "atomic unit depends on composition unit %q", d)
			}
			if pol.SeparateIntegration && (k == WorkUnitKindATOMIC || k == WorkUnitKindCOMPOSITION) && dk == WorkUnitKindINTEGRATION {
				p.add(u.Key, ProblemIntegrationOrder, "%s unit depends on integration unit %q", k, d)
			}
		}
		if pol.RequireSingleInvocation && !u.SingleInvocation {
			p.add(u.Key, ProblemNotSingle, "unit does not declare single_invocation")
		}
	}
	return p
}

func testable(u CandidateUnit) bool {
	if len(u.Validation) > 0 || len(u.RequiredTests) > 0 {
		return true
	}
	for _, ch := range u.AcceptanceChecks {
		if ch.Verification == VerifyCommand || ch.Verification == VerifyTest {
			return true
		}
	}
	return false
}

// ValidatePlanScope reports scope violations (WBS 16.16): declared paths
// must be clean, relative and inside the working directory; validation
// commands must name a program (no shell strings with whitespace).
func ValidatePlanScope(c *PlanCandidate) PlanProblems {
	var p PlanProblems
	if c == nil {
		return p
	}
	for _, u := range c.Units {
		for _, pth := range u.Paths {
			if msg := pathProblem(pth); msg != "" {
				p.add(u.Key, ProblemBadPath, "path %q: %s", pth, msg)
			}
		}
		cmds := append([]ValidationSpec(nil), u.Validation...)
		for _, ch := range u.AcceptanceChecks {
			if ch.Command != nil {
				cmds = append(cmds, *ch.Command)
			}
		}
		for _, v := range cmds {
			if msg := ValidationSpecProblem(v); msg != "" {
				p.add(u.Key, ProblemBadCommand, "%s", msg)
			}
		}
	}
	return p
}

// ValidatePrograms reports validation and COMMAND-check programs the
// overseer cannot run: found reports whether a bare program name resolves
// in the overseer's environment. Programs given as a path are skipped (a
// task may create them).
func ValidatePrograms(c *PlanCandidate, found func(program string) bool) PlanProblems {
	var p PlanProblems
	if c == nil || found == nil {
		return p
	}
	for _, u := range c.Units {
		cmds := append([]ValidationSpec(nil), u.Validation...)
		for _, ch := range u.AcceptanceChecks {
			if ch.Command != nil {
				cmds = append(cmds, *ch.Command)
			}
		}
		seen := map[string]bool{}
		for _, v := range cmds {
			prog := strings.TrimSpace(v.Program)
			if prog == "" || strings.Contains(prog, "/") || seen[prog] {
				continue
			}
			seen[prog] = true
			if !found(prog) {
				p.add(u.Key, ProblemNoProgram, "program %q is not available where the overseer runs validation; "+
					"use a program it has or make the criterion [MANUAL]", prog)
			}
		}
	}
	return p
}

func pathProblem(p string) string {
	switch {
	case strings.TrimSpace(p) == "":
		return "empty"
	case strings.Contains(p, "\\"):
		return "backslash not allowed"
	case path.IsAbs(p):
		return "absolute path"
	}
	cl := path.Clean(p)
	if cl == ".." || strings.HasPrefix(cl, "../") {
		return "escapes the working directory"
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "contains .."
		}
	}
	return ""
}

// ValidationSpecProblem describes why a validation command is invalid
// ("" = valid): a program is required and must be a single token.
func ValidationSpecProblem(v ValidationSpec) string {
	prog := strings.TrimSpace(v.Program)
	switch {
	case prog == "":
		return "validation command has no program"
	case strings.ContainsAny(v.Program, " \t\n;|&$`"):
		return fmt.Sprintf("validation program %q must be a single executable, not a shell string", v.Program)
	case v.TimeoutSeconds < 0:
		return "validation timeout must not be negative"
	}
	return ""
}

// PlannedUnit is one unit of a plan about to be activated: either a new
// unit from a candidate or a unit carried into an amendment.
type PlannedUnit struct {
	Key           string
	Sequence      int
	Name          string
	Kind          WorkUnitKind
	Status        WorkUnitStatus
	Spec          WorkUnitSpecification
	DependsOnKeys []string
	// CarriedFrom is the superseded plan's unit this one copies (nil = new).
	CarriedFrom *uuid.UUID
}

// PlannedUnitsFromCandidate maps a validated candidate onto planned units:
// sequence = candidate order (1-based), status PENDING.
func PlannedUnitsFromCandidate(c *PlanCandidate) []PlannedUnit {
	return appendCandidate(nil, c, 0)
}

func appendCandidate(out []PlannedUnit, c *PlanCandidate, offset int) []PlannedUnit {
	if c == nil {
		return out
	}
	for i, u := range c.Units {
		k, _ := ParseWorkUnitKind(u.Kind)
		out = append(out, PlannedUnit{
			Key:      u.Key,
			Sequence: offset + i + 1,
			Name:     u.Name,
			Kind:     k,
			Status:   WorkUnitStatusPENDING,
			Spec: WorkUnitSpecification{
				Objective:                  u.Objective,
				ImplementationInstructions: u.Instructions,
				AcceptanceCriteria:         checkTexts(u.AcceptanceChecks),
				RequiredTests:              append([]string(nil), u.RequiredTests...),
				Key:                        u.Key,
				Validation:                 append([]ValidationSpec(nil), u.Validation...),
				AcceptanceChecks:           append([]AcceptanceCheckSpec(nil), u.AcceptanceChecks...),
				Paths:                      append([]string(nil), u.Paths...),
			},
			DependsOnKeys: append([]string(nil), u.DependsOn...),
		})
	}
	return out
}

func checkTexts(cs []AcceptanceCheckSpec) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return out
}

// UnitKey is a unit's plan-local key: its specification key, else a
// deterministic fallback from its sequence (pre-Phase 16 units).
func UnitKey(u *WorkUnit) string {
	if u.Specification != nil && u.Specification.Key != "" {
		return u.Specification.Key
	}
	return fmt.Sprintf("unit-%d", u.Sequence)
}

// BuildAmendment builds the units of an amended plan version (WBS
// 16.66–16.68, PLAN Decision 10): every unit of the current plan is
// carried (same key, sequence and status — DONE stays DONE, anything else
// restarts as PENDING) with CarriedFrom set, followed by the repair units
// (sequence continues). Repair dependencies may reference carried keys;
// unknown references or keys colliding with carried units are problems.
// carriedDeps maps a current unit ID to the IDs it depends on.
func BuildAmendment(current []*WorkUnit, carriedDeps map[uuid.UUID][]uuid.UUID, repair *PlanCandidate) ([]PlannedUnit, PlanProblems) {
	var p PlanProblems
	cur := append([]*WorkUnit(nil), current...)
	sort.SliceStable(cur, func(i, j int) bool { return cur[i].Sequence < cur[j].Sequence })
	keyOf := map[uuid.UUID]string{}
	known := map[string]bool{}
	var out []PlannedUnit
	maxSeq := 0
	for _, u := range cur {
		keyOf[u.ID] = UnitKey(u)
	}
	for _, u := range cur {
		k := keyOf[u.ID]
		known[k] = true
		st := WorkUnitStatusPENDING
		if u.Status == WorkUnitStatusDONE {
			st = WorkUnitStatusDONE
		}
		var spec WorkUnitSpecification
		if u.Specification != nil {
			spec = *u.Specification
		}
		spec.Key = k
		if spec.Objective == "" {
			spec.Objective = u.Name
		}
		var deps []string
		for _, d := range carriedDeps[u.ID] {
			if dk, ok := keyOf[d]; ok {
				deps = append(deps, dk)
			}
		}
		sort.Strings(deps)
		id := u.ID
		out = append(out, PlannedUnit{
			Key: k, Sequence: u.Sequence, Name: u.Name, Kind: u.Kind, Status: st,
			Spec: spec, DependsOnKeys: deps, CarriedFrom: &id,
		})
		if u.Sequence > maxSeq {
			maxSeq = u.Sequence
		}
	}
	if repair == nil || len(repair.Units) == 0 {
		p.add("", ProblemEmpty, "repair plan adds no units")
		return out, p
	}
	newKeys := map[string]bool{}
	for _, u := range repair.Units {
		if known[u.Key] {
			p.add(u.Key, ProblemDuplicateKey, "repair unit reuses carried key")
		}
		newKeys[u.Key] = true
	}
	for _, u := range repair.Units {
		for _, d := range u.DependsOn {
			if !known[d] && !newKeys[d] {
				p.add(u.Key, ProblemUnknownDep, "unknown dependency %q", d)
			}
		}
	}
	return appendCandidate(out, repair, maxSeq), p
}

// Progress is DONE / total over one plan's units (WBS 16.59/16.68);
// always computed, never stored.
type Progress struct {
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Percent float64 `json:"percent"`
}

// PlanProgress counts DONE units (VALIDATING is not DONE, WBS 16.59).
func PlanProgress(units []*WorkUnit) Progress {
	var pr Progress
	for _, u := range units {
		if u == nil {
			continue
		}
		pr.Total++
		if u.Status == WorkUnitStatusDONE {
			pr.Done++
		}
	}
	if pr.Total > 0 {
		pr.Percent = float64(pr.Done) * 100 / float64(pr.Total)
	}
	return pr
}
