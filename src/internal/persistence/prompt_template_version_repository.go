package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PromptTemplateVersionRecord is the domain-facing view of a stored prompt
// template content version (WBS 8.4): immutable historical content plus its
// deterministic content hash. ContentHash is the SHA-256 over the exact
// stored bytes (prompt.ContentHash).
type PromptTemplateVersionRecord struct {
	ID         uuid.UUID
	TemplateID uuid.UUID

	Version int
	Content string

	ContentHash string

	CreatedAt time.Time
}

// PromptTemplateVersionRepository is the domain-facing persistence contract
// for prompt-template content versions (WBS 8.4 persistence half, P8-11).
// AppendVersion is the only writer of a version row (append-only; a version
// number is never reused). GetVersion/ListVersions/GetCurrentVersion read the
// stored history.
type PromptTemplateVersionRepository interface {
	// AppendVersion stores content as the next version, computed as
	// MAX(version)+1 inside a transaction serialized by a per-template
	// advisory lock and wrapped in a bounded retry on the (template_id,
	// version) unique constraint (T8.4.5). An unknown template yields
	// ErrNotFound.
	AppendVersion(ctx context.Context, templateID uuid.UUID, content string) (*PromptTemplateVersionRecord, error)
	// GetCurrentVersion returns the template's current (highest) version,
	// resolved from the stored history. An unknown template yields
	// ErrNotFound; a template with no versions returns (nil, nil).
	GetCurrentVersion(ctx context.Context, templateID uuid.UUID) (*PromptTemplateVersionRecord, error)
	// GetVersion returns the stored version; a missing version yields
	// ErrNotFound.
	GetVersion(ctx context.Context, templateID uuid.UUID, version int) (*PromptTemplateVersionRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, templateID uuid.UUID) ([]*PromptTemplateVersionRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) PromptTemplateVersionRepository
}
