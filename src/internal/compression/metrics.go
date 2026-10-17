package compression

import (
	"time"

	"overseer/internal/prompt"
)

// EstimateTokens is the token estimate used for every compression policy
// decision: the Phase 8 size-based estimator (bytes/4) applied to a whole
// text, so canonical, compressed and worker prompts are measured alike.
// Provider-reported usage is recorded metadata only.
func EstimateTokens(s string) int { return prompt.EstimateContentTokens(s) }

// Metrics is the compression measurement of one canonical/compressed pair
// (WBS 9.15).
type Metrics struct {
	CanonicalTokens  int
	CompressedTokens int

	// TokensSaved is canonical - compressed; negative on expansion.
	TokensSaved int
	// Ratio is compressed / canonical (0 when canonical is 0).
	Ratio float64
	// PercentSaved is TokensSaved / canonical * 100 (0 when canonical is 0);
	// negative on expansion.
	PercentSaved float64

	Duration time.Duration
	Attempts int
}

// ComputeMetrics computes the WBS 9.15 metrics. A zero-token canonical
// prompt yields zero ratio/percentage (T9.15.3); an expansion yields
// negative savings (T9.15.4) — never a false success.
func ComputeMetrics(canonicalTokens, compressedTokens int, d time.Duration, attempts int) Metrics {
	m := Metrics{
		CanonicalTokens:  canonicalTokens,
		CompressedTokens: compressedTokens,
		TokensSaved:      canonicalTokens - compressedTokens,
		Duration:         d,
		Attempts:         attempts,
	}
	if canonicalTokens > 0 {
		m.Ratio = float64(compressedTokens) / float64(canonicalTokens)
		m.PercentSaved = float64(m.TokensSaved) / float64(canonicalTokens) * 100
	}
	return m
}

// SavingsClass classifies a compression result against the minimum-savings
// policy (WBS 9.16/9.55).
type SavingsClass string

const (
	SavingsEffective   SavingsClass = "EFFECTIVE"
	SavingsIneffective SavingsClass = "INEFFECTIVE"
	SavingsExpanded    SavingsClass = "EXPANDED"
)

// ClassifySavings: compressed >= canonical (including the zero-canonical
// edge) is EXPANDED (no savings); a fractional reduction below
// minimumSavings is INEFFECTIVE; otherwise EFFECTIVE. Pure: the inputs are
// counts, so the canonical prompt can never be affected (T9.16.4).
func ClassifySavings(canonicalTokens, compressedTokens int, minimumSavings float64) SavingsClass {
	if canonicalTokens <= 0 || compressedTokens >= canonicalTokens {
		return SavingsExpanded
	}
	saved := float64(canonicalTokens-compressedTokens) / float64(canonicalTokens)
	if saved < minimumSavings {
		return SavingsIneffective
	}
	return SavingsEffective
}
