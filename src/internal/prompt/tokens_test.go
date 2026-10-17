package prompt

// P8-32 - WBS 8.28 prompt token estimation, TDD T8.28.1-T8.28.4.
//
// TokenEstimate is the deterministic, size-based estimate metadata
// (WBS 8.28): one tracked counter per WBS bucket (system,
// role-instruction, state-instruction, state-file, task, continuation,
// response-protocol) plus the total. Exact provider tokenization is out of
// scope (WBS 8.28: provider-specific adapters can be added later), so the
// chosen method (stated per ACTIVE_TASK NOTES so TESTER/PM can verify):
//
//   per-component tokens = ceil(bytes / 4), where bytes is the UTF-8 length
//   of the component content (0 bytes -> 0 tokens).
//
// The bucket mapping from the nine PromptComponentType values to the WBS
// tracked buckets is:
//
//   SYSTEM                -> SystemTokens
//   ROLE_INSTRUCTION      -> RoleInstructionTokens
//   STATE_INSTRUCTION     -> StateInstructionTokens
//   PROJECT_CONTEXT,
//   PHASE_CONTEXT         -> StateFileTokens   (state-file context)
//   TASK_CONTEXT,
//   TEST_CONTEXT          -> TaskTokens
//   CONTINUATION_CONTEXT  -> ContinuationTokens
//   RESPONSE_PROTOCOL     -> ResponseProtocolTokens
//
// Total = the sum of the buckets. The estimate is monotonic in content
// size: growing any component's content cannot decrease any bucket or the
// total (T8.28.3). It is deterministic: identical components produce a
// byte-identical TokenEstimate (DeepEqual, repeated computation).
//
// Persistability (T8.28.4): TokenEstimate is a plain exported value type
// with no functions/pointers/slices - storable as-is in a record the
// P8-30/P8-38 persistence path writes (CanonicalPrompt.EstimatedTokens
// rides on Total). The JSON round-trip test stands in for storage:
// marshal + unmarshal is byte-identical.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func teFixture() Components {
	return Components{
		{Type: ComponentSystem, Source: SourceSystemDefault, Order: 1, Content: "SYSTEM CONTENT", Policy: PolicyPreserveSemantics},
		{Type: ComponentRoleInstruction, Source: SourceRoleInstruction, Order: 2, Content: "ROLE INSTRUCTION CONTENT", Policy: PolicyPreserveSemantics},
		{Type: ComponentStateInstruction, Source: SourceStateInstruction, Order: 3, Content: "STATE INSTRUCTION CONTENT", Policy: PolicyPreserveSemantics},
		{Type: ComponentProjectContext, Source: SourceStateFile, Order: 4, Content: "PROJECT CONTEXT CONTENT", Policy: PolicyCompressible},
		{Type: ComponentPhaseContext, Source: SourceStateFile, Order: 5, Content: "PHASE CONTEXT CONTENT", Policy: PolicyCompressible},
		{Type: ComponentTaskContext, Source: SourceWorkUnit, Order: 6, Content: "TASK CONTEXT CONTENT", Policy: PolicyPreserveExact},
		{Type: ComponentTestContext, Source: SourceWorkUnit, Order: 7, Content: "TEST CONTEXT CONTENT", Policy: PolicyPreserveExact},
		{Type: ComponentContinuationContext, Source: SourceContinuation, Order: 8, Content: "CONTINUATION CONTENT", Policy: PolicyPreserveSemantics},
		{Type: ComponentResponseProtocol, Source: SourceResponseProtocol, Order: 9, Content: "RESPONSE PROTOCOL CONTENT", Policy: PolicyPreserveSemantics},
	}
}

// ceiltok is the test-side mirror of the stated estimation method:
// ceil(bytes/4) for the component content.
func ceiltok(s string) int {
	return (len(s) + 3) / 4
}

// T8.28.1 - Total estimate generated. The total is the sum of the tracked
// buckets and is positive for a non-empty component set.
func TestT8281_TotalEstimateGenerated(t *testing.T) {
	est := EstimateTokens(teFixture())
	want := ceiltok("SYSTEM CONTENT") +
		ceiltok("ROLE INSTRUCTION CONTENT") +
		ceiltok("STATE INSTRUCTION CONTENT") +
		ceiltok("PROJECT CONTEXT CONTENT") + ceiltok("PHASE CONTEXT CONTENT") +
		ceiltok("TASK CONTEXT CONTENT") + ceiltok("TEST CONTEXT CONTENT") +
		ceiltok("CONTINUATION CONTENT") + ceiltok("RESPONSE PROTOCOL CONTENT")
	if est.Total != want {
		t.Fatalf("total = %d, want %d", est.Total, want)
	}
	if est.Total <= 0 {
		t.Fatalf("total = %d, want > 0", est.Total)
	}
}

// T8.28.2 - Component estimates available. Each tracked bucket carries the
// exact per-component sum for its type(s) (the stated method, bucket by
// bucket), and an empty content contributes 0.
func TestT8282_ComponentEstimatesAvailable(t *testing.T) {
	est := EstimateTokens(teFixture())
	if est.SystemTokens != ceiltok("SYSTEM CONTENT") {
		t.Fatalf("system = %d, want %d", est.SystemTokens, ceiltok("SYSTEM CONTENT"))
	}
	if est.RoleInstructionTokens != ceiltok("ROLE INSTRUCTION CONTENT") {
		t.Fatalf("role-instruction = %d, want %d", est.RoleInstructionTokens, ceiltok("ROLE INSTRUCTION CONTENT"))
	}
	if est.StateInstructionTokens != ceiltok("STATE INSTRUCTION CONTENT") {
		t.Fatalf("state-instruction = %d, want %d", est.StateInstructionTokens, ceiltok("STATE INSTRUCTION CONTENT"))
	}
	wantStateFile := ceiltok("PROJECT CONTEXT CONTENT") + ceiltok("PHASE CONTEXT CONTENT")
	if est.StateFileTokens != wantStateFile {
		t.Fatalf("state-file = %d, want %d", est.StateFileTokens, wantStateFile)
	}
	wantTask := ceiltok("TASK CONTEXT CONTENT") + ceiltok("TEST CONTEXT CONTENT")
	if est.TaskTokens != wantTask {
		t.Fatalf("task = %d, want %d", est.TaskTokens, wantTask)
	}
	if est.ContinuationTokens != ceiltok("CONTINUATION CONTENT") {
		t.Fatalf("continuation = %d, want %d", est.ContinuationTokens, ceiltok("CONTINUATION CONTENT"))
	}
	if est.ResponseProtocolTokens != ceiltok("RESPONSE PROTOCOL CONTENT") {
		t.Fatalf("response-protocol = %d, want %d", est.ResponseProtocolTokens, ceiltok("RESPONSE PROTOCOL CONTENT"))
	}
	// Empty content contributes 0 to its bucket.
	empty := Components{{Type: ComponentSystem, Content: ""}}
	if got := EstimateTokens(empty).SystemTokens; got != 0 {
		t.Fatalf("empty system content = %d tokens, want 0", got)
	}
}

// T8.28.3 - Larger prompt produces larger estimate appropriately. The
// estimate is monotonic in content size: a strict growth of one component's
// content strictly grows its bucket and the total; adding components grows
// the total; nothing ever decreases.
func TestT8283_MonotonicInContentSize(t *testing.T) {
	base := teFixture()
	baseEst := EstimateTokens(base)

	grown := base
	grown[0] = base[0]
	grown[0].Content = base[0].Content + " MORE SYSTEM CONTENT"
	grownEst := EstimateTokens(grown)
	if grownEst.SystemTokens <= baseEst.SystemTokens {
		t.Fatalf("grown system bucket = %d, want > %d", grownEst.SystemTokens, baseEst.SystemTokens)
	}
	if grownEst.Total <= baseEst.Total {
		t.Fatalf("grown total = %d, want > %d", grownEst.Total, baseEst.Total)
	}

	bigger := base
	bigger = append(bigger, PromptComponent{Type: ComponentTaskContext, Content: "EXTRA WORK UNIT CONTEXT"})
	if EstimateTokens(bigger).Total <= baseEst.Total {
		t.Fatalf("adding a component did not grow the total")
	}

	// Monotone non-decreasing sweep: doubling each content step by step.
	prev := EstimateTokens(Components{{Type: ComponentTaskContext, Content: "a"}})
	for _, n := range []int{2, 4, 8, 16, 64} {
		content := make([]byte, n)
		for i := range content {
			content[i] = 'x'
		}
		est := EstimateTokens(Components{{Type: ComponentTaskContext, Content: string(content)}})
		if est.TaskTokens < prev.TaskTokens || est.Total < prev.Total {
			t.Fatalf("estimate decreased at size %d: %d < %d", n, est.Total, prev.Total)
		}
		prev = est
	}
}

// T8.28.4 - Estimate metadata persisted. The estimate is a plain persistable
// value type: a JSON round-trip (the storage stand-in) is byte-identical,
// and repeated estimation over identical components is DeepEqual
// (deterministic metadata, same value every time it is stored).
func TestT8284_EstimateMetadataPersistable(t *testing.T) {
	est := EstimateTokens(teFixture())
	data, err := json.Marshal(est)
	if err != nil {
		t.Fatalf("marshal estimate: %v", err)
	}
	var back TokenEstimate
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal estimate: %v", err)
	}
	if !reflect.DeepEqual(back, est) {
		t.Fatalf("round trip not byte-identical: %+v vs %+v", back, est)
	}
	for i := 0; i < 25; i++ {
		if got := EstimateTokens(teFixture()); !reflect.DeepEqual(got, est) {
			t.Fatalf("repeat %d: estimate not deterministic", i)
		}
	}
	// The metadata carries the WBS-tracked fields plus the total.
	var zero TokenEstimate
	if est == zero {
		t.Fatal("estimate is all zeros over a non-empty component set")
	}
}
