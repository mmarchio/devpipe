package statefile

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/role"
)

// P7-20 (WBS 7.35): snapshot token budget — pure-domain half.
//
// SnapshotSize aggregates the snapshot's component token estimates (the
// state-file pins' estimates summed, the role snapshot's estimate included
// where available, and their sum). The DB-persisted half (T7.35.4) rides
// with the integration tests over the real snapshot service.

// T7.35.1 — State-file estimates summed. The StateFileTokens field is the
// sum of the TokenEstimate of every pinned file's content.
func TestSnapshotSizeStateFileEstimatesSummed(t *testing.T) {
	contents := []string{
		strings.Repeat("p", 40), // 10 tokens
		strings.Repeat("h", 12), // 3 tokens
		strings.Repeat("t", 8),  // 2 tokens
	}
	files := make([]StateFileSnapshot, 0, len(contents))
	for i, c := range contents {
		files = append(files, StateFileSnapshot{
			StateFileID: uuid.New(),
			TypeKey:     "T" + string(rune('A'+i)),
			Scope:       ScopeProject,
			Version:     1,
			Content:     c,
		})
	}
	s := ExecutionContextSnapshot{StateFiles: files}
	got := s.Size()
	want := TokenEstimate(contents[0]) + TokenEstimate(contents[1]) + TokenEstimate(contents[2])
	if want != 15 {
		t.Fatalf("fixture sanity: want sum 15, got %d", want)
	}
	if got.StateFileTokens != want {
		t.Errorf("StateFileTokens = %d, want %d (the sum of the estimates)", got.StateFileTokens, want)
	}
}

// T7.35.2 — Role estimate included where available. The RoleTokens field is
// the role snapshot's estimate (system prompt + assigned instructions). A
// snapshot carrying no role content estimates 0 (where NOT available).
func TestSnapshotSizeRoleEstimateIncluded(t *testing.T) {
	prompt := strings.Repeat("p", 40) // 10 tokens
	instr := strings.Repeat("i", 8)   // 2 tokens
	s := ExecutionContextSnapshot{RoleSnapshot: role.RoleSnapshot{
		RoleKey: "ENGINEER",
		SystemPrompt: role.PromptSnapshot{
			Version: 1, Content: prompt, Classification: role.ClassificationSystem,
		},
		Instructions: []role.InstructionSnapshot{{
			Key: "I", Content: instr, Classification: role.ClassificationRoleInstruction,
		}},
	}}
	got := s.Size()
	want := TokenEstimate(prompt) + TokenEstimate(instr)
	if want != 12 {
		t.Fatalf("fixture sanity: want 12, got %d", want)
	}
	if got.RoleTokens != want {
		t.Errorf("RoleTokens = %d, want %d (prompt + instructions where available)", got.RoleTokens, want)
	}

	// Where NOT available (no role content): the estimate contributes 0,
	// never an error and never a guess.
	empty := ExecutionContextSnapshot{}
	if got := empty.Size(); got.RoleTokens != 0 || got.EstimatedTotal != 0 {
		t.Errorf("no-role snapshot Size = %+v, want zero", got)
	}
}

// T7.35.3 — Empty optional files handled. A pin with empty content
// (an optional file with no captured version) estimates 0 and does not
// disturb the aggregate.
func TestSnapshotSizeEmptyOptionalFiles(t *testing.T) {
	s := ExecutionContextSnapshot{StateFiles: []StateFileSnapshot{
		{StateFileID: uuid.New(), TypeKey: "OPT", Scope: ScopeProject, Version: 0, Content: ""},
		{StateFileID: uuid.New(), TypeKey: "PROJECT_STATE", Scope: ScopeProject, Version: 1, Content: strings.Repeat("x", 8)},
	}}
	got := s.Size()
	if got.StateFileTokens != 2 {
		t.Errorf("StateFileTokens = %d, want 2 (the empty file contributes 0)", got.StateFileTokens)
	}
	if got.EstimatedTotal != 2 {
		t.Errorf("EstimatedTotal = %d, want 2", got.EstimatedTotal)
	}
}

// T7.35.x — EstimatedTotal is the aggregate of the two components.
func TestSnapshotSizeEstimatedTotal(t *testing.T) {
	s := ExecutionContextSnapshot{
		StateFiles: []StateFileSnapshot{{TypeKey: "K", Scope: ScopeProject, Version: 1, Content: strings.Repeat("c", 12)}},
		RoleSnapshot: role.RoleSnapshot{
			SystemPrompt: role.PromptSnapshot{Version: 1, Content: strings.Repeat("r", 8)},
		},
	}
	got := s.Size()
	if got.EstimatedTotal != got.StateFileTokens+got.RoleTokens {
		t.Errorf("EstimatedTotal = %d, want StateFileTokens+RoleTokens = %d", got.EstimatedTotal, got.StateFileTokens+got.RoleTokens)
	}
}

// T7.36.1-T7.36.4 (pure half) — the WBS 7.36 classification policy assigns
// the right class to the canonical state-file types: project state ->
// PROJECT_CONTEXT, phase state -> PHASE_CONTEXT, active task ->
// TASK_CONTEXT, test state -> TEST_CONTEXT.
func TestSnapshotFileClassificationPolicy(t *testing.T) {
	cases := []struct {
		key   string
		scope StateFileScope
		want  role.Classification
	}{
		{"PROJECT_STATE", ScopeProject, role.ClassificationProjectContext},
		{"PHASE_STATE", ScopePhase, role.ClassificationPhaseContext},
		{"ACTIVE_TASK", ScopeWorkUnit, role.ClassificationTaskContext},
		{"TEST_STATE", ScopePhase, role.ClassificationTestContext},
	}
	for _, c := range cases {
		if got := ClassifySnapshotFile(c.key, c.scope); got != c.want {
			t.Errorf("ClassifySnapshotFile(%q, %s) = %q, want %q", c.key, c.scope, got, c.want)
		}
	}
}

// T7.36.x (documented task-level choice, pinned here) — a non-canonical
// type key falls back to the scope-based class: PROJECT scope ->
// PROJECT_CONTEXT, PHASE scope -> PHASE_CONTEXT, WORK_UNIT/EXECUTION scope
// -> TASK_CONTEXT.
func TestSnapshotFileClassificationScopeFallback(t *testing.T) {
	cases := []struct {
		key   string
		scope StateFileScope
		want  role.Classification
	}{
		{"ARCHITECTURE", ScopeProject, role.ClassificationProjectContext},
		{"SOME_PHASE_FILE", ScopePhase, role.ClassificationPhaseContext},
		{"SOME_UNIT_FILE", ScopeWorkUnit, role.ClassificationTaskContext},
		{"SOME_EXEC_FILE", ScopeExecution, role.ClassificationTaskContext},
	}
	for _, c := range cases {
		if got := ClassifySnapshotFile(c.key, c.scope); got != c.want {
			t.Errorf("ClassifySnapshotFile(%q, %s) = %q, want %q (scope fallback)", c.key, c.scope, got, c.want)
		}
	}
}
