package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

// ResolvedInstruction is one resolved instruction for a role (WBS 5.10):
// the instruction's key and its current version's content + hash.
type ResolvedInstruction struct {
	Key         string
	Version     int
	ContentHash string
	Content     string
}

// ResolveRoleInstructions resolves the complete ordered instruction set for
// a role (WBS 5.10): the role's assignments in position order, each expanded
// to its instruction's current version content + hash. Disabled instructions
// are excluded (T5.10.4); the ordering is deterministic (assignment position,
// T5.10.2); version/hash metadata is included (T5.10.5).
func (s *RoleService) ResolveRoleInstructions(ctx context.Context, roleID string) ([]ResolvedInstruction, error) {
	if s.assignments == nil || s.instructions == nil {
		return nil, ErrInfrastructure
	}
	assignments, err := s.assignments.ListByRole(ctx, mustUUID(roleID))
	if err != nil {
		return nil, err
	}
	var out []ResolvedInstruction
	for _, a := range assignments {
		inst, err := s.instructions.Get(ctx, a.InstructionID.String())
		if err != nil {
			return nil, err
		}
		if !inst.Enabled {
			// Disabled instructions are retained in storage but excluded from
			// resolution (WBS 5.7/5.10 T5.10.4).
			continue
		}
		content, version := s.currentInstruction(ctx, a.InstructionID.String(), inst)
		out = append(out, ResolvedInstruction{
			Key:         inst.Key,
			Version:     version,
			ContentHash: role.HashSystemPromptContent(content),
			Content:     content,
		})
	}
	return out, nil
}

// currentInstruction returns the instruction's current version content and
// version number. It prefers the version history (WBS 5.8) so a version
// append is reflected; if no versions exist it falls back to the
// instructions row's stored content/version.
func (s *RoleService) currentInstruction(ctx context.Context, id string, inst *persistence.InstructionRecord) (string, int) {
	if s.instructionVersions != nil {
		if cur, err := s.instructionVersions.GetCurrentVersion(ctx, id); err == nil && cur > 0 {
			if v, err := s.instructionVersions.GetVersion(ctx, id, cur); err == nil {
				return v.Content, v.Version
			}
		}
	}
	return inst.Content, inst.Version
}

// mustUUID parses a UUID string (service-internal helper).
func mustUUID(s string) uuid.UUID {
	u, _ := uuid.Parse(s)
	return u
}
