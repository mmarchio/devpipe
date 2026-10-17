package prompt

// P8-33 - WBS 8.29 prompt size validation, TDD T8.29.1-T8.29.4.
//
// Classification policy (stated per ACTIVE_TASK NOTES so TESTER/PM can
// verify): the validator compares the prompt's estimated total token count
// (P8-32 TokenEstimate.Total) against two values supplied by the caller
// from persisted project settings (ContextWindow / ContextThreshold):
//
//   hardLimit           = the model context window (ContextWindow, int tokens)
//   compressionTarget   = the compression target (ContextWindow *
//   ContextThreshold, computed by the CALLER - this function takes the
//   already-computed target so the threshold->target policy stays with the
//   configuration surface)
//
// Boundaries (chosen, and pinned below):
//
//   total <= compressionTarget            -> SizeValid
//   compressionTarget < total < hardLimit -> SizeCompressionRequired
//   total >= hardLimit                    -> SizeTooLargeToCompressSafely
//
// "At exactly the target" is VALID (the threshold is the lower inclusive
// bound, mirroring the P6-11 context-threshold resolver's semantics).
// "At exactly the hard limit" is TOO_LARGE_TO_COMPRESS_SAFELY (no room
// left for compression to be safe - the prompt already fills the window).
//
// The validator classifies ONLY (Phase 8, WBS 8.29): it takes the prompt
// content read-only and returns it byte-identical - no truncation, no
// mutation (T8.29.4).

import (
	"strings"
	"testing"
)

// T8.29.1 - small prompt classified valid.
func TestT8291SmallPromptValid(t *testing.T) {
	content := "# Objective\nsmall prompt"
	total := EstimateTokens(Components{{Type: ComponentTaskContext, Content: content}}).Total
	v, out := ValidatePromptSize(total, content, 1000, 10000)
	if v != SizeValid {
		t.Fatalf("want %s, got %s (total=%d target=1000 limit=10000)", SizeValid, v, total)
	}
	if out != content {
		t.Fatalf("content mutated by validation")
	}
	// Exactly at the compression target is still VALID (inclusive boundary).
	if v2, _ := ValidatePromptSize(1000, content, 1000, 10000); v2 != SizeValid {
		t.Fatalf("want %s at exactly-target, got %s", SizeValid, v2)
	}
}

// T8.29.2 - prompt over configured target classified compression-required.
func TestT8292OverTargetCompressionRequired(t *testing.T) {
	content := strings.Repeat("x", 8000) // 2000 tokens by the ceil(bytes/4) method
	v, _ := ValidatePromptSize(2000, content, 1000, 10000)
	if v != SizeCompressionRequired {
		t.Fatalf("want %s, got %s (total=2000 target=1000 limit=10000)", SizeCompressionRequired, v)
	}
	// One token above the target is already compression-required.
	if v2, _ := ValidatePromptSize(1001, content, 1000, 10000); v2 != SizeCompressionRequired {
		t.Fatalf("want %s at target+1, got %s", SizeCompressionRequired, v2)
	}
}

// T8.29.3 - hard-limit violation identified.
func TestT8293HardLimitViolation(t *testing.T) {
	content := "big"
	v, _ := ValidatePromptSize(10000, content, 1000, 10000)
	if v != SizeTooLargeToCompressSafely {
		t.Fatalf("want %s, got %s (total=10000 target=1000 limit=10000)", SizeTooLargeToCompressSafely, v)
	}
	// Exactly at the hard limit is TOO_LARGE (no compression headroom left).
	if v2, _ := ValidatePromptSize(9999, content, 1000, 10000); v2 != SizeCompressionRequired {
		t.Fatalf("want %s just under the limit, got %s", SizeCompressionRequired, v2)
	}
}

// T8.29.4 - content not mutated during validation.
func TestT8294ContentUnmutated(t *testing.T) {
	content := "# Objective\n- a\n- b\n# Acceptance Criteria\n- T8.29.4\n"
	for _, tc := range []struct {
		name   string
		total  int
		target int
		limit  int
		want   PromptSizeVerdict
	}{
		{"valid", 10, 100, 1000, SizeValid},
		{"compression", 200, 100, 1000, SizeCompressionRequired},
		{"too large", 1000, 100, 1000, SizeTooLargeToCompressSafely},
	} {
		got, out := ValidatePromptSize(tc.total, content, tc.target, tc.limit)
		if got != tc.want {
			t.Fatalf("%s: want %s, got %s", tc.name, tc.want, got)
		}
		if out != content {
			t.Fatalf("%s: content not returned byte-identical", tc.name)
		}
	}
}
