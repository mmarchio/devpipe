package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PromptTemplateRecord is the domain-facing view of a stored prompt template
// (WBS 8.1). A nil ProjectID is the GLOBAL scope; a set ProjectID is a
// PROJECT scope. Key uniqueness is scoped: the same key may exist in
// different projects and globally, but never twice within one scope (T8.1.3).
type PromptTemplateRecord struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string
	Purpose     string

	Enabled bool
	Version int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// PromptTemplateRepository is the domain-facing persistence contract for
// prompt templates (WBS 8.1 persistence half, P8-10). Create/Update carry the
// scoped-key uniqueness on the DB unique index: a same-scope duplicate yields
// ErrConflict (T8.1.3 DB half). Get/GetByKey return ErrNotFound when the row
// is absent.
type PromptTemplateRepository interface {
	// Create inserts a new template; a scoped key duplicate yields
	// ErrConflict, a missing project reference yields
	// ErrInvalidReference (T8.1.3 DB half).
	Create(ctx context.Context, rec *PromptTemplateRecord) error
	// Get returns the template by id; an unknown id yields ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (*PromptTemplateRecord, error)
	// GetByKey returns the template for (scope, key); an unknown key in
	// scope yields ErrNotFound.
	GetByKey(ctx context.Context, projectID *uuid.UUID, key string) (*PromptTemplateRecord, error)
	// ListGlobal returns the global-scope templates (project_id NULL) in
	// key order (P8-12 seed lookup).
	ListGlobal(ctx context.Context) ([]*PromptTemplateRecord, error)
	// Update mutates the template's mutable fields in place (name,
	// description, purpose, enabled — key and version are immutable);
	// an unknown id yields ErrNotFound.
	Update(ctx context.Context, rec *PromptTemplateRecord) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) PromptTemplateRepository
}
