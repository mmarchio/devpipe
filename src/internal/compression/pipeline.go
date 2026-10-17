package compression

import (
	"context"
	"errors"
	"time"
)

// Pipeline is the compression validation pipeline (WBS 9.25):
//
//	Structural → Exact → Acceptance → Tests → Identifiers → Schema →
//	Constraints → Semantic → Size/Savings → ACCEPTED
//
// Every inexpensive deterministic stage runs and all its failures are
// recorded; the semantic and size stages run only when every deterministic
// stage passed. A semantic validator error or timeout is a failure, never a
// pass; semantic confidence can never override a deterministic failure.
type Pipeline struct {
	// Semantic validators run in order (nil entries skipped).
	Semantic []SemanticValidator
	// SemanticTimeout bounds each semantic validator (0 = no extra bound).
	SemanticTimeout time.Duration
}

type deterministicStage struct {
	stage Stage
	run   func(ValidationInput) []Failure
}

var deterministicStages = []deterministicStage{
	{StageStructural, ValidateStructure},
	{StageExact, ValidateExact},
	{StageAcceptance, ValidateAcceptanceCriteria},
	{StageTests, ValidateRequiredTests},
	{StageIdentifiers, ValidateIdentifiers},
	{StageSchema, ValidateResponseSchema},
	{StageConstraints, ValidateConstraints},
}

// Validate runs the pipeline over one compressed output.
func (p Pipeline) Validate(ctx context.Context, in ValidationInput) ValidationResult {
	res := ValidationResult{Canonical: in.CanonicalTokens, Output: EstimateTokens(in.Compressed)}
	for _, st := range deterministicStages {
		res.Stages = append(res.Stages, st.stage)
		res.Failures = append(res.Failures, st.run(in)...)
		if st.stage == StageStructural && len(res.Failures) > 0 && res.Failures[0].Code == CodeEmpty {
			break
		}
	}
	res.Items = itemResults(in, res.Failures)
	if len(res.Failures) > 0 {
		res.Status = ValidationFailed
		return res
	}

	res.Stages = append(res.Stages, StageSemantic)
	canonical := in.Canonical
	if canonical == nil {
		canonical = in.Components
	}
	for _, v := range p.Semantic {
		if v == nil {
			continue
		}
		sctx, cancel := ctx, context.CancelFunc(func() {})
		if p.SemanticTimeout > 0 {
			sctx, cancel = context.WithTimeout(ctx, p.SemanticTimeout)
		}
		sr, err := v.Validate(sctx, canonical, in.Compressed)
		cancel()
		if err != nil {
			code := CodeSemanticError
			if errors.Is(err, context.DeadlineExceeded) {
				code = CodeSemanticTimeout
			}
			res.Failures = append(res.Failures, Failure{Stage: StageSemantic, Code: code, Detail: err.Error()})
			break
		}
		cp := sr
		res.Semantic = &cp
		for _, m := range sr.MissingRequirements {
			res.Failures = append(res.Failures, Failure{Stage: StageSemantic, Code: CodeMissing, Detail: m})
		}
		for _, m := range sr.ChangedRequirements {
			res.Failures = append(res.Failures, Failure{Stage: StageSemantic, Code: CodeChanged, Detail: m})
		}
		for _, m := range sr.AddedRequirements {
			res.Failures = append(res.Failures, Failure{Stage: StageSemantic, Code: CodeAdded, Detail: m})
		}
		if !sr.Passed && len(sr.MissingRequirements)+len(sr.ChangedRequirements)+len(sr.AddedRequirements) == 0 {
			res.Failures = append(res.Failures, Failure{Stage: StageSemantic, Code: CodeChanged, Detail: "semantic validator rejected the output: " + sr.Summary})
		}
		if len(res.Failures) > 0 {
			break
		}
	}
	if len(res.Failures) > 0 {
		res.Status = ValidationFailed
		return res
	}

	res.Stages = append(res.Stages, StageSize)
	res.Savings = ClassifySavings(in.CanonicalTokens, res.Output, in.MinimumSavings)
	switch {
	case res.Savings == SavingsExpanded:
		res.Failures = append(res.Failures, Failure{Stage: StageSize, Code: CodeExpanded, Detail: "compressed output is not smaller than the canonical prompt"})
	case res.Savings == SavingsIneffective:
		res.Failures = append(res.Failures, Failure{Stage: StageSize, Code: CodeIneffective, Detail: "savings below the configured minimum"})
	}
	if in.TargetTokens > 0 && res.Output > in.TargetTokens && in.CanonicalTokens > in.TargetTokens {
		res.Failures = append(res.Failures, Failure{Stage: StageSize, Code: CodeOverTarget, Detail: "compressed output exceeds the target"})
	}
	if len(res.Failures) > 0 {
		res.Status = ValidationIneffective
		return res
	}
	res.Status = ValidationPassed
	return res
}
