package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

// InitializeProjectRoles instantiates a project's role configuration from the
// GLOBAL defaults (WBS 5.3): every global role is copied into the project
// scope with a fresh, independent ID and its current system-prompt content
// captured as the project role's v1 prompt. It is idempotent — a re-run (or a
// partial prior run) creates no duplicates (T5.3.6) — and transactional
// (T5.3.5): when a tx pool is wired, one commit covers the whole
// initialization, and a mid-run failure rolls back everything. Once copied,
// the project role is independent: later global edits never mutate it
// (T5.3.4).
func (s *RoleService) InitializeProjectRoles(ctx context.Context, projectID string) ([]*persistence.RoleRecord, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	globals, err := s.roles.ListByScope(ctx, nil)
	if err != nil {
		return nil, err
	}
	if s.txPool == nil {
		var out []*persistence.RoleRecord
		for _, g := range globals {
			rec, err := s.initializeOne(ctx, nil, parsed, g)
			if err != nil {
				return out, fmt.Errorf("init role %s: %w", g.Key, err)
			}
			if rec != nil {
				out = append(out, rec)
			}
		}
		return out, nil
	}
	var created []*persistence.RoleRecord
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		created = nil // the closure re-runs on a retried transaction
		for _, g := range globals {
			rec, err := s.initializeOne(ctx, tx, parsed, g)
			if err != nil {
				return fmt.Errorf("init role %s: %w", g.Key, err)
			}
			if rec != nil {
				created = append(created, rec)
			}
		}
		return nil
	})
	if err != nil {
		return created, err
	}
	return created, nil
}

// initializeOne copies one global role into the project scope. It returns
// (nil, nil) when the key is already present (idempotent skip — T5.3.6), and
// the created record otherwise. When tx is non-nil all writes join it; when
// nil they run sequentially.
func (s *RoleService) initializeOne(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID, g *persistence.RoleRecord) (*persistence.RoleRecord, error) {
	roles := s.roles
	prompts := s.prompts
	if tx != nil {
		roles = s.roles.WithTx(tx)
		prompts = s.prompts.WithTx(tx)
	}
	// Idempotent guard: skip when the key already exists in this project scope.
	if s.projectRoleExists(ctx, roles, projectID, g.Key) {
		return nil, nil
	}
	// Capture the global role's CURRENT prompt content as v1 for the project
	// copy (T5.3.2).
	content, err := s.currentPromptContent(ctx, prompts, g.ID.String())
	if err != nil {
		return nil, err
	}
	parsed, _ := role.NewRole(role.RoleInput{Key: g.Key, Name: g.Name, Description: g.Description})
	rec := &persistence.RoleRecord{
		ID:          parsed.ID,
		ProjectID:   &projectID,
		Key:         parsed.Key,
		Name:        parsed.Name,
		Description: parsed.Description,
		Enabled:     parsed.Enabled,
		Version:     parsed.Version,
		CreatedAt:   parsed.CreatedAt,
		UpdatedAt:   parsed.UpdatedAt,
	}
	if err := roles.Create(ctx, rec); err != nil {
		return nil, err
	}
	if _, err := prompts.AppendVersion(ctx, rec.ID.String(), content); err != nil {
		return nil, err
	}
	return rec, nil
}

// projectRoleExists reports whether the key is already present in the given
// project scope.
func (s *RoleService) projectRoleExists(ctx context.Context, roles persistence.RoleRepository, projectID uuid.UUID, key string) bool {
	out, err := roles.ListByScope(ctx, &projectID)
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

// currentPromptContent returns the role's current system-prompt content (""
// when it has none).
func (s *RoleService) currentPromptContent(ctx context.Context, prompts persistence.SystemPromptRepository, roleID string) (string, error) {
	v, err := prompts.GetCurrentVersion(ctx, roleID)
	if err != nil {
		return "", err
	}
	if v == 0 {
		return "", nil
	}
	rec, err := prompts.GetVersion(ctx, roleID, v)
	if err != nil {
		return "", err
	}
	return rec.Content, nil
}
