package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/database"
)

// ErrNotClean: restore targets must be empty databases (WBS 18.44).
var ErrNotClean = errors.New("backup: restore target database is not empty")

// ErrIncompatible: the backup's schema is newer than this application.
var ErrIncompatible = errors.New("backup: backup schema is newer than this application")

// RestoreResult summarizes a restore.
type RestoreResult struct {
	Manifest   *Manifest
	Superuser  bool
	TotalRows  int64
	Sequences  int
	Validation string
}

// Restore loads the backup in dir into the clean database behind pool:
// the manifest and table hashes are verified first; the schema is
// migrated to exactly the backup's version; the data is loaded in one
// transaction (constraint triggers disabled when the role allows it);
// owned sequences are reset; restore_state is set VALIDATION_PENDING so
// the application validates the database before enabling execution
// (WBS 18.46). Nothing is written when verification fails.
func Restore(ctx context.Context, pool *pgxpool.Pool, dir string, migs []database.Migration) (*RestoreResult, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	if m.SchemaVersion > database.MaxVersion(migs) {
		return nil, fmt.Errorf("%w (backup %d, application %d)", ErrIncompatible, m.SchemaVersion, database.MaxVersion(migs))
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')`).Scan(&tables); err != nil {
		return nil, err
	}
	if tables != 0 {
		return nil, ErrNotClean
	}
	var upTo []database.Migration
	for _, mg := range migs {
		if mg.Version <= m.SchemaVersion {
			upTo = append(upTo, mg)
		}
	}
	if err := database.Migrate(ctx, pool, upTo); err != nil {
		return nil, fmt.Errorf("backup: migrate restore target: %w", err)
	}
	res := &RestoreResult{Manifest: m}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := tx.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&res.Superuser); err != nil {
		return nil, err
	}
	if res.Superuser {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return nil, err
		}
	}
	// Migrations may seed rows; the backup is authoritative.
	names := ""
	for i, t := range m.Tables {
		if i > 0 {
			names += ", "
		}
		names += "public." + ident(t.Name)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE `+names+` CASCADE`); err != nil {
		return nil, fmt.Errorf("backup: clear seeded rows: %w", err)
	}
	for _, t := range m.Tables {
		f, err := os.Open(filepath.Join(dir, filepath.Clean(t.File)))
		if err != nil {
			return nil, err
		}
		tag, err := tx.Conn().PgConn().CopyFrom(ctx, f, fmt.Sprintf(`COPY public.%s (%s) FROM STDIN`, ident(t.Name), identList(t.Columns)))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("backup: load %s: %w", t.Name, err)
		}
		if tag.RowsAffected() != t.Rows {
			return nil, fmt.Errorf("backup: %w: %s loaded %d rows, manifest says %d", ErrCorrupt, t.Name, tag.RowsAffected(), t.Rows)
		}
		res.TotalRows += t.Rows
	}
	rows, err := tx.Query(ctx, `SELECT s.relname, t.relname, a.attname FROM pg_class s JOIN pg_depend d ON d.objid = s.oid
		JOIN pg_class t ON t.oid = d.refobjid JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		JOIN pg_namespace n ON n.oid = s.relnamespace WHERE s.relkind = 'S' AND n.nspname = 'public' AND d.deptype IN ('a', 'i')`)
	if err != nil {
		return nil, err
	}
	type seq struct{ s, t, c string }
	var seqs []seq
	for rows.Next() {
		var q seq
		if err := rows.Scan(&q.s, &q.t, &q.c); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, q)
	}
	rows.Close()
	for _, q := range seqs {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT setval('public.%s', COALESCE((SELECT max(%s) FROM public.%s), 0) + 1, false)`,
			ident(q.s), ident(q.c), ident(q.t))); err != nil {
			return nil, fmt.Errorf("backup: reset sequence %s: %w", q.s, err)
		}
		res.Sequences++
	}
	if _, err := tx.Exec(ctx, `INSERT INTO restore_state (id, backup_id, backup_created_at, schema_version, status, restored_at)
		VALUES (1, $1, $2, $3, 'VALIDATION_PENDING', now())`, m.BackupID, m.CreatedAt, m.SchemaVersion); err != nil {
		return nil, fmt.Errorf("backup: restore state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	res.Validation = "VALIDATION_PENDING"
	return res, nil
}
