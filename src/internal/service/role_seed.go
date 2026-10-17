package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

// RoleService is the application-layer entry point for role configuration
// (Phase 5). This task provides only the idempotent default-role seed
// (WBS 5.2); CRUD/overrides/reset land in later tasks.
type RoleService struct {
	roles               persistence.RoleRepository
	prompts             persistence.SystemPromptRepository
	instructions        persistence.InstructionRepository
	assignments         persistence.RoleInstructionAssignmentRepository
	instructionVersions persistence.InstructionVersionRepository
	stateFiles          persistence.RoleStateFileRepository
	allowedStates       persistence.RoleAllowedStateRepository
	txPool              *pgxpool.Pool
}

// WithTxPool wires the transaction pool used for multi-write operations
// (WBS 5.3 project initialization). When nil, those operations run
// sequentially.
func (s *RoleService) WithTxPool(pool *pgxpool.Pool) *RoleService {
	s.txPool = pool
	return s
}

// WithStateFiles wires the state-file requirement repository (P5-11) for the
// RoleSnapshot read model (P5-14).
func (s *RoleService) WithStateFiles(repo persistence.RoleStateFileRepository) *RoleService {
	s.stateFiles = repo
	return s
}

// WithAllowedStates wires the allowed-state repository (P5-10) for the
// RoleSnapshot read model (P5-14).
func (s *RoleService) WithAllowedStates(repo persistence.RoleAllowedStateRepository) *RoleService {
	s.allowedStates = repo
	return s
}

// NewRoleService wires the role service to the role and system-prompt
// repositories.
func NewRoleService(
	roles persistence.RoleRepository,
	prompts persistence.SystemPromptRepository,
) *RoleService {
	return &RoleService{roles: roles, prompts: prompts}
}

// WithInstructions wires the instruction repository (P5-08).
func (s *RoleService) WithInstructions(instructions persistence.InstructionRepository) *RoleService {
	s.instructions = instructions
	return s
}

// WithAssignments wires the role → instruction assignment repository (P5-08).
func (s *RoleService) WithAssignments(assignments persistence.RoleInstructionAssignmentRepository) *RoleService {
	s.assignments = assignments
	return s
}

// WithInstructionVersions wires the instruction version repository (P5-07),
// used to resolve the current instruction version (P5-09).
func (s *RoleService) WithInstructionVersions(v persistence.InstructionVersionRepository) *RoleService {
	s.instructionVersions = v
	return s
}

// AssignInstruction links an instruction to a role (WBS 5.9), rejecting a
// cross-project assignment (T5.9.6): the instruction must be in the same
// scope as the role (both global, or the same project).
func (s *RoleService) AssignInstruction(ctx context.Context, roleID, instructionID string) error {
	if s.assignments == nil {
		return ErrInfrastructure
	}
	if s.instructions == nil {
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
	if _, err := s.assignments.Assign(ctx, role.ID, inst.ID); err != nil {
		return err
	}
	return nil
}

// Enable marks the role enabled so it can be selected for new execution
// (WBS 5.13).
func (s *RoleService) Enable(ctx context.Context, roleID string) error {
	_, err := s.roles.SetEnabled(ctx, parseRoleID(roleID), true)
	return err
}

// Disable marks the role disabled — it can no longer be selected for new
// execution, but its prompt history is retained (WBS 5.13, T5.13.3).
func (s *RoleService) Disable(ctx context.Context, roleID string) error {
	_, err := s.roles.SetEnabled(ctx, parseRoleID(roleID), false)
	return err
}

// IsEligible reports whether the role may be selected for new execution:
// the role must exist and be enabled (WBS 5.13, T5.13.1/T5.13.2).
func (s *RoleService) IsEligible(ctx context.Context, roleID string) (bool, error) {
	rec, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return false, err
	}
	return rec.Enabled, nil
}

func parseRoleID(id string) uuid.UUID {
	if u, err := uuid.Parse(id); err == nil {
		return u
	}
	return uuid.Nil
}

// sameScope reports whether two nullable project IDs denote the same scope.
func sameScope(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// SeedDefaultRoles seeds the five GLOBAL default roles as data (WBS 5.2):
// for each default, the global roles row and its v1 system prompt are
// created when absent. It is idempotent — re-running (or a service restart)
// creates no duplicates (T5.2.6/T5.2.7) — by checking for the existing
// global key before creating.
func (s *RoleService) SeedDefaultRoles(ctx context.Context) error {
	for _, def := range role.DefaultRoles() {
		if err := s.seedOne(ctx, def); err != nil {
			return fmt.Errorf("seed role %s: %w", def.Key, err)
		}
	}
	return nil
}

// seedOne creates one global default role + its v1 system prompt if the
// role key is not already present in the global scope (idempotent —
// T5.2.6/T5.2.7).
func (s *RoleService) seedOne(ctx context.Context, def role.DefaultRole) error {
	if s.existsGlobal(ctx, def.Key) {
		return nil
	}
	r, err := role.NewRole(role.RoleInput{Key: def.Key, Name: def.Name, Description: def.Description})
	if err != nil {
		return err
	}
	rec := &persistence.RoleRecord{
		ID:          r.ID,
		Key:         r.Key,
		Name:        r.Name,
		Description: r.Description,
		Enabled:     r.Enabled,
		Version:     r.Version,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
	if err := s.roles.Create(ctx, rec); err != nil {
		return err
	}
	// v1 system prompt.
	if _, err := s.prompts.AppendVersion(ctx, rec.ID.String(), def.SystemPrompt); err != nil {
		return err
	}
	return nil
}

// existsGlobal reports whether a global role with the given
// (already-normalized) key is present.
func (s *RoleService) existsGlobal(ctx context.Context, key string) bool {
	out, err := s.roles.ListByScope(ctx, nil)
	if err != nil {
		return false
	}
	for _, r := range out {
		if r.Key == key {
			return true
		}
	}
	return false
}
