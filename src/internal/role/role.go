package role

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is the core persistent domain entity (WBS 5.1). A role defines which
// agent persona performs work and the configuration governing that persona.
//
// Scopes (WBS 5.1): a nil ProjectID is the GLOBAL scope (defaults/templates);
// a set ProjectID is a PROJECT scope (the configuration a project actually
// uses). Key uniqueness is scoped: the same key may exist in different
// projects and globally, but never twice within one scope (T5.1.5/T5.1.6).
type Role struct {
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

// RoleInput carries the caller-supplied fields for NewRole. Key is normalized
// per the documented policy before validation; timestamps and Version are
// assigned by the domain.
type RoleInput struct {
	ProjectID   *uuid.UUID
	Key         string
	Name        string
	Description string
}

// NormalizeKey applies the documented key normalization policy (T5.1.4):
// trim leading/trailing whitespace (spaces, tabs, newlines), then uppercase.
// Keys are identifiers, so case-insensitive comparison is canonicalized by
// storing them uppercase ("engineer" → "ENGINEER"); interior characters are
// preserved exactly ("project-manager" → "PROJECT-MANAGER"). The policy is a
// pure function: deterministic, no state.
func NormalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// NewRole validates and constructs a new role (WBS 5.1): normalized non-empty
// key (T5.1.2/T5.1.4), non-empty name (T5.1.3), initial Version 1, Enabled
// true, timestamps set (T5.1.1/T5.1.7).
func NewRole(in RoleInput) (*Role, error) {
	return newRole(in, time.Now)
}

func newRole(in RoleInput, now func() time.Time) (*Role, error) {
	key := NormalizeKey(in.Key)
	if key == "" {
		return nil, fmt.Errorf("role: key must not be empty")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("role: name must not be empty")
	}
	ts := now().UTC()
	return &Role{
		ID:          uuid.New(),
		ProjectID:   in.ProjectID,
		Key:         key,
		Name:        strings.TrimSpace(in.Name),
		Description: in.Description,
		Enabled:     true,
		Version:     1,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}, nil
}

// SameScope reports whether two roles share a scope: both nil ProjectID is
// the global scope; otherwise both ProjectIDs must be equal (T5.1.5/T5.1.6).
func SameScope(a, b Role) bool {
	if a.ProjectID == nil || b.ProjectID == nil {
		return a.ProjectID == nil && b.ProjectID == nil
	}
	return *a.ProjectID == *b.ProjectID
}

// ExistingKeyInScope reports an error when a candidate (key, projectID)
// collides with an existing role's scoped key (T5.1.5): same key AND same
// scope. Different scopes (other projects, or global vs project) are allowed
// (T5.1.6).
func ExistingKeyInScope(existing *Role, candidateKey string, candidateProjectID *uuid.UUID) error {
	if existing == nil {
		return nil
	}
	if NormalizeKey(candidateKey) != existing.Key {
		return nil
	}
	if sameProjectID(existing.ProjectID, candidateProjectID) {
		return fmt.Errorf("role: key %q already exists in scope", existing.Key)
	}
	return nil
}

func sameProjectID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
