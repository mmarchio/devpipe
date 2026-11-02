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

func TestRenderMCPGuide(t *testing.T) {
	m := state.StateMachineSnapshot{States: []state.StateSnapshot{{Key: "PLAN", Roles: []string{"ARCHITECT", "PLANNER"}},
		{Key: "TEST", Roles: []string{"TESTER"}}}}
	got := RenderMCPGuide(m, []string{"ARCHITECT", "PLANNER", "TESTER"}, map[string]string{"PLAN": "plan phase <N>", "TEST": "verify <KEY>"})
	for _, want := range []string{"`statefiles/ARCHITECT.md`, `statefiles/PLANNER.md`, `statefiles/TESTER.md`",
		"| `plan phase <N>` | ARCHITECT, then PLANNER |", "| `verify <KEY>` | TESTER |", "## MCP tools", "submit_result",
		"Do not read or write files under `statefiles/` to record workflow state."} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	for _, gone := range []string{"BACKLOG.md", "ACTIVE_TASK.md", "TASK_ARCHIVE.md", "PLAN_REJECTED.md"} {
		if strings.Contains(got, gone) {
			t.Fatalf("the guide still names %s:\n%s", gone, got)
		}
	}
}

// The v4 workflow's guide has no test or acceptance command: the overseer
// tests and accepts every task, and MANUAL criteria are not allowed.
func TestRenderMCPGuideAutomatedAcceptance(t *testing.T) {
	var m state.StateMachineSnapshot
	for _, s := range defaultStateSpecs() {
		m.States = append(m.States, state.StateSnapshot{Key: s.key})
	}
	got := RenderMCPGuide(m, []string{"ENGINEER"}, nil)
	for _, want := range []string{"| implement task <KEY> / continue |", "The overseer tests and accepts every task itself",
		"a plan may not declare MANUAL criteria"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	for _, gone := range []string{"test task <KEY>", "acceptance tasks", "criteria_met"} {
		if strings.Contains(got, gone) {
			t.Fatalf("the v4 guide still names %q:\n%s", gone, got)
		}
	}
	m.States = nil
	for _, s := range statefileV3StateSpecs() {
		m.States = append(m.States, state.StateSnapshot{Key: s.key})
	}
	if got := RenderMCPGuide(m, nil, nil); !strings.Contains(got, "test task <KEY>") || !strings.Contains(got, "acceptance tasks") {
		t.Fatalf("the v3 guide lost its commands:\n%s", got)
	}
}

// mcpInstructions offers MANUAL criteria only where a tester judges them.
func TestMCPInstructionsVerifications(t *testing.T) {
	for _, key := range []string{"PLAN", "PLAN_AMENDMENT"} {
		if got := mcpInstructions("tok", key, true); strings.Contains(got, "MANUAL") || !strings.Contains(got, "verification: COMMAND|TEST,") {
			t.Fatalf("%s automated: %s", key, got)
		}
		if got := mcpInstructions("tok", key, false); !strings.Contains(got, "verification: COMMAND|TEST|MANUAL,") {
			t.Fatalf("%s: %s", key, got)
		}
	}
}
