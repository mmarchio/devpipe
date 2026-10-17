package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RoleRecord is the domain-facing view of a stored role (WBS 5.1). A nil
// ProjectID is the GLOBAL scope; a set value is a PROJECT scope.
type RoleRecord struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string

	Enabled bool
	Version int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RoleRepository is the domain-facing persistence contract for role
// definitions (WBS 5.1). Key uniqueness is scoped: the same key may exist in
// different projects and globally, but never twice within one scope.
type RoleRepository interface {
	// Create inserts a new role. A scoped key duplicate (same normalized key
	// in the same scope) yields ErrConflict.
	Create(ctx context.Context, r *RoleRecord) error
	// Get returns the role with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*RoleRecord, error)
	// ListByScope lists all roles in one scope: projectID nil lists the
	// global scope, a set id lists that project's roles. Ordered by key.
	ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*RoleRecord, error)
	// SetEnabled enables or disables the role (WBS 5.13). Disabling never
	// deletes prompt history (T5.13.3). ErrNotFound when the role is missing.
	SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (*RoleRecord, error)
	// Update mutates the role's editable fields (name/description) and
	// increments its version (WBS 5.17). ErrNotFound when the role is missing.
	Update(ctx context.Context, id uuid.UUID, name, description string) (*RoleRecord, error)
	// Delete removes the role row (WBS 5.21). The application constrains
	// destructive deletion of historically referenced roles before calling.
	Delete(ctx context.Context, id uuid.UUID) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) RoleRepository
}

// SystemPromptRecord is the domain-facing view of a stored system-prompt
// version (WBS 5.5): immutable historical content plus its deterministic
// content hash.
type SystemPromptRecord struct {
	ID          string
	RoleID      string
	Version     int
	Content     string
	ContentHash string
	CreatedAt   time.Time
}

// SystemPromptRepository is the domain-facing persistence contract for
// role system-prompt version history (WBS 5.5). Appends are append-only:
// historical versions are never overwritten, and the role's current-version
// pointer always tracks the latest.
type SystemPromptRepository interface {
	// AppendVersion stores content as the next version (MAX(version)+1) with
	// its content hash and advances the role's current-version pointer.
	AppendVersion(ctx context.Context, roleID, content string) (*SystemPromptRecord, error)
	// GetVersion returns the stored version; ErrNotFound when the role has
	// no such version.
	GetVersion(ctx context.Context, roleID string, version int) (*SystemPromptRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, roleID string) ([]*SystemPromptRecord, error)
	// GetCurrentVersion returns the role's current version (0 when the role
	// has no system prompt).
	GetCurrentVersion(ctx context.Context, roleID string) (int, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) SystemPromptRepository
}
