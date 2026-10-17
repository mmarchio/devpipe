package recovery

import (
	"fmt"
	"strings"
)

// Retry answers of the decision table.
const (
	RetryYes           = "Yes"
	RetryNo            = "No"
	RetryNoResend      = "No resend"
	RetryBounded       = "Yes, bounded"
	RetryIfRepeatable  = "Yes if safely repeatable"
	RetryNoSideEffects = "No side effects"
	RetryNoInfra       = "No infra retry"
)

// Review answers.
const (
	ReviewNo        = "No"
	ReviewMaybe     = "Maybe"
	ReviewReconcile = "Yes/reconcile"
)

// DecisionRow is one row of the recovery decision table (WBS 18.211).
// Condition is empty for situations handled by routing rather than a
// recovery condition.
type DecisionRow struct {
	Situation   string
	Condition   Condition
	AutoRetry   string
	AutoResume  string
	HumanReview string
	// RetryPolicy names the resilience policy applying automatically
	// ("" = none).
	RetryPolicy string
}

// DecisionTable is the explicit policy table (WBS 18.211).
func DecisionTable() []DecisionRow {
	return []DecisionRow{
		{Situation: "Worker connection failed before send", Condition: DispatchNotSent, AutoRetry: RetryYes, AutoResume: "Yes", HumanReview: ReviewNo, RetryPolicy: "worker_connect"},
		{Situation: "Delivery confirmed", Condition: InflightDispatch, AutoRetry: RetryNoResend, AutoResume: "Wait", HumanReview: ReviewMaybe},
		{Situation: "Delivery unknown", Condition: DeliveryUnknown, AutoRetry: RetryNo, AutoResume: "No", HumanReview: ReviewReconcile},
		{Situation: "Validation test failed", AutoRetry: RetryNoInfra, AutoResume: "FIX", HumanReview: ReviewNo},
		{Situation: "Validation infrastructure failed", Condition: ValidationInfra, AutoRetry: RetryBounded, AutoResume: "Yes", HumanReview: ReviewMaybe, RetryPolicy: "validation_infra"},
		{Situation: "Required checkpoint failed", Condition: PendingCheckpoint, AutoRetry: RetryIfRepeatable, AutoResume: "No DONE", HumanReview: ReviewMaybe, RetryPolicy: "git_lock"},
		{Situation: "DB unavailable", AutoRetry: RetryNoSideEffects, AutoResume: "After restore", HumanReview: ReviewMaybe, RetryPolicy: "db_tx"},
	}
}

// RetrySafety says when an external operation may be repeated.
type RetrySafety string

const (
	RetryAlways      RetrySafety = "ALWAYS"
	RetryOnlyNotSent RetrySafety = "ONLY_IF_PROVABLY_NOT_PERFORMED"
	RetryNever       RetrySafety = "NEVER_AUTOMATIC"
)

// SideEffect is one row of the side-effect safety matrix (WBS 18.223).
type SideEffect struct {
	Operation    string
	Idempotent   bool
	Observable   bool
	Reconcilable bool
	SafeToRetry  RetrySafety
	Ambiguity    string
	RetryPolicy  string
}

// SideEffectMatrix classifies every external operation (WBS 18.223).
func SideEffectMatrix() []SideEffect {
	return []SideEffect{
		{Operation: "Worker dispatch", Idempotent: false, Observable: true, Reconcilable: true, SafeToRetry: RetryOnlyNotSent,
			Ambiguity: "DELIVERY_UNKNOWN: stop, capture evidence, Phase 12 reconciliation, explicit operator resolution; never resent automatically", RetryPolicy: "worker_connect"},
		{Operation: "Compression call", Idempotent: true, Observable: false, Reconcilable: false, SafeToRetry: RetryAlways,
			Ambiguity: "No external state; a lost response is recomputed or the canonical prompt is used when it fits", RetryPolicy: "compression_call"},
		{Operation: "Validation command", Idempotent: true, Observable: true, Reconcilable: true, SafeToRetry: RetryAlways,
			Ambiguity: "An interrupted run is ABANDONED (never PASS / FAIL) and rerun", RetryPolicy: "validation_infra"},
		{Operation: "Git checkpoint", Idempotent: true, Observable: true, Reconcilable: true, SafeToRetry: RetryOnlyNotSent,
			Ambiguity: "Trailer lookup + parent / tree verification; found → adopt, uncertain → CHECKPOINT_AMBIGUOUS recovery", RetryPolicy: "git_lock"},
		{Operation: "Filesystem materialization", Idempotent: true, Observable: true, Reconcilable: true, SafeToRetry: RetryAlways,
			Ambiguity: "Atomic temp-file + rename: the original or the full replacement exists; derived files never become authority"},
	}
}

// DecisionTableMarkdown renders the decision table (docs are generated
// from code so they cannot drift, WBS 18.211).
func DecisionTableMarkdown() string {
	var b strings.Builder
	b.WriteString("| Condition | Recovery condition | Auto Retry | Auto Resume | Human Review | Retry policy |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range DecisionTable() {
		cond := string(r.Condition)
		if cond == "" {
			cond = "—"
		}
		pol := r.RetryPolicy
		if pol == "" {
			pol = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.Situation, cond, r.AutoRetry, r.AutoResume, r.HumanReview, pol)
	}
	return b.String()
}

// SideEffectMarkdown renders the side-effect safety matrix.
func SideEffectMarkdown() string {
	yn := func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}
	var b strings.Builder
	b.WriteString("| Operation | Idempotent | Observable | Reconcilable | Safe to retry | Ambiguity behavior |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, s := range SideEffectMatrix() {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", s.Operation, yn(s.Idempotent), yn(s.Observable), yn(s.Reconcilable), s.SafeToRetry, s.Ambiguity)
	}
	return b.String()
}

// ConditionsMarkdown renders every recovery condition with its
// classification and allowed operator actions (WBS 18.211).
func ConditionsMarkdown() string {
	acts := func(a []Action) string {
		if len(a) == 0 {
			return "—"
		}
		s := make([]string, len(a))
		for i, x := range a {
			s[i] = "`" + string(x) + "`"
		}
		return strings.Join(s, ", ")
	}
	var b strings.Builder
	b.WriteString("| Condition | Classification | Safe actions | Unsafe actions | Automatic | Human approval |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, c := range Conditions() {
		p := PlanFor(Facts{Condition: c})
		auto := "—"
		if p.RecommendedAutomatic != "" {
			auto = "`" + string(p.RecommendedAutomatic) + "`"
		}
		appr := "no"
		if p.RequiresApproval {
			appr = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n", c, p.Classification, acts(p.SafeActions), acts(p.UnsafeActions), auto, appr)
	}
	return b.String()
}
