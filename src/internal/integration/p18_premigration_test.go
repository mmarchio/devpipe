package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/backup"
	"overseer/internal/config"
	"overseer/internal/database"
)

// p18OldSchemaDB creates a database migrated only to version upTo.
func p18OldSchemaDB(t *testing.T, upTo int) (*pgxpool.Pool, string) {
	t.Helper()
	pool, name := p18CleanDB(t)
	var migs []database.Migration
	for _, m := range p18Migrations(t) {
		if m.Version <= upTo {
			migs = append(migs, m)
		}
	}
	if err := database.Migrate(context.Background(), pool, migs); err != nil {
		t.Fatal(err)
	}
	return pool, name
}

func p18SchemaVersion(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	v, err := database.CurrentVersion(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// T18.140 + decision U5 — startup with pending migrations takes a
// consistent backup first; its manifest and record carry the pre-upgrade
// schema version (recorded after the migration when the old schema
// predates backup_records). A failing backup stops startup with the
// schema untouched.
func TestT18140PreMigrationBackup(t *testing.T) {
	const from = 106
	ctx := context.Background()
	pool, name := p18OldSchemaDB(t, from)
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name, description, status, created_at, updated_at)
		VALUES (gen_random_uuid(), 'pre-upgrade', '', 'DRAFT', now(), now())`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := NewHarnessOnWith(t, baseDSN(), name, func(c *config.Config) { c.Backup.Dir = dir })
	t.Cleanup(h.Close)
	if v := p18SchemaVersion(t, pool); v != database.MaxVersion(p18Migrations(t)) {
		t.Fatalf("schema %d after startup", v)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "pre-migration-v106-") {
		t.Fatalf("backup dirs: %v", entries)
	}
	bdir := filepath.Join(dir, entries[0].Name())
	m, err := backup.ReadManifest(bdir)
	if err != nil || m.SchemaVersion != from || m.Reason != "pre_migration" {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	var recSchema int
	var reason string
	if err := pool.QueryRow(ctx, `SELECT schema_version, reason FROM backup_records WHERE id = $1`, m.BackupID).Scan(&recSchema, &reason); err != nil ||
		recSchema != from || reason != "pre_migration" {
		t.Fatalf("backup record: %d %q %v", recSchema, reason, err)
	}
	found := false
	for _, tb := range m.Tables {
		found = found || (tb.Name == "projects" && tb.Rows == 1)
	}
	if !found {
		t.Fatal("the pre-upgrade data is not in the backup")
	}
	// A current schema takes no backup at the next start.
	h.Close()
	h2 := NewHarnessOnWith(t, baseDSN(), name, func(c *config.Config) { c.Backup.Dir = dir })
	t.Cleanup(h2.Close)
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a backup was taken without pending migrations: %d", len(entries))
	}

	// Failure: the backup cannot be written → startup stops, nothing is
	// migrated.
	pool2, name2 := p18OldSchemaDB(t, from)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := p18AppConfig(t, name2)
	cfg.Backup.Dir = blocker
	a := newHarnessApp(cfg)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.Start(sctx); err == nil || !strings.Contains(err.Error(), "pre-migration backup") {
		_ = a.Shutdown(sctx)
		t.Fatalf("startup with a failing backup: %v", err)
	}
	if v := p18SchemaVersion(t, pool2); v != from {
		t.Fatalf("schema migrated to %d despite the failed backup", v)
	}
	// Production without backup.dir refuses to migrate.
	cfg = p18AppConfig(t, name2)
	cfg.Profile = config.ProfileProduction
	if err := newHarnessApp(cfg).Start(sctx); err == nil || !strings.Contains(err.Error(), "backup.dir is not configured") {
		t.Fatalf("production start without backup.dir: %v", err)
	}
	if v := p18SchemaVersion(t, pool2); v != from {
		t.Fatalf("schema migrated to %d without a backup", v)
	}
}

// p18AppConfig is the harness configuration for a database, unstarted.
func p18AppConfig(t *testing.T, dbName string) config.Config {
	t.Helper()
	host, port, user, pass, ssl := parseDSNFields(t, baseDSN())
	cfg := config.Default()
	cfg.Database.Host, cfg.Database.Port, cfg.Database.Name, cfg.Database.User, cfg.Database.Password, cfg.Database.SSLMode =
		host, port, dbName, user, pass, ssl
	cfg.Server.Host, cfg.Server.Port = "127.0.0.1", freePort(t)
	cfg.Engine.SchedulerDisabled = true
	return cfg
}
