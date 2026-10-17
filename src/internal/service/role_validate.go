package service

import (
	"context"
	"strings"
)

// RoleValidationProblem is one validation failure for a role (WBS 5.19).
type RoleValidationProblem struct {
	Field   string
	Message string
}

// ValidateRole validates a role before it can participate in execution
// (WBS 5.19), returning ALL problems found (not just the first): enabled,
// system prompt exists + non-empty, all assigned instructions resolve, and at
// least one allowed state (T5.19.1–T5.19.5). An empty slice means the role is
// valid.
func (s *RoleService) ValidateRole(ctx context.Context, roleID string) []RoleValidationProblem {
	var problems []RoleValidationProblem
	r, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return append(problems, RoleValidationProblem{Field: "role", Message: "role not found"})
	}
	// enabled.
	if !r.Enabled {
		problems = append(problems, RoleValidationProblem{Field: "enabled", Message: "role is disabled"})
	}
	// system prompt exists + non-empty.
	hasPrompt := false
	if s.prompts != nil {
		if cur, err := s.prompts.GetCurrentVersion(ctx, r.ID.String()); err == nil && cur > 0 {
			if v, err := s.prompts.GetVersion(ctx, r.ID.String(), cur); err == nil && strings.TrimSpace(v.Content) != "" {
				hasPrompt = true
			}
		}
	}
	if !hasPrompt {
		problems = append(problems, RoleValidationProblem{Field: "system_prompt", Message: "system prompt missing or empty"})
	}
	// all assigned instructions resolve.
	if s.assignments != nil && s.instructions != nil {
		if assignments, err := s.assignments.ListByRole(ctx, r.ID); err == nil {
			for _, a := range assignments {
				inst, err := s.instructions.Get(ctx, a.InstructionID.String())
				if err != nil {
					problems = append(problems, RoleValidationProblem{
						Field:   "instructions",
						Message: "assigned instruction " + a.InstructionID.String() + " does not resolve",
					})
					continue
				}
				if !inst.Enabled {
					problems = append(problems, RoleValidationProblem{
						Field:   "instructions",
						Message: "assigned instruction " + inst.Key + " is disabled",
					})
				}
			}
		}
	}
	// >=1 allowed state.
	if s.allowedStates != nil {
		if states, err := s.allowedStates.ListByRole(ctx, r.ID); err == nil && len(states) == 0 {
			problems = append(problems, RoleValidationProblem{Field: "allowed_states", Message: "no allowed state configured"})
		}
	}
	return problems
}
