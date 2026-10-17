package compression

import "strings"

// Phase 11 (PLAN Decision 5): precomputed prompts may carry a placeholder
// for a value that is not known yet (the projected unit's ACTIVE_TASK). A
// placeholder is delimited by these markers; it may exist inside a pending
// precomputed template but NEVER in a worker prompt.
const (
	PlaceholderMarker = "⟦OVERSEER:PENDING:"
	PlaceholderEnd    = "⟧"
)

// ContainsPlaceholder reports whether text carries a placeholder marker.
func ContainsPlaceholder(text string) bool { return strings.Contains(text, PlaceholderMarker) }

// CodePlaceholder is the structural failure code for a placeholder found
// in content that must be final.
const CodePlaceholder = "PLACEHOLDER"
