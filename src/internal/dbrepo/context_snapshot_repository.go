package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// ContextSnapshotRepository persists execution-context snapshots
// (WBS 7.31, P7-18) on PostgreSQL: the snapshot row plus its state-file
// pins, written atomically (a partial persist is a persistence failure —
// WBS 7.32.4). The role snapshot is stored as JSONB; the file pins carry
// the exact version, hash and content bytes (WBS 7.30).
type ContextSnapshotRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.ContextSnapshotRepository = (*ContextSnapshotRepository)(nil)

// NewContextSnapshotRepository binds the repository to an existing pool.
func NewContextSnapshotRepository(pool *pgxpool.Pool) *ContextSnapshotRepository {
	return &ContextSnapshotRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *ContextSnapshotRepository) WithTx(tx persistence.Transaction) persistence.ContextSnapshotRepository {
	return &ContextSnapshotRepository{exec: txExecutor{tx}}
}

// Create inserts the snapshot and all of its file pins.
func (r *ContextSnapshotRepository) Create(ctx context.Context, s *persistence.ContextSnapshotRecord) error {
	if _, err := r.exec.Exec(ctx, `
		INSERT INTO context_snapshots (
			id, project_id, phase_id, work_unit_id,
			runtime_state_id, runtime_state_version,
			state_machine_id, state_machine_version, state_key,
			role_snapshot, state_file_tokens, role_tokens, estimated_total, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		s.ID, s.ProjectID, s.PhaseID, s.WorkUnitID,
		s.RuntimeStateID, s.RuntimeStateVersion,
		s.StateMachineID, s.StateMachineVersion, s.StateKey,
		s.RoleSnapshotJSON, s.StateFileTokens, s.RoleTokens, s.EstimatedTotal, s.CreatedAt,
	); err != nil {
		return TranslateDBError("context_snapshot.create", err)
	}
	for _, f := range s.Files {
		if _, err := r.exec.Exec(ctx, `
			INSERT INTO context_snapshot_files (
				id, snapshot_id, state_file_id, type_key, scope,
				version, content_hash, content,
				state_file_token_estimate, classification, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			f.ID, f.SnapshotID, f.StateFileID, f.TypeKey, f.Scope,
			f.Version, f.Hash, f.Content, f.TokenEstimate, f.Classification, f.CreatedAt,
		); err != nil {
			return TranslateDBError("context_snapshot_file.create", err)
		}
	}
	return nil
}

func scanContextSnapshotFileRow(row interface{ Scan(dest ...any) error }) (*persistence.ContextSnapshotFileRecord, error) {
	var rec persistence.ContextSnapshotFileRecord
	if err := row.Scan(&rec.ID, &rec.SnapshotID, &rec.StateFileID, &rec.TypeKey,
		&rec.Scope, &rec.Version, &rec.Hash, &rec.Content,
		&rec.TokenEstimate, &rec.Classification, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *ContextSnapshotRepository) listFiles(ctx context.Context, snapshotID uuid.UUID) ([]*persistence.ContextSnapshotFileRecord, error) {
	rows, err := r.exec.Query(ctx, `
		SELECT id, snapshot_id, state_file_id, type_key, scope,
		       version, content_hash, content,
		       state_file_token_estimate, classification, created_at
		FROM context_snapshot_files
		WHERE snapshot_id = $1
		ORDER BY type_key`, snapshotID)
	if err != nil {
		return nil, TranslateDBError("context_snapshot_file.list", err)
	}
	defer rows.Close()
	var out []*persistence.ContextSnapshotFileRecord
	for rows.Next() {
		rec, err := scanContextSnapshotFileRow(rows)
		if err != nil {
			return nil, TranslateDBError("context_snapshot_file.scan", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("context_snapshot_file.rows", err)
	}
	return out, nil
}

func (r *ContextSnapshotRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.ContextSnapshotRecord, error) {
	row := r.exec.QueryRow(ctx, `
		SELECT id, project_id, phase_id, work_unit_id,
		       runtime_state_id, runtime_state_version,
		       state_machine_id, state_machine_version, state_key,
		       role_snapshot, state_file_tokens, role_tokens, estimated_total, created_at
		FROM context_snapshots WHERE id = $1`, id)
	rec, err := scanContextSnapshotRow(row)
	if err != nil {
		return nil, TranslateDBError("context_snapshot.get", err)
	}
	files, err := r.listFiles(ctx, id)
	if err != nil {
		return nil, err
	}
	rec.Files = files
	return rec, nil
}

func (r *ContextSnapshotRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.ContextSnapshotRecord, error) {
	rows, err := r.exec.Query(ctx, `
		SELECT id, project_id, phase_id, work_unit_id,
		       runtime_state_id, runtime_state_version,
		       state_machine_id, state_machine_version, state_key,
		       role_snapshot, state_file_tokens, role_tokens, estimated_total, created_at
		FROM context_snapshots
		WHERE project_id = $1
		ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, TranslateDBError("context_snapshot.list", err)
	}
	defer rows.Close()
	var out []*persistence.ContextSnapshotRecord
	for rows.Next() {
		rec, err := scanContextSnapshotRow(rows)
		if err != nil {
			return nil, TranslateDBError("context_snapshot.scan", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("context_snapshot.rows", err)
	}
	for _, rec := range out {
		files, err := r.listFiles(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
		rec.Files = files
	}
	return out, nil
}

func scanContextSnapshotRow(row interface{ Scan(dest ...any) error }) (*persistence.ContextSnapshotRecord, error) {
	var rec persistence.ContextSnapshotRecord
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.PhaseID, &rec.WorkUnitID,
		&rec.RuntimeStateID, &rec.RuntimeStateVersion,
		&rec.StateMachineID, &rec.StateMachineVersion, &rec.StateKey,
		&rec.RoleSnapshotJSON, &rec.StateFileTokens, &rec.RoleTokens, &rec.EstimatedTotal, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}
