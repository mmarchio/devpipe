package service

import (
	"context"
)

// RoleInstructionAssignment is the API-facing view of one role → instruction
// assignment (WBS 5.24): the instruction id, its position in the ordered list,
// and the instruction's key.
type RoleInstructionAssignment struct {
	InstructionID  string
	Position       int
	InstructionKey string
}

// ListRoleInstructions returns the role's instruction assignments in position
// order (WBS 5.24 T5.24.3), each expanded to its instruction id + key. A
// missing role is ErrNotFound.
func (s *RoleService) ListRoleInstructions(ctx context.Context, roleID string) ([]RoleInstructionAssignment, error) {
	if s.assignments == nil || s.instructions == nil {
		return nil, ErrInfrastructure
	}
	if _, err := s.roles.Get(ctx, roleID); err != nil {
		return nil, err
	}
	recs, err := s.assignments.ListByRole(ctx, mustUUID(roleID))
	if err != nil {
		return nil, err
	}
	out := make([]RoleInstructionAssignment, 0, len(recs))
	for _, a := range recs {
		inst, err := s.instructions.Get(ctx, a.InstructionID.String())
		if err != nil {
			return nil, err
		}
		out = append(out, RoleInstructionAssignment{
			InstructionID:  a.InstructionID.String(),
			Position:       a.Position,
			InstructionKey: inst.Key,
		})
	}
	return out, nil
}

// UnassignInstruction removes a role → instruction assignment (WBS 5.24
// T5.24.2). A missing role or instruction is ErrNotFound; a cross-project
// instruction is ErrInvalidInput (T5.9.6 parity with the assign path); an
// unassigned (role, instruction) pair is ErrNotFound from the repository.
func (s *RoleService) UnassignInstruction(ctx context.Context, roleID, instructionID string) error {
	if s.assignments == nil || s.instructions == nil {
		return ErrInfrastructure
	}
	role, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return err
	}
	inst, err := s.instructions.Get(ctx, instructionID)
	if err != nil {
		return err
	}
	if !sameScope(role.ProjectID, inst.ProjectID) {
		return ErrInvalidInput
	}
	return s.assignments.Unassign(ctx, role.ID, inst.ID)
}
