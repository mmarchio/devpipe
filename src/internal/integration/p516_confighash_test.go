package integration

import (
	"testing"

	"overseer/internal/role"
)

func baseSnapshot() *role.RoleSnapshot {
	return &role.RoleSnapshot{
		RoleKey:     "ENGINEER",
		RoleVersion: 1,
		SystemPrompt: role.PromptSnapshot{
			Version:        3,
			Content:        "system prompt body",
			ContentHash:    role.HashSystemPromptContent("system prompt body"),
			Classification: role.ClassificationSystem,
		},
		Instructions: []role.InstructionSnapshot{
			{Key: "CODE", Version: 1, Content: "write code", ContentHash: role.HashSystemPromptContent("write code"), Classification: role.ClassificationRoleInstruction},
			{Key: "TEST", Version: 1, Content: "write tests", ContentHash: role.HashSystemPromptContent("write tests"), Classification: role.ClassificationRoleInstruction},
		},
		AllowedStates: []string{"implementation", "continuation"},
		StateFiles:    []string{"ACTIVE_TASK", "PROJECT_STATE"},
	}
}

// T5.16.1 — Identical configuration produces identical hash.
func TestP516IdenticalConfigIdenticalHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	if a.ConfigurationHash() != b.ConfigurationHash() {
		t.Fatalf("identical configs hash differently: %q vs %q", a.ConfigurationHash(), b.ConfigurationHash())
	}
}

// T5.16.2 — System prompt edit changes hash.
func TestP516SystemPromptEditChangesHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	b.SystemPrompt.Content = "edited system prompt"
	b.SystemPrompt.ContentHash = role.HashSystemPromptContent(b.SystemPrompt.Content)
	b.SystemPrompt.Version = 4
	if a.ConfigurationHash() == b.ConfigurationHash() {
		t.Fatal("system prompt edit did not change the configuration hash")
	}
}

// T5.16.3 — Instruction edit changes hash.
func TestP516InstructionEditChangesHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	b.Instructions[0].Content = "edited instruction"
	b.Instructions[0].ContentHash = role.HashSystemPromptContent(b.Instructions[0].Content)
	if a.ConfigurationHash() == b.ConfigurationHash() {
		t.Fatal("instruction edit did not change the configuration hash")
	}
}

// T5.16.4 — Instruction ordering change changes hash.
func TestP516InstructionOrderChangeChangesHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	b.Instructions[0], b.Instructions[1] = b.Instructions[1], b.Instructions[0]
	if a.ConfigurationHash() == b.ConfigurationHash() {
		t.Fatal("instruction reordering did not change the configuration hash (order must matter)")
	}
}

// T5.16.5 — Allowed-state change changes hash.
func TestP516AllowedStateChangeChangesHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	b.AllowedStates = []string{"implementation", "review"}
	if a.ConfigurationHash() == b.ConfigurationHash() {
		t.Fatal("allowed-state change did not change the configuration hash")
	}
}

// T5.16.6 — State-file requirement change changes hash.
func TestP516StateFileChangeChangesHash(t *testing.T) {
	a := baseSnapshot()
	b := baseSnapshot()
	b.StateFiles = []string{"ACTIVE_TASK", "TEST_STATE"}
	if a.ConfigurationHash() == b.ConfigurationHash() {
		t.Fatal("state-file change did not change the configuration hash")
	}
}
