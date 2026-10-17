package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

const responseProtocolColumns = `id, project_id, name, kind, enabled, created_at, updated_at`
const responseProtocolVersionColumns = `id, protocol_id, version, content, content_hash, created_at`

// ResponseProtocolRepository implements response-protocol persistence on
// PostgreSQL (WBS 8.23 persistence half, P8-13), mirroring the
// prompt-template recipe (P8-10/P8-11): scoped-kind uniqueness on a unique
// index, and append-only content versions whose next version is
// MAX(version)+1 computed inside a transaction serialized by a
// protocol-scoped advisory lock, wrapped in a bounded retry on the
// (protocol_id, version) unique constraint (T8.23.2). The content hash is
// the P8-02 domain function (prompt.ContentHash).
type ResponseProtocolRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.ResponseProtocolRepository = (*ResponseProtocolRepository)(nil)

// NewResponseProtocolRepository binds the repository to an existing pool.
func NewResponseProtocolRepository(pool *pgxpool.Pool) *ResponseProtocolRepository {
	return &ResponseProtocolRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *ResponseProtocolRepository) WithTx(tx persistence.Transaction) persistence.ResponseProtocolRepository {
	return &ResponseProtocolRepository{exec: txExecutor{tx}}
}

func scanResponseProtocolRow(row interface{ Scan(dest ...any) error }) (*persistence.ResponseProtocolRecord, error) {
	var rec persistence.ResponseProtocolRecord
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &rec.Kind, &rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

func scanResponseProtocolVersionRow(row interface{ Scan(dest ...any) error }) (*persistence.ResponseProtocolVersionRecord, error) {
	var rec persistence.ResponseProtocolVersionRecord
	if err := row.Scan(&rec.ID, &rec.ProtocolID, &rec.Version, &rec.Content, &rec.ContentHash, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Create inserts a new protocol with its v1 version row (T8.23.1); a scoped
// kind duplicate yields persistence.ErrConflict, a missing project reference
// yields persistence.ErrInvalidReference.
func (r *ResponseProtocolRepository) Create(ctx context.Context, rec *persistence.ResponseProtocolRecord, initialContent string) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO response_protocols (id, project_id, name, kind, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rec.ID, rec.ProjectID, rec.Name, rec.Kind, rec.Enabled, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create response protocol", err)
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO response_protocol_versions (id, protocol_id, version, content, content_hash, created_at)
		 VALUES ($1, $2, 1, $3, $4, $5)`,
		uuid.New(), rec.ID, initialContent, prompt.ContentHash(initialContent), rec.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create response protocol initial version", err)
	}
	return nil
}

// Get returns the response protocol with the given id; an unknown id yields
// persistence.ErrNotFound.
func (r *ResponseProtocolRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.ResponseProtocolRecord, error) {
	rec, err := scanResponseProtocolRow(r.exec.QueryRow(ctx, `SELECT `+responseProtocolColumns+` FROM response_protocols WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get response protocol", err)
	}
	return rec, nil
}

func (r *ResponseProtocolRepository) protocolExists(ctx context.Context, protocolID uuid.UUID) error {
	var exists bool
	if err := r.exec.QueryRow(ctx, `SELECT true FROM response_protocols WHERE id = $1`, protocolID).Scan(&exists); err != nil {
		return TranslateDBError("get response protocol", err)
	}
	if !exists {
		return persistence.ErrNotFound
	}
	return nil
}

// AppendVersion stores content as the next version (T8.23.2). A missing
// protocol yields persistence.ErrNotFound BEFORE any write.
func (r *ResponseProtocolRepository) AppendVersion(ctx context.Context, protocolID uuid.UUID, content string) (*persistence.ResponseProtocolVersionRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v, err := r.appendProtocolVersionOnce(ctx, protocolID, content)
		if err == nil || !errors.Is(err, persistence.ErrConflict) {
			return v, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *ResponseProtocolRepository) appendProtocolVersionOnce(ctx context.Context, protocolID uuid.UUID, content string) (*persistence.ResponseProtocolVersionRecord, error) {
	// Fast-path protocol existence check: a missing protocol is a plain
	// ErrNotFound, not a conflict.
	if err := r.protocolExists(ctx, protocolID); err != nil {
		return nil, err
	}
	tx, joined, err := r.beginProtocolVersionTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "response-protocol-versions:"+protocolID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var cur int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM response_protocol_versions WHERE protocol_id = $1`, protocolID).
		Scan(&cur); err != nil {
		return nil, TranslateDBError("max response protocol version", err)
	}
	next := cur + 1
	v := &persistence.ResponseProtocolVersionRecord{
		ID: uuid.New(), ProtocolID: protocolID, Version: next,
		Content: content, ContentHash: prompt.ContentHash(content),
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO response_protocol_versions (id, protocol_id, version, content, content_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+responseProtocolVersionColumns,
		v.ID, protocolID, next, content, v.ContentHash)
	if err := row.Scan(&v.ID, &v.ProtocolID, &v.Version, &v.Content, &v.ContentHash, &v.CreatedAt); err != nil {
		return nil, TranslateDBError("insert response protocol version", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit response protocol version append", err)
		}
	}
	return v, nil
}

func (r *ResponseProtocolRepository) beginProtocolVersionTx(ctx context.Context) (persistence.Transaction, bool, error) {
	if te, ok := r.exec.(txExecutor); ok {
		return te.tx, true, nil
	}
	if b, ok := r.exec.(persistence.TxBeginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return nil, false, err
		}
		return tx, false, nil
	}
	return nil, false, &persistence.InfrastructureError{Op: "response protocol version append", Cause: errors.New("executor cannot begin transactions")}
}

// GetCurrentVersion returns the protocol's current (highest) version,
// resolved from the stored history; an unknown protocol yields ErrNotFound
// and a protocol with no versions returns (nil, nil).
func (r *ResponseProtocolRepository) GetCurrentVersion(ctx context.Context, protocolID uuid.UUID) (*persistence.ResponseProtocolVersionRecord, error) {
	if err := r.protocolExists(ctx, protocolID); err != nil {
		return nil, err
	}
	var cur int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM response_protocol_versions WHERE protocol_id = $1`, protocolID).Scan(&cur); err != nil {
		return nil, TranslateDBError("get current response protocol version", err)
	}
	if cur == 0 {
		return nil, nil
	}
	return r.GetVersion(ctx, protocolID, cur)
}

// GetVersion returns the stored version; a missing version yields
// persistence.ErrNotFound (T8.23.4: historical versions stay retrievable).
func (r *ResponseProtocolRepository) GetVersion(ctx context.Context, protocolID uuid.UUID, version int) (*persistence.ResponseProtocolVersionRecord, error) {
	v, err := scanResponseProtocolVersionRow(r.exec.QueryRow(ctx,
		`SELECT `+responseProtocolVersionColumns+` FROM response_protocol_versions WHERE protocol_id = $1 AND version = $2`,
		protocolID, version))
	if err != nil {
		return nil, TranslateDBError("get response protocol version", err)
	}
	return v, nil
}

// ListVersions returns all stored versions in ascending order.
func (r *ResponseProtocolRepository) ListVersions(ctx context.Context, protocolID uuid.UUID) ([]*persistence.ResponseProtocolVersionRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+responseProtocolVersionColumns+` FROM response_protocol_versions WHERE protocol_id = $1 ORDER BY version ASC`, protocolID)
	if err != nil {
		return nil, TranslateDBError("list response protocol versions", err)
	}
	defer rows.Close()
	var out []*persistence.ResponseProtocolVersionRecord
	for rows.Next() {
		v, err := scanResponseProtocolVersionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list response protocol versions", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list response protocol versions", err)
	}
	return out, nil
}

// CreateStateReference pins a state to (protocol, version) (T8.23.3). The
// state, the protocol, and the pinned version must all exist; unknown state
// or protocol (or an absent version) yields persistence.ErrNotFound, a
// duplicate (state, protocol) reference yields persistence.ErrConflict.
func (r *ResponseProtocolRepository) CreateStateReference(ctx context.Context, stateID, protocolID uuid.UUID, version int) (*persistence.StateProtocolReferenceRecord, error) {
	var exists bool
	if err := r.exec.QueryRow(ctx, `SELECT true FROM workflow_states WHERE id = $1`, stateID).Scan(&exists); err != nil {
		return nil, TranslateDBError("state protocol reference state check", err)
	}
	if !exists {
		return nil, persistence.ErrNotFound
	}
	var versionExists bool
	if err := r.exec.QueryRow(ctx, `SELECT true FROM response_protocol_versions WHERE protocol_id = $1 AND version = $2`, protocolID, version).Scan(&versionExists); err != nil {
		return nil, TranslateDBError("state protocol reference version check", err)
	}
	if !versionExists {
		// Covers both a missing protocol row and a version the protocol does
		// not carry.
		return nil, persistence.ErrNotFound
	}
	rec := &persistence.StateProtocolReferenceRecord{StateID: stateID, ProtocolID: protocolID, ProtocolVersion: version}
	row := r.exec.QueryRow(ctx,
		`INSERT INTO state_protocol_references (state_id, protocol_id, protocol_version)
		 VALUES ($1, $2, $3) RETURNING state_id, protocol_id, protocol_version, created_at`,
		stateID, protocolID, version)
	if err := row.Scan(&rec.StateID, &rec.ProtocolID, &rec.ProtocolVersion, &rec.CreatedAt); err != nil {
		return nil, TranslateDBError("create state protocol reference", err)
	}
	return rec, nil
}

// GetStateReference returns the state's reference to the protocol; an absent
// reference yields persistence.ErrNotFound.
func (r *ResponseProtocolRepository) GetStateReference(ctx context.Context, stateID, protocolID uuid.UUID) (*persistence.StateProtocolReferenceRecord, error) {
	rec := &persistence.StateProtocolReferenceRecord{}
	if err := r.exec.QueryRow(ctx,
		`SELECT state_id, protocol_id, protocol_version, created_at FROM state_protocol_references WHERE state_id = $1 AND protocol_id = $2`,
		stateID, protocolID).Scan(&rec.StateID, &rec.ProtocolID, &rec.ProtocolVersion, &rec.CreatedAt); err != nil {
		return nil, TranslateDBError("get state protocol reference", err)
	}
	return rec, nil
}

// FindByKind returns the protocol of a kind visible to a project: the
// project-scoped one when present, else the global one (P13-33). Absent →
// persistence.ErrNotFound.
func (r *ResponseProtocolRepository) FindByKind(ctx context.Context, projectID *uuid.UUID, kind string) (*persistence.ResponseProtocolRecord, error) {
	rec, err := scanResponseProtocolRow(r.exec.QueryRow(ctx, `SELECT `+responseProtocolColumns+` FROM response_protocols
		WHERE kind = $1 AND (project_id = $2 OR project_id IS NULL) ORDER BY project_id NULLS LAST LIMIT 1`, kind, projectID))
	if err != nil {
		return nil, TranslateDBError("find response protocol by kind", err)
	}
	return rec, nil
}
