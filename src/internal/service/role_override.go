package service

import (
	"context"

	"overseer/internal/persistence"
)

// UpdateRole edits a project role's definition (WBS 5.17): it updates the
// role's name/description and increments its version. The update targets only
// the given role row, so the global default and other projects' copies are
// untouched (T5.17.2/T5.17.3); the version increments (T5.17.4). A system
// prompt edit is a separate, versioned operation (WBS 5.5) — this override
// touches the role's metadata, not its prompt history.
func (s *RoleService) UpdateRole(ctx context.Context, roleID, name, description string) (*persistence.RoleRecord, error) {
	return s.roles.Update(ctx, parseRoleID(roleID), name, description)
}
