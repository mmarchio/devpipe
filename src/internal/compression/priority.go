package compression

import (
	"errors"
	"fmt"

	"overseer/internal/prompt"
)

// ErrCompressionTargetImpossible: the content that must survive verbatim
// (PRESERVE_EXACT components + <preserve> regions) alone exceeds the target,
// so no compression can satisfy it safely (WBS 9.9/9.56). Exact content is
// never truncated to meet a target.
var ErrCompressionTargetImpossible = errors.New("compression: target impossible — exact-preserve content exceeds the target")

// ExactTokens is the estimated size of all verbatim-required content.
func ExactTokens(comps []CompressionComponent) int {
	n := 0
	for _, f := range ExactFragments(comps) {
		n += EstimateTokens(f.Text)
	}
	return n
}

// ComponentTokens is the estimated size of every component's content.
func ComponentTokens(comps []CompressionComponent) int {
	n := 0
	for _, c := range comps {
		n += EstimateTokens(c.Content)
	}
	return n
}

// CheckTargetFeasible returns ErrCompressionTargetImpossible (wrapped with
// the sizes) when the exact content alone exceeds target (T9.9.3).
func CheckTargetFeasible(comps []CompressionComponent, target int) error {
	if exact := ExactTokens(comps); exact > target {
		return fmt.Errorf("%w (exact %d tokens > target %d)", ErrCompressionTargetImpossible, exact, target)
	}
	return nil
}

// reductionRank is the WBS 9.9 reduction order: OPTIONAL (1) is reduced
// first, then COMPRESSIBLE (2), then PRESERVE_SEMANTICS (3); PRESERVE_EXACT
// is never reduced (0 = excluded).
func reductionRank(p prompt.CompressionPolicy) int {
	switch p {
	case prompt.PolicyOptional:
		return 1
	case prompt.PolicyCompressible:
		return 2
	case prompt.PolicyPreserveSemantics:
		return 3
	default:
		return 0
	}
}

// ReductionOrder returns the ordinals of the reducible components in the
// deterministic WBS 9.9 order (by policy rank, then canonical order).
// PRESERVE_EXACT components never appear (T9.9.2).
func ReductionOrder(comps []CompressionComponent) []int {
	var out []int
	for rank := 1; rank <= 3; rank++ {
		for _, c := range comps {
			if reductionRank(c.Policy) == rank {
				out = append(out, c.Ordinal)
			}
		}
	}
	return out
}

// DroppedComponent records an OPTIONAL component removed before compression
// (T9.8.3).
type DroppedComponent struct {
	Ordinal       int                        `json:"ordinal"`
	Type          prompt.PromptComponentType `json:"type"`
	SourceID      string                     `json:"source_id"`
	SourceVersion int                        `json:"source_version"`
	Tokens        int                        `json:"tokens"`
}

// DropOptional deterministically removes OPTIONAL components, last canonical
// position first, only while the total component size exceeds target (WBS
// 9.8). A component carrying a <preserve> region is never dropped, and no
// non-OPTIONAL component is ever dropped (T9.8.2). The input is not mutated.
func DropOptional(comps []CompressionComponent, target int) (kept []CompressionComponent, dropped []DroppedComponent) {
	total := ComponentTokens(comps)
	drop := map[int]bool{}
	for i := len(comps) - 1; i >= 0 && total > target; i-- {
		c := comps[i]
		if c.Policy != prompt.PolicyOptional || len(PreserveRegionsOf(c)) > 0 || HasPreserveTag(c.Content) {
			continue
		}
		t := EstimateTokens(c.Content)
		drop[c.Ordinal] = true
		total -= t
		dropped = append(dropped, DroppedComponent{
			Ordinal: c.Ordinal, Type: c.Type, SourceID: c.SourceID.String(),
			SourceVersion: c.SourceVersion, Tokens: t,
		})
	}
	for _, c := range comps {
		if !drop[c.Ordinal] {
			kept = append(kept, c)
		}
	}
	// dropped was collected last-first; report in canonical order.
	for i, j := 0, len(dropped)-1; i < j; i, j = i+1, j-1 {
		dropped[i], dropped[j] = dropped[j], dropped[i]
	}
	return kept, dropped
}
