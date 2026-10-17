package dbrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// Document is the persisted representation of a phase document (WBS 2.9/2.10).
type Document struct {
	ID        string
	PhaseID   string
	DocType   string
	Content   string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DocumentRepository persists phase documents on PostgreSQL.
type DocumentRepository struct {
	exec executor
}

// NewDocumentRepository binds the repository to an existing pool.
func NewDocumentRepository(pool *pgxpool.Pool) *DocumentRepository {
	return &DocumentRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *DocumentRepository) WithTx(tx persistence.Transaction) *DocumentRepository {
	return &DocumentRepository{exec: txExecutor{tx}}
}

// Create inserts a new document at version 1 (T2.10.1).
func (r *DocumentRepository) Create(ctx context.Context, phaseID, docType, content string) (*Document, error) {
	now := time.Now().UTC()
	doc := &Document{
		ID: newUUIDString(), PhaseID: phaseID, DocType: docType, Content: content,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	row := r.exec.QueryRow(ctx,
		`INSERT INTO phase_documents (id, phase_id, doc_type, content, version, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,1,$5,$5) RETURNING id, phase_id, doc_type, content, version, created_at, updated_at`,
		doc.ID, phaseID, docType, content, now)
	if err := row.Scan(&doc.ID, &doc.PhaseID, &doc.DocType, &doc.Content, &doc.Version, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return nil, TranslateDBError("create document", err)
	}
	return doc, nil
}

// GetByPhase returns the WBS document for the given phase (T2.10.3);
// ErrNotFound when the phase has no WBS document.
func (r *DocumentRepository) GetByPhase(ctx context.Context, phaseID string) (*Document, error) {
	doc := &Document{}
	row := r.exec.QueryRow(ctx,
		`SELECT id, phase_id, doc_type, content, version, created_at, updated_at
		 FROM phase_documents WHERE phase_id = $1 AND doc_type = 'WBS'`, phaseID)
	if err := row.Scan(&doc.ID, &doc.PhaseID, &doc.DocType, &doc.Content, &doc.Version, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return nil, TranslateDBError("get document", err)
	}
	return doc, nil
}

// UpdateContent replaces the document content, increments the version, and
// refreshes updated_at (T2.10.4).
func (r *DocumentRepository) UpdateContent(ctx context.Context, phaseID, content string) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE phase_documents SET content = $2, version = version + 1, updated_at = now()
		 WHERE phase_id = $1 AND doc_type = 'WBS'`, phaseID, content)
	if err != nil {
		return TranslateDBError("update document", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// Delete removes a document (T2.10.5 cascade handled by the FK).
func (r *DocumentRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM phase_documents WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete document", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
