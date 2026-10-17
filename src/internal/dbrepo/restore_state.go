package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RestoreStateRepository reads and advances the restore gate of a
// database populated by a restore (P18-24, WBS 18.46, decision U4).
type RestoreStateRepository struct{ exec executor }

// NewRestoreStateRepository binds the repository to a pool.
func NewRestoreStateRepository(pool *pgxpool.Pool) *RestoreStateRepository {
	return &RestoreStateRepository{exec: poolAdapter{pool}}
}

// Get returns the restore state (nil when the database was not restored).
func (r *RestoreStateRepository) Get(ctx context.Context) (*persistence.RestoreState, error) {
	var st persistence.RestoreState
	var detail []byte
	err := r.exec.QueryRow(ctx, `SELECT backup_id, backup_created_at, schema_version, status, restored_at, validated_at, detail,
		enabled_at, enabled_by, enable_reason FROM restore_state WHERE id = 1`).Scan(&st.BackupID, &st.BackupCreatedAt, &st.SchemaVersion,
		&st.Status, &st.RestoredAt, &st.ValidatedAt, &detail, &st.EnabledAt, &st.EnabledBy, &st.EnableReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, tr("read restore state", err)
	}
	_ = json.Unmarshal(detail, &st.Detail)
	return &st, nil
}

// RecordValidation stores the startup check outcome (VALIDATED / FAILED)
// and its report; an ENABLED restore is never changed.
func (r *RestoreStateRepository) RecordValidation(ctx context.Context, status string, detail map[string]any, at time.Time) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = r.exec.Exec(ctx, `UPDATE restore_state SET status = $1, detail = $2, validated_at = $3 WHERE id = 1 AND status <> 'ENABLED'`,
		status, b, at.UTC())
	return tr("record restore validation", err)
}

// Enable marks a VALIDATED restore ENABLED (ErrConflict otherwise).
func (r *RestoreStateRepository) Enable(ctx context.Context, by, reason string, at time.Time) error {
	tag, err := r.exec.Exec(ctx, `UPDATE restore_state SET status = 'ENABLED', enabled_at = $1, enabled_by = $2, enable_reason = $3
		WHERE id = 1 AND status = 'VALIDATED'`, at.UTC(), by, reason)
	if err != nil {
		return tr("enable restored database", err)
	}
	if tag.RowsAffected() != 1 {
		return persistence.ErrConflict
	}
	return nil
}
