package compression

import (
	"errors"
	"fmt"

	"overseer/internal/prompt"
)

// Action is the compression decision for one canonical prompt.
type Action string

const (
	ActionBypass   Action = "BYPASS"
	ActionCompress Action = "COMPRESS"
)

// Decision is the decision plus its recorded reason (T9.30.4).
type Decision struct {
	Action Action
	Reason string
}

// Preparation errors.
var (
	// ErrCanonicalExceedsContext: compression is disabled (NEVER) and the
	// canonical prompt cannot fit the worker context — rejected rather than
	// truncated (T9.31.4).
	ErrCanonicalExceedsContext = errors.New("compression: canonical prompt exceeds the worker context and compression is disabled")
	// ErrPromptPreparationFailed: compression failed and the canonical
	// prompt cannot safely fit the worker context (WBS 9.29).
	ErrPromptPreparationFailed = errors.New("compression: prompt preparation failed — compression failed and the canonical prompt does not fit the worker context")
)

// Decide applies the forced-compression mode (WBS 9.31) and the size policy
// (WBS 9.30): AUTO compresses only when the canonical estimate exceeds the
// target (the Phase 8 size verdict); ALWAYS always compresses; NEVER always
// bypasses but rejects a canonical prompt that cannot fit the context.
func Decide(s Settings, canonicalTokens int) (Decision, error) {
	target := s.EffectiveTarget()
	switch s.Mode {
	case ModeNever:
		if canonicalTokens > s.ContextWindow {
			return Decision{}, fmt.Errorf("%w (%d > %d tokens)", ErrCanonicalExceedsContext, canonicalTokens, s.ContextWindow)
		}
		return Decision{Action: ActionBypass, Reason: "compression disabled (mode NEVER)"}, nil
	case ModeAlways:
		return Decision{Action: ActionCompress, Reason: "compression forced (mode ALWAYS)"}, nil
	}
	verdict, _ := prompt.ValidatePromptSize(canonicalTokens, "", target, s.ContextWindow)
	switch verdict {
	case prompt.SizeValid:
		return Decision{Action: ActionBypass, Reason: fmt.Sprintf("compression not required: canonical %d tokens <= target %d", canonicalTokens, target)}, nil
	case prompt.SizeCompressionRequired:
		return Decision{Action: ActionCompress, Reason: fmt.Sprintf("compression required: canonical %d tokens > target %d", canonicalTokens, target)}, nil
	default:
		return Decision{Action: ActionCompress, Reason: fmt.Sprintf("compression required: canonical %d tokens >= context window %d", canonicalTokens, s.ContextWindow)}, nil
	}
}

// FallbackDecision is the explicit fallback after compression failed or
// was ineffective (WBS 9.29): the canonical prompt is used when it safely
// fits the worker context; otherwise preparation fails. Invalid compressed
// output is never an option.
func FallbackDecision(canonicalTokens, contextWindow int, why string) (RunOutcome, string, error) {
	if canonicalTokens <= contextWindow {
		return RunFallbackCanonical, fmt.Sprintf("%s; canonical %d tokens fits the worker context %d", why, canonicalTokens, contextWindow), nil
	}
	reason := fmt.Sprintf("%s; canonical %d tokens exceeds the worker context %d", why, canonicalTokens, contextWindow)
	return RunFailed, reason, fmt.Errorf("%w: %s", ErrPromptPreparationFailed, reason)
}
