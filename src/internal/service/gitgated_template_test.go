package service

import (
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/state"
)

// The git-gated workflow (v5) is the v4 workflow with the QA tests first:
// a new, requeued or replanned task goes to QA_TESTS, whose check opens
// the coder's sandbox, and a tampered sandbox goes back to the coder.
func TestGitGatedTemplateTopology(t *testing.T) {
	states := map[string]stateSpec{}
	for _, s := range gitGatedStateSpecs() {
		if _, dup := states[s.key]; dup {
			t.Fatalf("duplicate state %s", s.key)
		}
		states[s.key] = s
	}
	for _, s := range defaultStateSpecs() {
		if _, ok := states[s.key]; !ok {
			t.Errorf("v4 state %s missing", s.key)
		}
	}
	if s := states["QA_TESTS"]; s.kind != state.KindAction || s.template != "CMD_QA_TESTS" {
		t.Fatalf("QA_TESTS %+v", s)
	}
	if a := execution.ResolveAction(state.StateSnapshot{Key: "QA_TESTS_CHECK", Kind: string(state.KindControl)}); a != execution.ActionQATestsCheck {
		t.Fatalf("QA_TESTS_CHECK resolves to %s", a)
	}
	edges := map[string]string{}
	for _, tr := range gitGatedTransitionSpecs() {
		if _, ok := states[tr.from]; !ok {
			t.Errorf("edge from unknown state %s", tr.from)
		}
		if _, ok := states[tr.to]; !ok {
			t.Errorf("edge to unknown state %s", tr.to)
		}
		k := tr.from + "/" + tr.outcome
		if _, dup := edges[k]; dup {
			t.Errorf("duplicate edge %s", k)
		}
		edges[k] = tr.to
	}
	for k, want := range map[string]string{
		"ACCEPTANCE_CHECK/NEXT": "QA_TESTS", "REPLAN_CHECK/REPLANNED": "QA_TESTS", "QA_TESTS/SUBMIT": "QA_TESTS_CHECK",
		"QA_TESTS_CHECK/READY": "IMPLEMENT", "QA_TESTS_CHECK/RETRY": "QA_TESTS", "VALIDATE/TAMPERED": "IMPLEMENT",
		"VALIDATE/FAIL": "IMPLEMENT", "VALIDATE/PASS": "ACCEPTANCE_CHECK", "IMPLEMENT_CHECK/REPLIED": "VALIDATE",
	} {
		if edges[k] != want {
			t.Errorf("%s → %q, want %s", k, edges[k], want)
		}
	}
	// The v4 workflow is unchanged.
	for _, tr := range defaultTransitionSpecs() {
		if tr.to == "QA_TESTS" || tr.outcome == "TAMPERED" {
			t.Fatalf("v4 gained a git-gated edge: %+v", tr)
		}
	}
	if DefaultStateRoles["QA_TESTS"][0] != "TESTER" || ReportKind("QA_TESTS") == "" {
		t.Fatal("QA_TESTS has no TESTER / result kind")
	}
}

func TestRenderMCPGuideGitGated(t *testing.T) {
	var m state.StateMachineSnapshot
	for _, s := range gitGatedStateSpecs() {
		m.States = append(m.States, state.StateSnapshot{Key: s.key, Roles: DefaultStateRoles[s.key]})
	}
	got := RenderMCPGuide(m, []string{"ENGINEER", "TESTER"}, nil)
	for _, want := range []string{"| write the tests for task <KEY> | TESTER |", "`task/<KEY>`", "`sandbox/<KEY>`",
		"fast-forwarded into `main`", "| implement task <KEY> / continue |"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if !strings.Contains(mcpInstructions("tok", "QA_TESTS", true), "Write the task's specification tests only") {
		t.Fatal("QA_TESTS command carries no instructions")
	}
}
