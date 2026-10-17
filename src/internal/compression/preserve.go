package compression

import (
	"strings"

	"overseer/internal/prompt"
)

// User requirement (PLAN P9-10A): content wrapped in <preserve></preserve>
// tags is never compressed, paraphrased, or dropped, in any component and
// regardless of the component's policy.
const (
	PreserveOpenTag  = "<preserve>"
	PreserveCloseTag = "</preserve>"
)

// PreserveRegion is one <preserve> region: the inner text and its byte
// offsets inside the component content (Start/End delimit the inner text,
// excluding the tags).
type PreserveRegion struct {
	Start int
	End   int
	Text  string
}

// ExtractPreserveRegions returns the ordered <preserve> regions of content.
// Tags are matched literally and case-sensitively. Nested tags: the
// outermost region wins (inner tags are part of its text). wellFormed is
// false when a tag is unclosed or a close tag is stray — the caller then
// treats the whole component as PRESERVE_EXACT (fail safe: content is never
// lost because of a malformed tag).
func ExtractPreserveRegions(content string) (regions []PreserveRegion, wellFormed bool) {
	depth := 0
	start := 0
	i := 0
	for i < len(content) {
		switch {
		case strings.HasPrefix(content[i:], PreserveOpenTag):
			if depth == 0 {
				start = i + len(PreserveOpenTag)
			}
			depth++
			i += len(PreserveOpenTag)
		case strings.HasPrefix(content[i:], PreserveCloseTag):
			if depth == 0 {
				return regions, false
			}
			depth--
			if depth == 0 {
				regions = append(regions, PreserveRegion{Start: start, End: i, Text: content[start:i]})
			}
			i += len(PreserveCloseTag)
		default:
			i++
		}
	}
	if depth != 0 {
		return regions, false
	}
	return regions, true
}

// HasPreserveTag reports whether content mentions either preserve tag.
func HasPreserveTag(content string) bool {
	return strings.Contains(content, PreserveOpenTag) || strings.Contains(content, PreserveCloseTag)
}

// policyRank orders policies by strength; raising never lowers.
func policyRank(p prompt.CompressionPolicy) int {
	switch p {
	case prompt.PolicyPreserveExact:
		return 3
	case prompt.PolicyPreserveSemantics:
		return 2
	case prompt.PolicyCompressible:
		return 1
	default:
		return 0
	}
}

// raise returns the stronger of the two policies.
func raise(cur, to prompt.CompressionPolicy) prompt.CompressionPolicy {
	if policyRank(to) > policyRank(cur) {
		return to
	}
	return cur
}

// EffectivePolicy is the policy the compression pass applies to a component
// (WBS 9.5): raise-only over the canonical policy. The structured response
// schema (RESPONSE_PROTOCOL) and the identity-bearing task/test context are
// PRESERVE_EXACT; a component with a malformed <preserve> tag is raised to
// PRESERVE_EXACT as a whole. A canonical policy is never lowered.
func EffectivePolicy(c CompressionComponent) prompt.CompressionPolicy {
	p := c.Policy
	switch c.Type {
	case prompt.ComponentResponseProtocol, prompt.ComponentTaskContext, prompt.ComponentTestContext,
		// The captured continuation status (completed / remaining / blocking)
		// is execution-critical: a compressor must never move an item between
		// lists (WBS 9.51), so it is preserved exactly.
		prompt.ComponentContinuationContext:
		p = raise(p, prompt.PolicyPreserveExact)
	}
	if HasPreserveTag(c.Content) {
		if _, ok := ExtractPreserveRegions(c.Content); !ok {
			p = raise(p, prompt.PolicyPreserveExact)
		}
	}
	return p
}

// ApplyEffectivePolicies returns a copy of comps with each policy replaced
// by its EffectivePolicy. The input is never mutated.
func ApplyEffectivePolicies(comps []CompressionComponent) []CompressionComponent {
	out := copyComponents(comps)
	for i := range out {
		out[i].Policy = EffectivePolicy(out[i])
	}
	return out
}

// PreserveRegionsOf returns the well-formed <preserve> regions of a
// component (none for a malformed component — it is exact as a whole).
func PreserveRegionsOf(c CompressionComponent) []PreserveRegion {
	regs, ok := ExtractPreserveRegions(c.Content)
	if !ok {
		return nil
	}
	return regs
}

// FragmentSource names why a fragment must survive verbatim.
type FragmentSource string

const (
	FragmentComponent FragmentSource = "COMPONENT"
	FragmentPreserve  FragmentSource = "PRESERVE"
)

// ExactFragment is one piece of content that must appear verbatim in the
// compressed output (WBS 9.5/9.17).
type ExactFragment struct {
	ID      string
	Ordinal int
	Type    prompt.PromptComponentType
	Source  FragmentSource
	Text    string
}

// ExactFragments lists every verbatim-required fragment of the (effective
// policy) component set, in component order: the whole content of every
// PRESERVE_EXACT component, and the inner text of every <preserve> region in
// any other component. Empty / whitespace-only fragments are omitted.
func ExactFragments(comps []CompressionComponent) []ExactFragment {
	var out []ExactFragment
	for _, c := range comps {
		if c.Policy == prompt.PolicyPreserveExact {
			if strings.TrimSpace(c.Content) != "" {
				out = append(out, ExactFragment{
					ID:      "EXACT-" + itoa(c.Ordinal),
					Ordinal: c.Ordinal, Type: c.Type, Source: FragmentComponent, Text: c.Content,
				})
			}
			continue
		}
		for j, r := range PreserveRegionsOf(c) {
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
			out = append(out, ExactFragment{
				ID:      "PRESERVE-" + itoa(c.Ordinal) + "." + itoa(j+1),
				Ordinal: c.Ordinal, Type: c.Type, Source: FragmentPreserve, Text: r.Text,
			})
		}
	}
	return out
}
