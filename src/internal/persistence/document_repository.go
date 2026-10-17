package persistence

import (
	"context"
)

// WBSDocumentRecord is the domain-facing view of a phase WBS document
// (WBS 3.5). The service only needs to know that a document with content
// exists for a phase.
type WBSDocumentRecord struct {
	ID      string
	PhaseID string
	Content string
	Version int
}

// WBSDocumentRepository is the domain-facing persistence contract for WBS
// documents (WBS 3.5).
type WBSDocumentRepository interface {
	// GetByPhase returns the WBS document of a phase; ErrNotFound when the
	// phase has none.
	GetByPhase(ctx context.Context, phaseID string) (*WBSDocumentRecord, error)
	// Create stores the phase's first WBS document at version 1 (WBS 3.5).
	Create(ctx context.Context, phaseID, content string) (*WBSDocumentRecord, error)
	// UpdateContent replaces the document content and increments its
	// version (WBS 3.18).
	UpdateContent(ctx context.Context, phaseID, content string) error
	// Delete removes the WBS document of a phase (WBS 4.6); ErrNotFound
	// when the phase has none.
	Delete(ctx context.Context, phaseID string) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21, 3.26).
	WithTx(tx Transaction) WBSDocumentRepository
}
