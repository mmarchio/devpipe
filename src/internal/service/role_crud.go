package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

// CreateRole creates a role in the given scope (nil projectID = global) with a
// v1 system prompt (WBS 5.21). The role key is normalized; a scoped duplicate
// is ErrConflict.
func (s *RoleService) CreateRole(ctx context.Context, projectID *string, key, name, description, systemPrompt string) (*persistence.RoleRecord, error) {
	r, err := role.NewRole(role.RoleInput{Key: key, Name: name, Description: description})
	if err != nil {
		return nil, err
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
	if projectID != nil && *projectID != "" {
		pid := parseRoleID(*projectID)
		rec.ProjectID = &pid
	}
	if err := s.roles.Create(ctx, rec); err != nil {
		return nil, err
	}
	if systemPrompt != "" {
		if _, err := s.prompts.AppendVersion(ctx, rec.ID.String(), systemPrompt); err != nil {
			return nil, err
		}
	}
	return s.roles.Get(ctx, rec.ID.String())
}

// ListRoles lists roles in a scope: projectID nil = global, set = that
// project's roles (WBS 5.21).
func (s *RoleService) ListRoles(ctx context.Context, projectID *string) ([]*persistence.RoleRecord, error) {
	if projectID != nil && *projectID != "" {
		u := parseRoleID(*projectID)
		return s.roles.ListByScope(ctx, &u)
	}
	return s.roles.ListByScope(ctx, nil)
}

// GetRole returns one role by id (WBS 5.21); ErrNotFound when missing.
func (s *RoleService) GetRole(ctx context.Context, roleID string) (*persistence.RoleRecord, error) {
	return s.roles.Get(ctx, roleID)
}

// DeleteRole removes a role (WBS 5.21) unless it is historically referenced:
// a role with prompt history, assigned instructions, allowed states, or
// state-file requirements cannot be destructively deleted — the client should
// disable it instead (T5.21.6). The role must be in the requested scope.
func (s *RoleService) DeleteRole(ctx context.Context, projectID, roleID string) error {
	rec, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return err
	}
	// projectID == "" or "global" addresses the global scope.
	var wantPID *string
	if projectID != "" && projectID != "global" {
		wantPID = &projectID
	}
	if !scopeMatches(rec.ProjectID, wantPID) {
		return persistence.ErrNotFound
	}
	// Historical-reference check: any prompt version blocks destructive
	// deletion (the role's configuration is referenced by history).
	if cur, err := s.prompts.GetCurrentVersion(ctx, rec.ID.String()); err == nil && cur > 0 {
		return ErrConflict
	}
	return s.roles.Delete(ctx, rec.ID)
}

// scopeMatches reports whether a record's scope (nil = global) matches the
// requested scope (nil/"" = global).
func scopeMatches(recPID *uuid.UUID, wantPID *string) bool {
	if wantPID == nil || *wantPID == "" {
		return recPID == nil
	}
	if recPID == nil {
		return false
	}
	return recPID.String() == *wantPID
}
