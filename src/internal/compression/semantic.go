package compression

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"overseer/internal/prompt"
)

// SemanticValidator validates that compressed output kept the meaning of the
// canonical components (WBS 9.23). Implementations: the deterministic
// structured checks (DeterministicSemanticValidator), an LLM-based validator
// (adapter, HYBRID mode), or a mock. The compression model is never its own
// sole judge.
type SemanticValidator interface {
	Validate(ctx context.Context, canonical []CompressionComponent, compressed string) (SemanticValidationResult, error)
}

// SemanticValidationResult (WBS 9.24). Confidence is optional metadata and
// can never override a deterministic failure (T9.24.5).
type SemanticValidationResult struct {
	Passed bool `json:"passed"`

	MissingRequirements []string `json:"missing_requirements,omitempty"`
	ChangedRequirements []string `json:"changed_requirements,omitempty"`
	AddedRequirements   []string `json:"added_requirements,omitempty"`

	Confidence *float64 `json:"confidence,omitempty"`

	Summary string `json:"summary,omitempty"`
}

// ErrSemanticValidation marks a semantic-validator failure (the validator
// itself failed — distinguishable from a compression error, T9.23.3).
var ErrSemanticValidation = errors.New("compression: semantic validation error")

// SemanticValidatorFunc adapts a function to SemanticValidator.
type SemanticValidatorFunc func(ctx context.Context, canonical []CompressionComponent, compressed string) (SemanticValidationResult, error)

// Validate calls f.
func (f SemanticValidatorFunc) Validate(ctx context.Context, canonical []CompressionComponent, compressed string) (SemanticValidationResult, error) {
	return f(ctx, canonical, compressed)
}

// DeterministicSemanticValidator is the deterministic structured semantic
// check (no LLM):
//   - added requirements: an output sentence introducing domain terms that
//     occur nowhere in the canonical prompt (WBS 9.53) — compression may
//     shorten and abbreviate, never introduce new subject matter;
//   - prohibitions in PRESERVE_SEMANTICS components must survive negated:
//     a matching output sentence without negation is a CHANGED (reversed)
//     requirement; no matching sentence is MISSING (WBS 9.6).
type DeterministicSemanticValidator struct{}

// Validate implements SemanticValidator.
func (DeterministicSemanticValidator) Validate(ctx context.Context, canonical []CompressionComponent, compressed string) (SemanticValidationResult, error) {
	if err := ctx.Err(); err != nil {
		return SemanticValidationResult{}, err
	}
	var res SemanticValidationResult
	vocab := vocabulary(canonical)
	for _, sent := range splitSentences(compressed) {
		if novel := novelTerms(sent, vocab); len(novel) > 0 {
			res.AddedRequirements = append(res.AddedRequirements, truncate(sent, 160)+" [new terms: "+strings.Join(novel, ", ")+"]")
		}
	}
	outSentences := splitSentences(compressed)
	for _, c := range componentsWithPolicy(canonical, prompt.PolicyPreserveSemantics) {
		for _, sent := range splitSentences(c.Content) {
			loc := negationRE.FindStringIndex(sent)
			if loc == nil {
				continue
			}
			words := contentWords(sent[loc[1]:])
			if len(words) == 0 {
				continue
			}
			need := (len(words)*6 + 9) / 10
			matched, negated := false, false
			for _, o := range outSentences {
				lo := strings.ToLower(o)
				hit := 0
				for _, w := range words {
					if strings.Contains(lo, w) {
						hit++
					}
				}
				if hit >= need {
					matched = true
					if hasNegation(o) {
						negated = true
						break
					}
				}
			}
			switch {
			case !matched:
				res.MissingRequirements = append(res.MissingRequirements, truncate(sent, 160))
			case !negated:
				res.ChangedRequirements = append(res.ChangedRequirements, "prohibition reversed: "+truncate(sent, 160))
			}
		}
	}
	res.Passed = len(res.AddedRequirements) == 0 && len(res.MissingRequirements) == 0 && len(res.ChangedRequirements) == 0
	if res.Passed {
		res.Summary = "deterministic semantic checks passed"
	} else {
		res.Summary = "deterministic semantic checks failed"
	}
	return res, nil
}

var wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_\-]*`)

// semanticStopwords are connective/telegraphese words a compressor may
// legitimately introduce; they never count as new subject matter.
var semanticStopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`also then when with without into from that this these those each every none only must should shall will would could
	have has been being were what which while where there their they them your yours before after above below under over
	keep kept required require requires requirement requirements note notes summary summarized summarize section sections
	context contexts prompt prompts compressed compression verbatim exact exactly preserve preserved omitted omit optional
	following follows follow per via using use used same other others more most less least many much some any both either
	neither than such just very about across within upon onto ensure ensures include includes included including list lists
	item items etc yes okay true false null none current currently previous previously next first last done item`) {
		semanticStopwords[w] = true
	}
}

func vocabulary(comps []CompressionComponent) map[string]bool {
	v := map[string]bool{}
	for _, c := range comps {
		for _, w := range wordRE.FindAllString(c.Content, -1) {
			v[strings.ToLower(w)] = true
		}
	}
	return v
}

// novelTerms returns the significant words of s absent from the canonical
// vocabulary: not a stopword, not a known word modulo a plural "s", and not
// an abbreviation (prefix) of a known word.
func novelTerms(s string, vocab map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range wordRE.FindAllString(s, -1) {
		lw := strings.ToLower(w)
		if len(lw) < 4 || seen[lw] || semanticStopwords[lw] || vocab[lw] || vocab[strings.TrimSuffix(lw, "s")] || vocab[lw+"s"] {
			continue
		}
		if isPrefixOfAny(lw, vocab) {
			continue
		}
		seen[lw] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func isPrefixOfAny(p string, vocab map[string]bool) bool {
	for w := range vocab {
		if len(w) > len(p) && strings.HasPrefix(w, p) {
			return true
		}
	}
	return false
}

// contentWords returns the lower-cased significant words of s.
func contentWords(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range wordRE.FindAllString(s, -1) {
		lw := strings.ToLower(w)
		if len(lw) < 4 || semanticStopwords[lw] || seen[lw] {
			continue
		}
		seen[lw] = true
		out = append(out, lw)
	}
	return out
}
