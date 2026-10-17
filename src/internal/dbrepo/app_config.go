package dbrepo

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// AppConfigSnapshotRepository stores startup configuration snapshots
// (P18-15).
type AppConfigSnapshotRepository struct{ pool *pgxpool.Pool }

// NewAppConfigSnapshotRepository builds the repository.
func NewAppConfigSnapshotRepository(pool *pgxpool.Pool) *AppConfigSnapshotRepository {
	return &AppConfigSnapshotRepository{pool: pool}
}

// Insert appends a snapshot.
func (r *AppConfigSnapshotRepository) Insert(ctx context.Context, s *persistence.AppConfigSnapshotRecord) error {
	secs, err := json.Marshal(s.Sections)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO app_config_snapshots (id, created_at, instance_id, profile, app_version, schema_version,
		config_hash, sections, config) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, s.ID, s.CreatedAt.UTC(), s.InstanceID, s.Profile,
		s.AppVersion, s.SchemaVersion, s.ConfigHash, secs, s.Config)
	return tr("insert app config snapshot", err)
}

const appConfigCols = `id, created_at, instance_id, profile, app_version, schema_version, config_hash, sections, config`

func scanAppConfig(row interface{ Scan(...any) error }) (*persistence.AppConfigSnapshotRecord, error) {
	var s persistence.AppConfigSnapshotRecord
	var secs []byte
	if err := row.Scan(&s.ID, &s.CreatedAt, &s.InstanceID, &s.Profile, &s.AppVersion, &s.SchemaVersion, &s.ConfigHash, &secs, &s.Config); err != nil {
		return nil, tr("read app config snapshot", err)
	}
	_ = json.Unmarshal(secs, &s.Sections)
	return &s, nil
}

// Latest returns the newest snapshot.
func (r *AppConfigSnapshotRepository) Latest(ctx context.Context) (*persistence.AppConfigSnapshotRecord, error) {
	return scanAppConfig(r.pool.QueryRow(ctx, `SELECT `+appConfigCols+` FROM app_config_snapshots ORDER BY created_at DESC, id LIMIT 1`))
}

// ByHash returns a snapshot carrying hash.
func (r *AppConfigSnapshotRepository) ByHash(ctx context.Context, hash string) (*persistence.AppConfigSnapshotRecord, error) {
	return scanAppConfig(r.pool.QueryRow(ctx, `SELECT `+appConfigCols+` FROM app_config_snapshots WHERE config_hash = $1 ORDER BY created_at DESC LIMIT 1`, hash))
}
