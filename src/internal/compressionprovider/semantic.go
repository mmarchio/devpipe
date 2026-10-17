package compressionprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"overseer/internal/compression"
)

// LLMSemanticValidator is the LLM-based compression.SemanticValidator (WBS
// 9.23, HYBRID mode). It asks a configured model for a structured verdict;
// a malformed verdict is a validation ERROR, never a pass. It runs in
// addition to — never instead of — the deterministic checks, and it is a
// separate call from the compression call (the compression model is not its
// own sole judge).
type LLMSemanticValidator struct {
	Client *OpenAICompatible
	Model  string
}

var _ compression.SemanticValidator = (*LLMSemanticValidator)(nil)

const semanticSystem = `You check whether a COMPRESSED development prompt preserves the meaning of the ORIGINAL prompt sections.
Report every requirement, prohibition, dependency or constraint that is missing or changed in the compressed prompt, and every requirement the compressed prompt adds that the original does not contain.
Answer ONLY with JSON: {"passed": bool, "missing_requirements": [string], "changed_requirements": [string], "added_requirements": [string], "confidence": number, "summary": string}`

type verdict struct {
	Passed              *bool    `json:"passed"`
	MissingRequirements []string `json:"missing_requirements"`
	ChangedRequirements []string `json:"changed_requirements"`
	AddedRequirements   []string `json:"added_requirements"`
	Confidence          *float64 `json:"confidence"`
	Summary             string   `json:"summary"`
}

// Validate implements compression.SemanticValidator.
func (v *LLMSemanticValidator) Validate(ctx context.Context, canonical []compression.CompressionComponent, compressed string) (compression.SemanticValidationResult, error) {
	var b strings.Builder
	b.WriteString("ORIGINAL SECTIONS:\n")
	b.WriteString(compression.RenderStructuredInput(canonical).Text)
	b.WriteString("\nCOMPRESSED PROMPT:\n")
	b.WriteString(compressed)
	res, err := v.Client.chat(ctx, v.Model, semanticSystem, b.String())
	if err != nil {
		return compression.SemanticValidationResult{}, fmt.Errorf("%w: %v", compression.ErrSemanticValidation, err)
	}
	text := strings.TrimSpace(res.Content)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	var vd verdict
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &vd); err != nil || vd.Passed == nil {
		return compression.SemanticValidationResult{}, fmt.Errorf("%w: malformed semantic verdict", compression.ErrSemanticValidation)
	}
	out := compression.SemanticValidationResult{
		MissingRequirements: vd.MissingRequirements,
		ChangedRequirements: vd.ChangedRequirements,
		AddedRequirements:   vd.AddedRequirements,
		Confidence:          vd.Confidence,
		Summary:             vd.Summary,
	}
	// A "passed" verdict that lists problems is not a pass.
	out.Passed = *vd.Passed && len(vd.MissingRequirements)+len(vd.ChangedRequirements)+len(vd.AddedRequirements) == 0
	return out, nil
}
