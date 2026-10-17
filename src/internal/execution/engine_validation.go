package execution

import (
	"strings"
	"time"

	"overseer/internal/wbs"
)

// ValidationScope is what a deterministic validation run verifies.
type ValidationScope string

// Validation scopes.
const (
	ScopeUnit      ValidationScope = "UNIT"
	ScopePhase     ValidationScope = "PHASE"
	ScopeFinal     ValidationScope = "FINAL"
	ScopeHardening ValidationScope = "HARDENING"
)

// ValidationOutcome is the deterministic result of one command or run
// (WBS 16.44–16.47).
type ValidationOutcome string

// Validation outcomes (WBS 16.44–16.47, 18.66). FAIL is the spec's
// TEST_FAILURE: the command ran and reported failure (non-zero exit, or a
// crash of the code under test). TIMEOUT: the command hit its deadline and
// its whole process group was killed. EXECUTION_FAILURE: the command could
// not be started or was killed from outside (SIGKILL / SIGTERM — e.g. the
// OOM killer). INFRA_ERROR = the check could not run at all (missing
// executable, bad CWD, full disk) — never a code failure. CANCELLED: the
// overseer itself stopped it (shutdown); never PASS / FAIL. NOT_CONFIGURED
// = no command resolved — never a PASS.
const (
	ValidationPass             ValidationOutcome = "PASS"
	ValidationFail             ValidationOutcome = "FAIL"
	ValidationTimeout          ValidationOutcome = "TIMEOUT"
	ValidationExecutionFailure ValidationOutcome = "EXECUTION_FAILURE"
	ValidationInfraError       ValidationOutcome = "INFRA_ERROR"
	ValidationCancelled        ValidationOutcome = "CANCELLED"
	ValidationNotConfigured    ValidationOutcome = "NOT_CONFIGURED"
)

// CodeFailure reports a per-command outcome that is the code's fault
// (routes to FIX, Decision 9): FAIL or TIMEOUT.
func (o ValidationOutcome) CodeFailure() bool { return o == ValidationFail || o == ValidationTimeout }

// CommandKey identifies a command by program + arguments.
func CommandKey(v wbs.ValidationSpec) string {
	return v.Program + "\x00" + strings.Join(v.Args, "\x00")
}

// ResolveValidationCommands returns the ordered, de-duplicated commands a
// run must execute (WBS 16.43, PLAN Decision 8). UNIT: the unit's declared
// validation, then its COMMAND acceptance checks, then the project's unit
// policy. PHASE / FINAL: the project policy. HARDENING: none (the caller
// supplies them). Worker results are never an input, so a worker cannot
// replace the required tests with its own claims. Empty = NOT_CONFIGURED.
func ResolveValidationCommands(spec *wbs.WorkUnitSpecification, st EngineSettings, scope ValidationScope) []wbs.ValidationSpec {
	var src []wbs.ValidationSpec
	switch scope {
	case ScopeUnit:
		if spec != nil {
			src = append(src, spec.Validation...)
			for _, ch := range spec.AcceptanceChecks {
				if ch.Verification == wbs.VerifyCommand && ch.Command != nil {
					src = append(src, *ch.Command)
				}
			}
		}
		src = append(src, st.UnitValidation...)
	case ScopePhase:
		src = append(src, st.PhaseValidation...)
	case ScopeFinal:
		src = append(src, st.FinalValidation...)
	}
	seen := map[string]bool{}
	var out []wbs.ValidationSpec
	for _, v := range src {
		k := CommandKey(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}

// ValidationRecord is one executed command (WBS 16.44).
type ValidationRecord struct {
	Name        string            `json:"name,omitempty"`
	Program     string            `json:"program"`
	Args        []string          `json:"args"`
	CWD         string            `json:"cwd"`
	ExitCode    *int              `json:"exit_code"`
	Stdout      string            `json:"stdout"`
	Stderr      string            `json:"stderr"`
	Duration    time.Duration     `json:"duration"`
	Timeout     time.Duration     `json:"timeout"`
	TimedOut    bool              `json:"timed_out"`
	Outcome     ValidationOutcome `json:"outcome"`
	InfraReason string            `json:"infra_reason,omitempty"`
}

// AggregateValidation folds command outcomes into a run outcome: none →
// NOT_CONFIGURED; any INFRA_ERROR → INFRA_ERROR (inability to run is
// reported before failures); any FAIL → FAIL; else PASS.
func AggregateValidation(records []ValidationRecord) ValidationOutcome {
	if len(records) == 0 {
		return ValidationNotConfigured
	}
	// Precedence: CANCELLED (the run is abandoned, never judged) >
	// infrastructure (INFRA_ERROR / EXECUTION_FAILURE: never a code
	// failure, WBS 18.66 / 18.67) > code failure (FAIL / TIMEOUT → FIX) >
	// PASS.
	out := ValidationPass
	infra := false
	for _, r := range records {
		switch {
		case r.Outcome == ValidationCancelled:
			return ValidationCancelled
		case r.Outcome == ValidationPass:
		case r.Outcome.CodeFailure():
			out = ValidationFail
		default: // INFRA_ERROR, EXECUTION_FAILURE, NOT_CONFIGURED, unknown
			infra = true
		}
	}
	if infra {
		return ValidationInfraError
	}
	return out
}

// AcceptanceOutcome is the result of evaluating a unit's acceptance
// checks (WBS 16.48).
type AcceptanceOutcome string

// Acceptance outcomes.
const (
	AcceptancePass           AcceptanceOutcome = "PASS"
	AcceptanceFail           AcceptanceOutcome = "FAIL"
	AcceptanceReviewRequired AcceptanceOutcome = "REVIEW_REQUIRED"
)

// Check verdicts.
const (
	VerdictPass       = "PASS"
	VerdictFail       = "FAIL"
	VerdictUnverified = "UNVERIFIED"
)

// CheckVerdict is the verdict of one acceptance check.
type CheckVerdict struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	Verification string `json:"verification"`
	Verdict      string `json:"verdict"`
}

// AcceptanceResult is the evaluation of every check.
type AcceptanceResult struct {
	Outcome  AcceptanceOutcome `json:"outcome"`
	Verdicts []CheckVerdict    `json:"verdicts"`
}

// EvaluateAcceptance evaluates checks by their declared verification type
// (WBS 16.48): COMMAND → the matching command record; TEST → the unit run
// (the named tests run inside the unit's commands) — PASS only when the
// run passed; MANUAL → PASS only with an operator approval (index set).
// Any FAIL → FAIL; else any UNVERIFIED → REVIEW_REQUIRED; else PASS. An
// unapproved manual criterion is never automatically PASS.
func EvaluateAcceptance(checks []wbs.AcceptanceCheckSpec, records []ValidationRecord, approved map[int]bool) AcceptanceResult {
	byKey := map[string]ValidationOutcome{}
	for _, r := range records {
		byKey[CommandKey(wbs.ValidationSpec{Program: r.Program, Args: r.Args})] = r.Outcome
	}
	run := AggregateValidation(records)
	res := AcceptanceResult{Outcome: AcceptancePass, Verdicts: []CheckVerdict{}}
	for i, ch := range checks {
		v := VerdictUnverified
		switch ch.Verification {
		case wbs.VerifyCommand:
			if ch.Command != nil {
				switch o := byKey[CommandKey(*ch.Command)]; {
				case o == ValidationPass:
					v = VerdictPass
				case o.CodeFailure():
					v = VerdictFail
				}
			}
		case wbs.VerifyTest:
			switch run {
			case ValidationPass:
				v = VerdictPass
			case ValidationFail:
				v = VerdictFail
			}
		case wbs.VerifyManual:
			if approved[i] {
				v = VerdictPass
			}
		}
		res.Verdicts = append(res.Verdicts, CheckVerdict{Index: i, Text: ch.Text, Verification: ch.Verification, Verdict: v})
		switch {
		case v == VerdictFail:
			res.Outcome = AcceptanceFail
		case v == VerdictUnverified && res.Outcome == AcceptancePass:
			res.Outcome = AcceptanceReviewRequired
		}
	}
	return res
}

// MissingResults returns the commands (by CommandKey) without a record of
// the wanted outcome — any record when want is "" (WBS 18.202: a required
// command without its result can never yield PASS).
func MissingResults(cmds []wbs.ValidationSpec, records []ValidationRecord, want ValidationOutcome) []string {
	have := map[string]bool{}
	for _, r := range records {
		if want == "" || r.Outcome == want {
			have[CommandKey(wbs.ValidationSpec{Program: r.Program, Args: r.Args})] = true
		}
	}
	var missing []string
	for _, c := range cmds {
		if k := CommandKey(c); !have[k] {
			missing = append(missing, strings.TrimSpace(c.Program+" "+strings.Join(c.Args, " ")))
		}
	}
	return missing
}
