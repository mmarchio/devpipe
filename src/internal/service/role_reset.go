package service

import (
	"context"
	"fmt"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// ResetRole explicitly resets a project role from the current GLOBAL default
// of the same key (WBS 5.18): the project role's editable metadata is copied
// from the global default, the global's CURRENT system-prompt content is
// appended as a NEW version of the project role's prompt (history retained,
// T5.18.3), and the project role's version increments (T5.18.1). The reset
// touches only the named project role — other roles, the global, and other
// projects are unaffected (T5.18.4). When a tx pool is wired the metadata
// update + prompt append are one transaction.
func (s *RoleService) ResetRole(ctx context.Context, projectRoleID string) (*persistence.RoleRecord, error) {
	r, err := s.roles.Get(ctx, projectRoleID)
	if err != nil {
		return nil, err
	}
	if r.ProjectID == nil {
		return nil, ErrInvalidInput
	}
	// Find the matching GLOBAL default by key.
	globals, err := s.roles.ListByScope(ctx, nil)
	if err != nil {
		return nil, err
	}
	var g *persistence.RoleRecord
	for _, cand := range globals {
		if cand.Key == r.Key {
			g = cand
			break
		}
	}
	if g == nil {
		return nil, fmt.Errorf("reset %s: no global default with key %q: %w", r.Key, r.Key, persistence.ErrNotFound)
	}
	globalContent, err := s.currentPromptContent(ctx, s.prompts, g.ID.String())
	if err != nil {
		return nil, err
	}
	var out *persistence.RoleRecord
	if s.txPool == nil {
		out, err = s.resetOnce(ctx, nil, r, g, globalContent)
		return out, err
	}
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		out, err = s.resetOnce(ctx, tx, r, g, globalContent)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resetOnce performs the metadata copy + prompt-version append; when tx is
// non-nil both join it.
func (s *RoleService) resetOnce(ctx context.Context, tx persistence.Transaction, r, g *persistence.RoleRecord, globalContent string) (*persistence.RoleRecord, error) {
	roles := s.roles
	prompts := s.prompts
	if tx != nil {
		roles = s.roles.WithTx(tx)
		prompts = s.prompts.WithTx(tx)
	}
	if _, err := roles.Update(ctx, r.ID, g.Name, g.Description); err != nil {
		return nil, err
	}
	if _, err := prompts.AppendVersion(ctx, r.ID.String(), globalContent); err != nil {
		return nil, err
	}
	return roles.Get(ctx, r.ID.String())
}

// SetRoleAsDefault is the reverse of ResetRole: the GLOBAL default of the
// project role's key takes the project role's current name, description
// and system prompt (appended as a NEW version of the global prompt, so
// its history is retained; skipped when the content is already current).
// Projects initialized afterwards copy the new default; existing project
// roles are untouched. It returns the updated global default.
func (s *RoleService) SetRoleAsDefault(ctx context.Context, projectRoleID string) (*persistence.RoleRecord, error) {
	r, err := s.roles.Get(ctx, projectRoleID)
	if err != nil {
		return nil, err
	}
	if r.ProjectID == nil {
		return nil, ErrInvalidInput
	}
	globals, err := s.roles.ListByScope(ctx, nil)
	if err != nil {
		return nil, err
	}
	var g *persistence.RoleRecord
	for _, cand := range globals {
		if cand.Key == r.Key {
			g = cand
			break
		}
	}
	if g == nil {
		return nil, fmt.Errorf("set %s as default: no global default with key %q: %w", r.Key, r.Key, persistence.ErrNotFound)
	}
	content, err := s.currentPromptContent(ctx, s.prompts, r.ID.String())
	if err != nil {
		return nil, err
	}
	globalContent, err := s.currentPromptContent(ctx, s.prompts, g.ID.String())
	if err != nil {
		return nil, err
	}
	apply := func(tx persistence.Transaction) error {
		roles, prompts := s.roles, s.prompts
		if tx != nil {
			roles, prompts = s.roles.WithTx(tx), s.prompts.WithTx(tx)
		}
		if _, err := roles.Update(ctx, g.ID, r.Name, r.Description); err != nil {
			return err
		}
		if content != globalContent {
			if _, err := prompts.AppendVersion(ctx, g.ID.String(), content); err != nil {
				return err
			}
		}
		return nil
	}
	if s.txPool == nil {
		err = apply(nil)
	} else {
		err = dbrepo.Run(ctx, s.txPool, apply)
	}
	if err != nil {
		return nil, err
	}
	return s.roles.Get(ctx, g.ID.String())
}
