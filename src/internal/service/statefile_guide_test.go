package service

import (
	"strings"
	"testing"

	"overseer/internal/role"
	"overseer/internal/state"
)

func TestRenderRoleFile(t *testing.T) {
	got := renderRoleFile(role.RoleSnapshot{RoleKey: "TESTER", RoleVersion: 3,
		SystemPrompt:  role.PromptSnapshot{Content: "You are the TESTER.\n"},
		Instructions:  []role.InstructionSnapshot{{Key: "TDD", Name: "Test-driven development", Content: "Tests first."}, {Key: "GATE", Content: "Run the gate."}},
		AllowedStates: []string{"TEST", "PHASE_VALIDATE"}})
	for _, want := range []string{"# TESTER\n", "role TESTER v3", "statefiles/HERMES.md", "## ROLE\n\nYou are the TESTER.\n",
		"## TEST-DRIVEN DEVELOPMENT\n\nTests first.\n", "## GATE\n\nRun the gate.\n", "## WORKFLOW STATES\n\nPHASE_VALIDATE, TEST\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if strings.Index(got, "Tests first.") > strings.Index(got, "Run the gate.") {
		t.Fatal("instructions out of order")
	}
}

func TestRenderHermesGuide(t *testing.T) {
	m := state.StateMachineSnapshot{States: []state.StateSnapshot{{Key: "PLAN", Roles: []string{"ARCHITECT", "PLANNER"}},
		{Key: "TEST", Roles: []string{"TESTER"}}}}
	got := RenderHermesGuide(m, []string{"ARCHITECT", "PLANNER", "TESTER"}, map[string]string{"PLAN": "plan phase <N>", "TEST": "verify <KEY>"})
	for _, want := range []string{"`statefiles/ARCHITECT.md`, `statefiles/PLANNER.md`, `statefiles/TESTER.md`",
		"| `plan phase <N>` | ARCHITECT, then PLANNER |", "| `verify <KEY>` | TESTER |", "## How the overseer decides", "## Return format",
		"never parsed", "Status: TODO | ACTIVE | COMPLETE | DONE", "- [ ] [MANUAL] <criterion>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}
