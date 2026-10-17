package service

import (
	"context"
	"fmt"
	"overseer/internal/bounds"
	"overseer/internal/dbrepo"

	"overseer/internal/persistence"
)

// SystemPromptView is the API-facing view of a role system-prompt version
// (WBS 5.22).
type SystemPromptView struct {
	Version     int
	Content     string
	ContentHash string
}

// GetCurrentSystemPrompt returns the role's current system prompt (WBS 5.22);
// ErrNotFound when the role has none.
func (s *RoleService) GetCurrentSystemPrompt(ctx context.Context, roleID string) (*SystemPromptView, error) {
	r, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return nil, err
	}
	cur, err := s.prompts.GetCurrentVersion(ctx, r.ID.String())
	if err != nil {
		return nil, err
	}
	if cur == 0 {
		return nil, persistence.ErrNotFound
	}
	v, err := s.prompts.GetVersion(ctx, r.ID.String(), cur)
	if err != nil {
		return nil, err
	}
	return &SystemPromptView{Version: v.Version, Content: v.Content, ContentHash: v.ContentHash}, nil
}

// UpdateSystemPrompt stores content as a NEW version only when the role's
// current version equals expectedVersion (WBS 5.22 T5.22.4): a stale expected
// version is ErrConflict.
func (s *RoleService) UpdateSystemPrompt(ctx context.Context, roleID, content string, expectedVersion int) (*SystemPromptView, error) {
	if err := bounds.Check(bounds.RolePrompt, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	r, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return nil, err
	}
	// The version check and the append are one transaction: a stale
	// expected version leaves the prompt untouched.
	var rec *persistence.SystemPromptRecord
	appendChecked := func(tx persistence.Transaction) error {
		prompts := s.prompts
		if tx != nil {
			prompts = s.prompts.WithTx(tx)
		}
		var err error
		if rec, err = prompts.AppendVersion(ctx, r.ID.String(), content); err != nil {
			return err
		}
		if rec.Version != expectedVersion+1 {
			return persistence.ErrConflict
		}
		return nil
	}
	if s.txPool == nil {
		err = appendChecked(nil)
	} else {
		err = dbrepo.Run(ctx, s.txPool, appendChecked)
	}
	if err != nil {
		return nil, err
	}
	return &SystemPromptView{Version: rec.Version, Content: rec.Content, ContentHash: rec.ContentHash}, nil
}

// ListSystemPromptVersions returns all stored versions in ascending order
// (WBS 5.22 T5.22.3).
func (s *RoleService) ListSystemPromptVersions(ctx context.Context, roleID string) ([]*SystemPromptView, error) {
	r, err := s.roles.Get(ctx, roleID)
	if err != nil {
		return nil, err
	}
	versions, err := s.prompts.ListVersions(ctx, r.ID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*SystemPromptView, 0, len(versions))
	for _, v := range versions {
		out = append(out, &SystemPromptView{Version: v.Version, Content: v.Content, ContentHash: v.ContentHash})
	}
	return out, nil
}
