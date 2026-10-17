package prompt

// WBS 8.29: validate the estimated canonical prompt size against the
// project/model context configuration, classifying ONLY (Phase 8 performs
// no compression and no truncation).

// PromptSizeVerdict is the classification of a prompt's estimated size.
type PromptSizeVerdict string

const (
	// SizeValid: the estimated total is within the compression target.
	SizeValid PromptSizeVerdict = "VALID"

	// SizeCompressionRequired: the estimated total is over the configured
	// target but still below the hard limit - a later compression pass
	// (Phase 9) is required.
	SizeCompressionRequired PromptSizeVerdict = "COMPRESSION_REQUIRED"

	// SizeTooLargeToCompressSafely: the estimated total has reached the
	// hard limit (the model context window) - compression can no longer be
	// safe.
	SizeTooLargeToCompressSafely PromptSizeVerdict = "TOO_LARGE_TO_COMPRESS_SAFELY"
)

// ValidatePromptSize classifies the prompt's estimated total token count
// (P8-32) against the configured compression target and hard limit (WBS
// 8.29). The values are supplied by the caller from persisted project
// settings: hardLimit is the model context window (Settings.ContextWindow)
// and compressionTarget is the configured target derived from it (the
// ContextWindow * ContextThreshold product - computed by the caller so the
// threshold->target policy stays on the configuration surface).
//
// Boundary policy (stated per ACTIVE_TASK NOTES): total <= target is VALID;
// target < total < hardLimit is COMPRESSION_REQUIRED; total >= hardLimit is
// TOO_LARGE_TO_COMPRESS_SAFELY. Exactly-at-target is VALID; exactly-at-limit
// is TOO_LARGE (no compression headroom remains).
//
// The content is taken read-only and returned byte-identical: the validator
// classifies and never mutates (T8.29.4 - no silent truncation). Pure and
// deterministic: the same input always yields the same verdict.
func ValidatePromptSize(estimatedTotal int, content string, compressionTarget, hardLimit int) (PromptSizeVerdict, string) {
	if estimatedTotal >= hardLimit {
		return SizeTooLargeToCompressSafely, content
	}
	if estimatedTotal > compressionTarget {
		return SizeCompressionRequired, content
	}
	return SizeValid, content
}
