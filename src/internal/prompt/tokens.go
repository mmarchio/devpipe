package prompt

// TokenEstimate is the deterministic token-estimate metadata for one
// canonical prompt (WBS 8.28): one counter per tracked WBS bucket plus the
// total. It is a plain value type (no functions, pointers, or slices), so it
// is persistable as-is by the P8-30/P8-38 persistence path (the canonical
// prompt's EstimatedTokens field rides on Total; the full per-bucket
// metadata round-trips through JSON).
type TokenEstimate struct {
	SystemTokens           int
	RoleInstructionTokens  int
	StateInstructionTokens int
	StateFileTokens        int
	TaskTokens             int
	ContinuationTokens     int
	ResponseProtocolTokens int

	Total int
}

// tokensForContent is the stated size-based estimation method (WBS 8.28,
// provider-specific adapters later): ceil(bytes/4) over the component
// content's UTF-8 length. Empty content contributes 0. The division by a
// constant makes the result monotonic non-decreasing in content size
// (T8.28.3).
func tokensForContent(s string) int {
	return (len(s) + 3) / 4
}

// EstimateTokens computes the per-bucket and total token estimate over the
// component set (WBS 8.28). The component-type to WBS-bucket mapping (stated
// per ACTIVE_TASK NOTES): SYSTEM -> SystemTokens; ROLE_INSTRUCTION ->
// RoleInstructionTokens; STATE_INSTRUCTION -> StateInstructionTokens;
// PROJECT_CONTEXT + PHASE_CONTEXT -> StateFileTokens (state-file context);
// TASK_CONTEXT + TEST_CONTEXT -> TaskTokens; CONTINUATION_CONTEXT ->
// ContinuationTokens; RESPONSE_PROTOCOL -> ResponseProtocolTokens.
// Total is the sum of the buckets. Pure and deterministic: identical
// components yield a byte-identical TokenEstimate.
func EstimateTokens(components Components) TokenEstimate {
	var est TokenEstimate
	for _, c := range components {
		toks := tokensForContent(c.Content)
		switch c.Type {
		case ComponentSystem:
			est.SystemTokens += toks
		case ComponentRoleInstruction:
			est.RoleInstructionTokens += toks
		case ComponentStateInstruction:
			est.StateInstructionTokens += toks
		case ComponentProjectContext, ComponentPhaseContext:
			est.StateFileTokens += toks
		case ComponentTaskContext, ComponentTestContext:
			est.TaskTokens += toks
		case ComponentContinuationContext:
			est.ContinuationTokens += toks
		case ComponentResponseProtocol:
			est.ResponseProtocolTokens += toks
		}
	}
	est.Total = est.SystemTokens + est.RoleInstructionTokens +
		est.StateInstructionTokens + est.StateFileTokens + est.TaskTokens +
		est.ContinuationTokens + est.ResponseProtocolTokens
	return est
}

// EstimateContentTokens is the stated size-based estimation method applied to
// one arbitrary text (ceil(bytes/4), WBS 8.28) — exported so later phases
// (Phase 9 compression metrics) estimate compressed/worker prompts with the
// same method as canonical components.
func EstimateContentTokens(s string) int {
	return tokensForContent(s)
}
