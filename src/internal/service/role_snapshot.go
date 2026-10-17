package service

import (
	"context"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

// SnapshotRole builds the immutable RoleSnapshot read model for a role (WBS
// 5.14/5.29): the role, its exact current system-prompt version, its assigned
// instructions in position order (each at its current version, with hashes),
// its allowed states, and its state-file requirements. It is deterministic for
// an unchanged configuration (T5.14.5) and tags every component with its
// source classification (WBS 5.29): SYSTEM for the prompt, ROLE_INSTRUCTION
// for each instruction (T5.29.1/T5.29.2). When a tx pool is wired the whole
// read happens in a single transaction (WBS 5.14 "one transaction").
func (s *RoleService) SnapshotRole(ctx context.Context, roleID string) (*role.RoleSnapshot, error) {
	r, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return nil, err
	}
	if s.txPool == nil {
		return s.assembleSnapshot(ctx, nil, r)
	}
	var out *role.RoleSnapshot
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var aerr error
		out, aerr = s.assembleSnapshot(ctx, tx, r)
		return aerr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// assembleSnapshot composes the read model from the role and its associated
// data. When tx is non-nil every read joins it (WBS 5.14 one-transaction read);
// when nil the reads run sequentially.
func (s *RoleService) assembleSnapshot(ctx context.Context, tx persistence.Transaction, r *persistence.RoleRecord) (*role.RoleSnapshot, error) {
	snap := &role.RoleSnapshot{
		RoleID:      r.ID,
		RoleKey:     r.Key,
		RoleVersion: r.Version,
	}
	// System prompt at its current version (T5.14.2), classified SYSTEM
	// (T5.29.1).
	if s.prompts != nil {
		promptRepo := s.prompts
		if tx != nil {
			promptRepo = s.prompts.WithTx(tx)
		}
		if cur, err := promptRepo.GetCurrentVersion(ctx, r.ID.String()); err == nil && cur > 0 {
			if v, err := promptRepo.GetVersion(ctx, r.ID.String(), cur); err == nil {
				snap.SystemPrompt = role.PromptSnapshot{
					Version:        v.Version,
					Content:        v.Content,
					ContentHash:    v.ContentHash,
					Classification: role.ClassificationSystem,
				}
			}
		}
	}
	// Assigned instructions in position order, each at its current version,
	// classified ROLE_INSTRUCTION (T5.14.3, T5.29.2).
	if s.assignments != nil && s.instructions != nil {
		assignRepo := s.assignments
		instrRepo := s.instructions
		if tx != nil {
			assignRepo = s.assignments.WithTx(tx)
			instrRepo = s.instructions.WithTx(tx)
		}
		assignments, err := assignRepo.ListByRole(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range assignments {
			inst, err := instrRepo.Get(ctx, a.InstructionID.String())
			if err != nil {
				return nil, err
			}
			if !inst.Enabled {
				continue
			}
			content, version := s.currentInstruction(ctx, a.InstructionID.String(), inst)
			snap.Instructions = append(snap.Instructions, role.InstructionSnapshot{
				Key:            inst.Key,
				Name:           inst.Name,
				Version:        version,
				Content:        content,
				ContentHash:    role.HashSystemPromptContent(content),
				Classification: role.ClassificationRoleInstruction,
			})
		}
	}
	// Allowed states (WBS 5.11).
	if s.allowedStates != nil {
		stRepo := s.allowedStates
		if tx != nil {
			stRepo = s.allowedStates.WithTx(tx)
		}
		states, err := stRepo.ListByRole(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range states {
			snap.AllowedStates = append(snap.AllowedStates, st.StateKey)
		}
	}
	// State-file requirements (WBS 5.12).
	if s.stateFiles != nil {
		sfRepo := s.stateFiles
		if tx != nil {
			sfRepo = s.stateFiles.WithTx(tx)
		}
		files, err := sfRepo.ListByRole(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			snap.StateFiles = append(snap.StateFiles, f.StateFileKey)
		}
	}
	return snap, nil
}
