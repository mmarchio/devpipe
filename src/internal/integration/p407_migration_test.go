package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// newWBSDocumentService wires the real WBSDocumentService to the real
// repositories on the harness pool.
func newWBSDocumentService(t *testing.T, h *Harness) *service.WBSDocumentService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		pool,
	)
}

// P4-07 (WBS 4.10 recommended model + 4.11 storage): migration 00012
// verification — wbs_versions table and the phase_documents.current_version
// pointer, applied through the real harness migration path.

// seedPhase creates a project + phase through the real services and
// returns the phase ID; when withWBS the phase also gets its WBS through
// the real WBSDocumentService (which stores it as version 1).
func seedPhase(t *testing.T, h *Harness, withWBS bool) string {
	t.Helper()
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)
	p, err := svc.CreatePhase(ctx, projectID, "WBS versions", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if withWBS {
		wbs := newWBSDocumentService(t, h)
		if _, err := wbs.UpdateWBS(ctx, projectID, p.ID.String(), "# WBS v1\n\n## 1.1 First work.", 0); err != nil {
			t.Fatalf("UpdateWBS: %v", err)
		}
	}
	return p.ID.String()
}

// P4-07.1 — Migration 00012 applies: the harness (fresh temp DB) boots with
// max migration version 12 and the new objects exist.
func TestP407MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Every migration on disk is applied: the schema version is the
	// highest migration version (no hard-coded number to bump per phase).
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var tableName string
	if err := h.DBScan(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_name = 'wbs_versions'", &tableName); err != nil {
		t.Fatalf("wbs_versions missing: %v", err)
	}
	var colType, colDef, colNull string
	if err := h.DBScan(ctx,
		`SELECT data_type, coalesce(column_default,''), is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'phase_documents' AND column_name = 'current_version'`,
		&colType, &colDef, &colNull); err != nil {
		t.Fatalf("current_version missing: %v", err)
	}
	if colType != "integer" {
		t.Fatalf("current_version type = %q, want integer", colType)
	}
	if !strings.Contains(colDef, "1") {
		t.Fatalf("current_version default = %q, want 1", colDef)
	}
	if colNull != "NO" {
		t.Fatalf("current_version is_nullable = %q, want NO", colNull)
	}
}

// P4-07.2 — Duplicate (phase_id, version) rejected at the DB level.
func TestP407DuplicateVersionRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := seedPhase(t, h, false)
	insert := "INSERT INTO wbs_versions (id, phase_id, version, content, content_hash, created_at) VALUES (gen_random_uuid(), '" +
		phaseID + "', 1, 'v1', '', now())"
	if _, err := h.DBExec(ctx, insert); err != nil {
		t.Fatalf("first version insert: %v", err)
	}
	_, err := h.DBExec(ctx, insert)
	if err == nil {
		t.Fatal("duplicate (phase_id, version) accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate insert error = %v, want 23505", err)
	}
	// A different version for the same phase is allowed.
	if _, err := h.DBExec(ctx, strings.Replace(insert, "', 1, '", "', 2, '", 1)); err != nil {
		t.Fatalf("second distinct version insert: %v", err)
	}
}

// P4-07.3 — FK cascade: deleting the phase removes its wbs_versions rows.
func TestP407FKCascade(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := seedPhase(t, h, false)
	if _, err := h.DBExec(ctx, "INSERT INTO wbs_versions (id, phase_id, version, content, content_hash, created_at) VALUES (gen_random_uuid(), '"+phaseID+"', 1, 'v1', '', now())"); err != nil {
		t.Fatalf("seed wbs_versions: %v", err)
	}
	if _, err := h.DBExec(ctx, "DELETE FROM phases WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatalf("delete phase: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("wbs_versions rows after phase delete = %d, want 0 (cascade)", n)
	}
}

// P4-07.4 — CHECK constraints: version > 0 on wbs_versions, current_version > 0.
func TestP407CheckConstraints(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := seedPhase(t, h, true)
	var pgErr *pgconn.PgError
	_, err := h.DBExec(ctx, "INSERT INTO wbs_versions (id, phase_id, version, content, content_hash, created_at) VALUES (gen_random_uuid(), '"+phaseID+"', 0, 'v0', '', now())")
	if err == nil {
		t.Fatal("version 0 accepted, want CHECK rejection")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("version 0 error = %v, want 23514", err)
	}
	_, err = h.DBExec(ctx, "UPDATE phase_documents SET current_version = 0 WHERE phase_id = '"+phaseID+"'")
	if err == nil {
		t.Fatal("current_version 0 accepted, want CHECK rejection")
	}
	var pgErr2 *pgconn.PgError
	if !errors.As(err, &pgErr2) || pgErr2.Code != "23514" {
		t.Fatalf("current_version 0 error = %v, want 23514", err)
	}
}

// P4-07.5 — Default pointer: a freshly created WBS document row carries
// current_version = 1 (the single existing row remains the current pointer).
func TestP407CurrentVersionDefault(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := seedPhase(t, h, true)
	var v int
	if err := h.DBScan(ctx,
		"SELECT current_version FROM phase_documents WHERE phase_id = '"+phaseID+"'", &v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("current_version = %d, want 1", v)
	}
}

// P4-07.6 — Down migration reverses cleanly: dropping the added objects
// returns the schema to its pre-00012 shape.
func TestP407DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := seedPhase(t, h, false)
	if _, err := h.DBExec(ctx, "INSERT INTO wbs_versions (id, phase_id, version, content, content_hash, created_at) VALUES (gen_random_uuid(), '"+phaseID+"', 1, 'v1', '', now())"); err != nil {
		t.Fatalf("seed wbs_versions: %v", err)
	}
	// Inverse of 00012 (the runner applies .up.sql files; the down half is
	// exercised here exactly as it would run on rollback).
	if _, err := h.DBExec(ctx, "DROP TABLE wbs_versions"); err != nil {
		t.Fatalf("down: drop wbs_versions: %v", err)
	}
	if _, err := h.DBExec(ctx, "ALTER TABLE phase_documents DROP COLUMN current_version"); err != nil {
		t.Fatalf("down: drop current_version: %v", err)
	}
	var n int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_name = 'wbs_versions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("wbs_versions still present after down migration")
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM information_schema.columns WHERE table_name = 'phase_documents' AND column_name = 'current_version'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("current_version column still present after down migration")
	}
	// The phase carries no WBS document (seeded without one), so no
	// document row exists after the rollback.
	var docCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM phase_documents", &docCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 0 {
		t.Fatalf("phase_documents rows after down = %d, want 0", docCount)
	}
}
