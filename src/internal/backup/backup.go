// Package backup is the Go-native logical backup / restore of the
// overseer database (Phase 18, WBS 18.41–18.47; PLAN Decision 6). A backup
// copies every table inside one REPEATABLE READ READ ONLY transaction, so
// it is transactionally consistent even while executions are running, and
// writes a manifest with per-table row counts and sha256 hashes. A restore
// loads a backup into a clean database and marks it VALIDATION_PENDING:
// the application validates it before execution is enabled.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FormatVersion identifies the backup layout.
const FormatVersion = 1

// excluded tables are per-database state, never copied.
var excluded = map[string]bool{"restore_state": true}

// Table is one copied table.
type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	SHA256  string   `json:"sha256"`
	File    string   `json:"file"`
}

// Manifest describes a backup (WBS 18.42).
type Manifest struct {
	Format        int       `json:"format"`
	BackupID      uuid.UUID `json:"backup_id"`
	CreatedAt     time.Time `json:"created_at"`
	SchemaVersion int       `json:"schema_version"`
	AppVersion    string    `json:"app_version"`
	Profile       string    `json:"profile"`
	Reason        string    `json:"reason"`
	Snapshot      string    `json:"snapshot"`
	Tables        []Table   `json:"tables"`
}

// Options are the backup metadata.
type Options struct {
	AppVersion string
	Profile    string
	// Reason is "manual" (default) or "pre_migration" (WBS 18.140).
	Reason string
}

// ManifestFile is the manifest file name inside a backup directory.
const ManifestFile = "manifest.json"

// Hash returns the sha256 of the manifest file content.
func manifestHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Backup writes a consistent logical backup of the database into dir
// (created; must be empty or absent) and records it in backup_records.
func Backup(ctx context.Context, pool *pgxpool.Pool, dir string, o Options) (*Manifest, error) {
	if o.Reason == "" {
		o.Reason = "manual"
	}
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o750); err != nil {
		return nil, fmt.Errorf("backup: create dir: %w", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 1 {
		return nil, fmt.Errorf("backup: %s is not empty", dir)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("backup: begin snapshot: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	m := &Manifest{Format: FormatVersion, BackupID: uuid.New(), CreatedAt: time.Now().UTC(), AppVersion: o.AppVersion,
		Profile: o.Profile, Reason: o.Reason}
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&m.SchemaVersion); err != nil {
		return nil, fmt.Errorf("backup: schema version: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT pg_current_snapshot()::text`).Scan(&m.Snapshot); err != nil {
		return nil, fmt.Errorf("backup: snapshot id: %w", err)
	}
	tables, err := orderedTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i, name := range tables {
		cols, err := columns(ctx, tx, name)
		if err != nil {
			return nil, err
		}
		file := fmt.Sprintf("tables/%03d_%s.copy", i+1, name)
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("backup: %s: %w", name, err)
		}
		h := sha256.New()
		tag, err := tx.Conn().PgConn().CopyTo(ctx, io.MultiWriter(f, h), fmt.Sprintf(`COPY public.%s (%s) TO STDOUT`, ident(name), identList(cols)))
		cerr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("backup: copy %s: %w", name, err)
		}
		if cerr != nil {
			return nil, fmt.Errorf("backup: write %s: %w", name, cerr)
		}
		m.Tables = append(m.Tables, Table{Name: name, Columns: cols, Rows: tag.RowsAffected(), SHA256: hex.EncodeToString(h.Sum(nil)), File: file})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o640); err != nil {
		return nil, fmt.Errorf("backup: manifest: %w", err)
	}
	if err := record(ctx, pool, dir, m, raw); err != nil {
		return nil, err
	}
	return m, nil
}

// record inserts the backup's metadata into backup_records (idempotent).
// A database whose schema predates backup_records (an upgrade from an
// older version, WBS 18.140) is skipped: RecordManifest records the
// backup once the schema has been migrated.
func record(ctx context.Context, pool *pgxpool.Pool, dir string, m *Manifest, raw []byte) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.backup_records') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("backup: record: %w", err)
	}
	if !exists {
		return nil
	}
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	abs, _ := filepath.Abs(dir)
	if _, err := pool.Exec(ctx, `INSERT INTO backup_records (id, created_at, schema_version, app_version, location, reason, tables, total_rows, manifest_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`, m.BackupID, m.CreatedAt, m.SchemaVersion, m.AppVersion, abs, m.Reason,
		len(m.Tables), rows, manifestHash(raw)); err != nil {
		return fmt.Errorf("backup: record: %w", err)
	}
	return nil
}

// RecordManifest records the (verified) backup in dir in backup_records;
// recording twice is a no-op.
func RecordManifest(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return fmt.Errorf("backup: manifest: %w", err)
	}
	return record(ctx, pool, dir, m, raw)
}

// ReadManifest loads and verifies a backup directory: the manifest parses
// and every table file matches its recorded hash.
func ReadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("backup: manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("backup: manifest: %w", err)
	}
	if m.Format != FormatVersion || m.SchemaVersion <= 0 || len(m.Tables) == 0 {
		return nil, errors.New("backup: manifest invalid or unsupported")
	}
	for _, t := range m.Tables {
		f, err := os.Open(filepath.Join(dir, filepath.Clean(t.File)))
		if err != nil {
			return nil, fmt.Errorf("backup: %s: %w", t.Name, err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		if hex.EncodeToString(h.Sum(nil)) != t.SHA256 {
			return nil, fmt.Errorf("backup: %w: table %s does not match its recorded hash", ErrCorrupt, t.Name)
		}
	}
	return &m, nil
}

// ErrCorrupt marks a backup whose content does not match its manifest.
var ErrCorrupt = errors.New("backup corrupt")

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// orderedTables lists the public base tables, referenced tables first
// (cycles are broken deterministically by name; restore disables
// constraint triggers when it can, so order is a best effort).
func orderedTables(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("backup: list tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		if !excluded[n] {
			names = append(names, n)
		}
	}
	rows.Close()
	deps := map[string]map[string]bool{}
	rows, err = q.Query(ctx, `SELECT c.relname, r.relname FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid
		JOIN pg_class r ON r.oid = k.confrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE k.contype = 'f' AND n.nspname = 'public' AND c.oid <> r.oid`)
	if err != nil {
		return nil, fmt.Errorf("backup: list foreign keys: %w", err)
	}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return nil, err
		}
		if deps[from] == nil {
			deps[from] = map[string]bool{}
		}
		deps[from][to] = true
	}
	rows.Close()
	return topo(names, deps), nil
}

// topo orders names so that dependencies come first; remaining cycles are
// emitted in name order.
func topo(names []string, deps map[string]map[string]bool) []string {
	sort.Strings(names)
	done := map[string]bool{}
	var out []string
	for len(out) < len(names) {
		progress := false
		for _, n := range names {
			if done[n] {
				continue
			}
			ready := true
			for d := range deps[n] {
				if !done[d] && contains(names, d) {
					ready = false
					break
				}
			}
			if ready {
				done[n] = true
				out = append(out, n)
				progress = true
			}
		}
		if !progress {
			for _, n := range names {
				if !done[n] {
					done[n] = true
					out = append(out, n)
					break
				}
			}
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func columns(ctx context.Context, q querier, table string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT a.attname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = $1 AND a.attnum > 0
		AND NOT a.attisdropped AND a.attgenerated = '' ORDER BY a.attnum`, table)
	if err != nil {
		return nil, fmt.Errorf("backup: columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func identList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = ident(c)
	}
	return strings.Join(out, ", ")
}
