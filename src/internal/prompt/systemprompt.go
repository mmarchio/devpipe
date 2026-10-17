package prompt

import (
	"errors"
	"fmt"

	"overseer/internal/role"
)

// ErrMissingSystemPromptVersion reports a composition failure: the
// RoleSnapshot carries no usable captured system-prompt version (WBS
// 8.10, T8.10.3).
var ErrMissingSystemPromptVersion = errors.New("prompt: snapshot carries no captured system prompt version")

// ResolveSystemPrompt resolves the exact system-prompt version captured
// in the RoleSnapshot (WBS 8.10). It uses the snapshot's captured version
// and content — never "latest" — so a later role edit is ignored (there is
// no later-version input to this function), a missing historical version
// is a composition failure (ErrMissingSystemPromptVersion), and the
// snapshot's source hash is retained on the resulting component.
func ResolveSystemPrompt(snap role.RoleSnapshot) (PromptComponent, error) {
	sp := snap.SystemPrompt
	if sp.Version <= 0 || sp.Content == "" {
		return PromptComponent{}, fmt.Errorf("role %s: %w", snap.RoleKey, ErrMissingSystemPromptVersion)
	}
	c, err := NewPromptComponent(
		ComponentSystem,
		SourceRoleSystemPrompt,
		0,
		sp.Content,
		snap.RoleID,
		sp.Version,
		sp.ContentHash,
	)
	if err != nil {
		return PromptComponent{}, err
	}
	return c, nil
}
