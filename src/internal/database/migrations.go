package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is a single versioned database change.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// NonTransactional migrations (first line "-- overseer:nontransactional")
	// run step by step outside a transaction with resumable progress
	// (WBS 18.133); steps are separated by "-- overseer:step" lines and
	// must be idempotent.
	NonTransactional bool
}

// NonTransactionalDirective marks a migration that cannot run in a
// transaction (e.g. CREATE INDEX CONCURRENTLY).
const NonTransactionalDirective = "-- overseer:nontransactional"

// StepSeparator separates the steps of a non-transactional migration.
const StepSeparator = "-- overseer:step"

// Steps returns the statements of a non-transactional migration.
func (m Migration) Steps() []string {
	var out []string
	for _, part := range strings.Split(m.SQL, StepSeparator) {
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), NonTransactionalDirective))
		if body != "" {
			out = append(out, body)
		}
	}
	return out
}

// ErrSchemaNewer: the database schema is newer than this binary knows
// (WBS 18.134). The application refuses to run against it.
var ErrSchemaNewer = errors.New("migrations: database schema is newer than this application")

// ErrSchemaOutdated: the database schema is older than this binary and
// migrations are disabled.
var ErrSchemaOutdated = errors.New("migrations: database schema is older than this application and migrations are disabled")

// MaxVersion returns the highest version of migs (0 when empty).
func MaxVersion(migs []Migration) int {
	if len(migs) == 0 {
		return 0
	}
	return migs[len(migs)-1].Version
}

// CheckCompatibility compares the database schema with the application's
// migrations (WBS 18.130 / 18.134): a newer database is ErrSchemaNewer; an
// older one is ErrSchemaOutdated unless canMigrate.
func CheckCompatibility(ctx context.Context, pool *pgxpool.Pool, migs []Migration, canMigrate bool) (int, error) {
	db, err := CurrentVersion(ctx, pool)
	if err != nil {
		return 0, err
	}
	app := MaxVersion(migs)
	switch {
	case db > app:
		return db, fmt.Errorf("%w (database %d, application %d)", ErrSchemaNewer, db, app)
	case db < app && !canMigrate:
		return db, fmt.Errorf("%w (database %d, application %d)", ErrSchemaOutdated, db, app)
	}
	return db, nil
}

var migrationFileRE = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.up\.sql$`)

// LoadMigrations reads all <version>_<name>.up.sql files from dir and
// returns them sorted by version (T1.5.5). Duplicate versions are an error.
func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: read dir: %w", err)
	}
	seen := map[int]string{}
	var migs []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migrations: %s: %w", e.Name(), err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations: duplicate version %d (%s and %s)", v, prev, e.Name())
		}
		seen[v] = e.Name()
		sql, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		migs = append(migs, Migration{Version: v, Name: m[2], SQL: string(sql),
			NonTransactional: strings.HasPrefix(strings.TrimSpace(string(sql)), NonTransactionalDirective)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	return migs, nil
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INT PRIMARY KEY,
	name TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// CurrentVersion returns the highest applied migration version, or 0 when
// no migrations have been applied.
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var v int
	err := pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		if isMissingTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("migrations: current version: %w", err)
	}
	return v, nil
}

func isMissingTable(err error) bool {
	var c interface{ SQLState() string }
	if errors.As(err, &c) {
		return c.SQLState() == "42P01"
	}
	return false
}

// migrationLockKey serializes migration runs across instances.
const migrationLockKey = "overseer:migrations"

const progressDDL = `CREATE TABLE IF NOT EXISTS schema_migration_progress (
	version INT PRIMARY KEY,
	name TEXT NOT NULL,
	steps_done INT NOT NULL,
	steps_total INT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('IN_PROGRESS', 'FAILED')),
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// MigrationProgress is a non-transactional migration that has not
// finished (WBS 18.133: diagnosable after a crash).
type MigrationProgress struct {
	Version    int
	Name       string
	StepsDone  int
	StepsTotal int
	Status     string
	LastError  string
}

// PendingProgress lists unfinished non-transactional migrations.
func PendingProgress(ctx context.Context, pool *pgxpool.Pool) ([]MigrationProgress, error) {
	rows, err := pool.Query(ctx, `SELECT version, name, steps_done, steps_total, status, last_error FROM schema_migration_progress ORDER BY version`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []MigrationProgress
	for rows.Next() {
		var p MigrationProgress
		if err := rows.Scan(&p.Version, &p.Name, &p.StepsDone, &p.StepsTotal, &p.Status, &p.LastError); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Migrate applies all pending migrations in version order. Each migration
// runs in its own transaction: a failure rolls back only that migration,
// leaves earlier ones committed, records no version for the failure, and
// returns an error that must stop application startup (T1.5.4).
// Concurrent instances serialize on a session advisory lock (WBS 18.134).
// Non-transactional migrations run step by step with resumable progress
// (WBS 18.133).
func Migrate(ctx context.Context, pool *pgxpool.Pool, migs []Migration) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrations: acquire lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, migrationLockKey); err != nil {
		return fmt.Errorf("migrations: lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, migrationLockKey)
	}()
	return migrateLocked(ctx, pool, migs)
}

func migrateLocked(ctx context.Context, pool *pgxpool.Pool, migs []Migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrations: begin: %w", err)
	}
	if _, err := tx.Exec(ctx, schemaMigrationsDDL); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("migrations: create bookkeeping table: %w", err)
	}
	if _, err := tx.Exec(ctx, progressDDL); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("migrations: create progress table: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrations: commit bookkeeping table: %w", err)
	}

	for _, m := range migs {
		var applied bool
		err := pool.QueryRow(ctx, "SELECT true FROM schema_migrations WHERE version = $1", m.Version).Scan(&applied)
		if err == nil && applied {
			continue
		}
		if m.NonTransactional {
			if err := applyNonTransactional(ctx, pool, m); err != nil {
				return err
			}
			continue
		}
		mtx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migrations: begin v%d: %w", m.Version, err)
		}
		if _, err := mtx.Exec(ctx, m.SQL); err != nil {
			mtx.Rollback(ctx)
			return fmt.Errorf("migrations: apply v%d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := mtx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
			mtx.Rollback(ctx)
			return fmt.Errorf("migrations: record v%d: %w", m.Version, err)
		}
		if err := mtx.Commit(ctx); err != nil {
			return fmt.Errorf("migrations: commit v%d: %w", m.Version, err)
		}
	}
	return nil
}

// applyNonTransactional runs the remaining steps of m, recording progress
// after each one; a crash or failure leaves the progress row, and the next
// run resumes after the last completed step.
func applyNonTransactional(ctx context.Context, pool *pgxpool.Pool, m Migration) error {
	steps := m.Steps()
	done := 0
	err := pool.QueryRow(ctx, `SELECT steps_done FROM schema_migration_progress WHERE version = $1`, m.Version).Scan(&done)
	if err != nil {
		done = 0
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migration_progress (version, name, steps_done, steps_total, status)
			VALUES ($1, $2, 0, $3, 'IN_PROGRESS')`, m.Version, m.Name, len(steps)); err != nil {
			return fmt.Errorf("migrations: record progress v%d: %w", m.Version, err)
		}
	}
	for i := done; i < len(steps); i++ {
		if _, err := pool.Exec(ctx, steps[i]); err != nil {
			_, _ = pool.Exec(context.WithoutCancel(ctx), `UPDATE schema_migration_progress SET status = 'FAILED', last_error = $2, updated_at = now() WHERE version = $1`,
				m.Version, err.Error())
			return fmt.Errorf("migrations: apply v%d (%s) step %d/%d: %w", m.Version, m.Name, i+1, len(steps), err)
		}
		if _, err := pool.Exec(ctx, `UPDATE schema_migration_progress SET steps_done = $2, status = 'IN_PROGRESS', last_error = '', updated_at = now() WHERE version = $1`,
			m.Version, i+1); err != nil {
			return fmt.Errorf("migrations: record progress v%d: %w", m.Version, err)
		}
		if stepHook != nil {
			if err := stepHook(m.Version, i+1); err != nil {
				return err
			}
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
		return fmt.Errorf("migrations: record v%d: %w", m.Version, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migration_progress WHERE version = $1`, m.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// stepHook is a test seam run after each non-transactional step.
var stepHook func(version, step int) error

// SetStepHook installs the non-transactional step hook (tests).
func SetStepHook(h func(version, step int) error) { stepHook = h }
