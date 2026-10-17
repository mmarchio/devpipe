package state

import "testing"

// P6-18 (WBS 6.21): deterministic machine configuration hash — unit half
// over the pure domain function, DB-free.

func p618BaseDef() *MachineDefinition {
	return &MachineDefinition{
		Name:        "m",
		Description: "demo machine",
		Version:     1,
		Enabled:     true,
		InitialKey:  "A",
		States: []StateConfigDef{
			{
				Key: "A", Name: "A", PromptTemplate: "tpl-a", Kind: "ACTION",
				Enabled: true, InstructionVersion: 2, InstructionHash: "h-a-v2",
				Roles: []string{"ENGINEER", "TESTER"}, StateFiles: []string{"REPORT"},
			},
			{
				Key: "B", Name: "B", Kind: "TERMINAL", Enabled: true,
				Roles: []string{"PM"},
			},
		},
		Transitions: []TransitionConfigDef{
			{From: "A", To: "B", Outcome: "COMPLETE", Priority: 1, Enabled: true},
		},
	}
}

// T6.21.1 — Same machine (unmodified) produces the same hash on repeated
// calls.
func TestP618SameMachineSameHash(t *testing.T) {
	def := p618BaseDef()
	h1 := MachineConfigurationHash(def)
	h2 := MachineConfigurationHash(def)
	if h1 == "" {
		t.Fatal("hash is empty")
	}
	if h1 != h2 {
		t.Fatalf("repeated hashes differ: %s vs %s", h1, h2)
	}
}

// T6.21.2 — A state-instruction edit changes the hash.
func TestP618StateInstructionEditChangesHash(t *testing.T) {
	def := p618BaseDef()
	before := MachineConfigurationHash(def)
	def.States[0].InstructionVersion = 3
	def.States[0].InstructionHash = "h-a-v3"
	after := MachineConfigurationHash(def)
	if before == after {
		t.Fatalf("hash unchanged after state-instruction edit: %s", before)
	}
}

// T6.21.3 — A transition edit changes the hash.
func TestP618TransitionEditChangesHash(t *testing.T) {
	def := p618BaseDef()
	before := MachineConfigurationHash(def)
	def.Transitions[0].To = "B"
	def.Transitions[0].Outcome = "FAIL"
	after := MachineConfigurationHash(def)
	if before == after {
		t.Fatalf("hash unchanged after transition edit: %s", before)
	}
}

// T6.21.4 — A role-assignment change changes the hash.
func TestP618RoleAssignmentChangeChangesHash(t *testing.T) {
	def := p618BaseDef()
	before := MachineConfigurationHash(def)
	def.States[0].Roles = []string{"ENGINEER"}
	after := MachineConfigurationHash(def)
	if before == after {
		t.Fatalf("hash unchanged after role-assignment change: %s", before)
	}
}

// T6.21.5 — Transition ordering alone (semantic order unchanged) does not
// alter the hash: the same states and transitions supplied in a
// different input order hash identically.
func TestP618SemanticOrderStability(t *testing.T) {
	def := p618BaseDef()
	reordered := &MachineDefinition{
		Name: def.Name, Description: def.Description, Version: def.Version,
		Enabled: def.Enabled, InitialKey: def.InitialKey,
		States: []StateConfigDef{
			def.States[1], def.States[0], // state input order flipped
		},
		Transitions: []TransitionConfigDef{
			{From: "B", To: "A", Outcome: "RESET", Priority: 2, Enabled: true},
			{From: "A", To: "B", Outcome: "COMPLETE", Priority: 1, Enabled: true},
		},
	}
	// Same content as def plus the extra transition, for both; the
	// reordered copy carries them in a different input order.
	def.Transitions = append(def.Transitions, stateTransitionReset())
	h2 := MachineConfigurationHash(def)
	h3 := MachineConfigurationHash(reordered)
	if h2 != h3 {
		t.Fatalf("hash changed under semantic-order permutation: %s vs %s", h2, h3)
	}
	h0 := MachineConfigurationHash(&MachineDefinition{
		Name: def.Name, Description: def.Description, Version: def.Version,
		Enabled: def.Enabled, InitialKey: def.InitialKey,
		States: def.States, Transitions: def.Transitions[:1],
	})
	if h2 == h0 {
		t.Fatal("test is vacuous: the extra transition did not change the hash")
	}
}

func stateTransitionReset() TransitionConfigDef {
	return TransitionConfigDef{From: "B", To: "A", Outcome: "RESET", Priority: 2, Enabled: true}
}
