// Package retention classifies stored artifacts and decides garbage
// collection eligibility (Phase 18, WBS 18.98–18.102). Execution-critical
// or referenced artifacts are never eligible; audit has its own policy.
package retention

import "time"

// Class is a retention class (WBS 18.98).
type Class string

const (
	ExecutionCritical Class = "EXECUTION_CRITICAL"
	Audit             Class = "AUDIT"
	Cache             Class = "CACHE"
	Ephemeral         Class = "EPHEMERAL"
)

// Kind is a stored artifact kind.
type Kind string

const (
	ConfigurationSnapshot Kind = "CONFIGURATION_SNAPSHOT"
	CanonicalPrompt       Kind = "CANONICAL_PROMPT"
	WorkerPrompt          Kind = "WORKER_PROMPT"
	WorkerResult          Kind = "WORKER_RESULT"
	ValidationResult      Kind = "VALIDATION_RESULT"
	ValidationRawLog      Kind = "VALIDATION_RAW_LOG"
	Checkpoint            Kind = "CHECKPOINT"
	ExecutionEvent        Kind = "EXECUTION_EVENT"
	AuditEvent            Kind = "AUDIT_EVENT"
	PromptCacheEntry      Kind = "PROMPT_CACHE_ENTRY"
	PrecomputeArtifact    Kind = "PRECOMPUTE_ARTIFACT"
	SessionToken          Kind = "SESSION"
)

var classes = map[Kind]Class{
	ConfigurationSnapshot: ExecutionCritical,
	CanonicalPrompt:       ExecutionCritical,
	WorkerPrompt:          ExecutionCritical,
	WorkerResult:          ExecutionCritical,
	ValidationResult:      ExecutionCritical,
	Checkpoint:            ExecutionCritical,
	ExecutionEvent:        ExecutionCritical,
	AuditEvent:            Audit,
	PromptCacheEntry:      Cache,
	PrecomputeArtifact:    Cache,
	ValidationRawLog:      Ephemeral,
	SessionToken:          Ephemeral,
}

// ClassOf returns an artifact kind's class; unknown kinds are treated as
// execution-critical (never collected).
func ClassOf(k Kind) Class {
	if c, ok := classes[k]; ok {
		return c
	}
	return ExecutionCritical
}

// Kinds lists every known kind.
func Kinds() []Kind {
	out := make([]Kind, 0, len(classes))
	for k := range classes {
		out = append(out, k)
	}
	return out
}

// Policy is the configured retention per class (0 = keep forever).
type Policy struct {
	Cache     time.Duration
	Ephemeral time.Duration
	// Audit is independent of every other class (WBS 18.102).
	Audit time.Duration
}

// DefaultPolicy keeps audit forever, cache 30 days, raw logs 90 days.
func DefaultPolicy() Policy {
	return Policy{Cache: 30 * 24 * time.Hour, Ephemeral: 90 * 24 * time.Hour, Audit: 0}
}

// Eligible reports whether an artifact may be collected: never
// execution-critical, never referenced, only past its class retention.
func Eligible(k Kind, age time.Duration, referenced bool, p Policy) bool {
	if referenced {
		return false
	}
	var keep time.Duration
	switch ClassOf(k) {
	case Cache:
		keep = p.Cache
	case Ephemeral:
		keep = p.Ephemeral
	case Audit:
		keep = p.Audit
	default:
		return false
	}
	return keep > 0 && age > keep
}
