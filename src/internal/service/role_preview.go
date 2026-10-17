package service

import (
	"context"

	"overseer/internal/role"
)

// RolePreviewView is the role preview (WBS 5.27): what currently constitutes
// a role — its name, its current system prompt, its instructions in
// assignment order, its allowed states, its state-file requirements, and a
// deterministic configuration hash derived at read time. It is a pure read
// model: no prompt composition, no mutation.
type RolePreviewView struct {
	Key               string
	Name              string
	Version           int
	SystemPrompt      role.PromptSnapshot
	Instructions      []role.InstructionSnapshot
	AllowedStates     []string
	StateFiles        []string
	ConfigurationHash string
}

// Preview assembles the role preview for a role (WBS 5.27) from the
// RoleSnapshot read model (WBS 5.14) plus the deterministic configuration
// hash (WBS 5.16). The hash is derived at read time from the snapshot, never
// persisted. ErrNotFound when the role is missing.
func (s *RoleService) Preview(ctx context.Context, roleID string) (*RolePreviewView, error) {
	rec, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return nil, err
	}
	snap, err := s.SnapshotRole(ctx, roleID)
	if err != nil {
		return nil, err
	}
	states := snap.AllowedStates
	if states == nil {
		states = []string{}
	}
	files := snap.StateFiles
	if files == nil {
		files = []string{}
	}
	instructions := snap.Instructions
	if instructions == nil {
		instructions = []role.InstructionSnapshot{}
	}
	return &RolePreviewView{
		Key:               rec.Key,
		Name:              rec.Name,
		Version:           rec.Version,
		SystemPrompt:      snap.SystemPrompt,
		Instructions:      instructions,
		AllowedStates:     states,
		StateFiles:        files,
		ConfigurationHash: snap.ConfigurationHash(),
	}, nil
}
