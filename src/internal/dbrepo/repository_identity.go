package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RepositoryIdentityStore is the PostgreSQL repository identity store
// (P18-40).
type RepositoryIdentityStore struct{ pool *pgxpool.Pool }

// NewRepositoryIdentityStore builds the store.
func NewRepositoryIdentityStore(pool *pgxpool.Pool) *RepositoryIdentityStore {
	return &RepositoryIdentityStore{pool: pool}
}

var _ persistence.RepositoryIdentityStore = (*RepositoryIdentityStore)(nil)

// Get returns the recorded identity.
func (s *RepositoryIdentityStore) Get(ctx context.Context, executionID uuid.UUID) (*persistence.RepositoryIdentityRecord, error) {
	r := &persistence.RepositoryIdentityRecord{}
	err := s.pool.QueryRow(ctx, `SELECT execution_id, project_id, toplevel, root_commit, branch, is_git, source, recorded_at
		FROM execution_repository_identities WHERE execution_id = $1`, executionID).Scan(&r.ExecutionID, &r.ProjectID,
		&r.Identity.Toplevel, &r.Identity.RootCommit, &r.Identity.Branch, &r.Identity.IsGit, &r.Source, &r.RecordedAt)
	if err != nil {
		return nil, TranslateDBError("get repository identity", err)
	}
	return r, nil
}

// Record inserts rec unless an identity is already recorded.
func (s *RepositoryIdentityStore) Record(ctx context.Context, rec *persistence.RepositoryIdentityRecord) (*persistence.RepositoryIdentityRecord, bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO execution_repository_identities (execution_id, project_id, toplevel, root_commit, branch,
		is_git, source, recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (execution_id) DO NOTHING`,
		rec.ExecutionID, rec.ProjectID, rec.Identity.Toplevel, rec.Identity.RootCommit, rec.Identity.Branch, rec.Identity.IsGit,
		rec.Source, rec.RecordedAt.UTC())
	if err != nil {
		return nil, false, TranslateDBError("record repository identity", err)
	}
	stored, err := s.Get(ctx, rec.ExecutionID)
	return stored, err == nil && tag.RowsAffected() == 1, err
}

// LatestExpectedHead returns the current expected HEAD.
func (s *RepositoryIdentityStore) LatestExpectedHead(ctx context.Context, executionID uuid.UUID) (*persistence.ExpectedHeadRecord, error) {
	r := &persistence.ExpectedHeadRecord{}
	err := s.pool.QueryRow(ctx, `SELECT id, execution_id, project_id, seq, head, branch, source, checkpoint_id, actor, reason, recorded_at
		FROM execution_expected_heads WHERE execution_id = $1 ORDER BY seq DESC LIMIT 1`, executionID).Scan(&r.ID, &r.ExecutionID,
		&r.ProjectID, &r.Seq, &r.Head, &r.Branch, &r.Source, &r.CheckpointID, &r.Actor, &r.Reason, &r.RecordedAt)
	if err != nil {
		return nil, TranslateDBError("get expected head", err)
	}
	return r, nil
}

// AppendExpectedHead appends an expected HEAD (next sequence number).
func (s *RepositoryIdentityStore) AppendExpectedHead(ctx context.Context, rec *persistence.ExpectedHeadRecord) (*persistence.ExpectedHeadRecord, error) {
	out := *rec
	out.ID = uuid.New()
	err := s.pool.QueryRow(ctx, `INSERT INTO execution_expected_heads (id, execution_id, project_id, seq, head, branch, source,
		checkpoint_id, actor, reason, recorded_at)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(seq), 0) + 1 FROM execution_expected_heads WHERE execution_id = $2), $4, $5, $6, $7, $8, $9, $10)
		RETURNING seq`, out.ID, out.ExecutionID, out.ProjectID, out.Head, out.Branch, out.Source, out.CheckpointID, out.Actor, out.Reason,
		out.RecordedAt.UTC()).Scan(&out.Seq)
	if err != nil {
		return nil, TranslateDBError("append expected head", err)
	}
	return &out, nil
}
