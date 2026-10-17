package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/app"
	"overseer/internal/backup"
	"overseer/internal/execution"
	"overseer/internal/service"
)

// p18RestoreOver replaces database name with a restore of the backup in
// dir (the harness on it must be closed); after restore it runs sql (may
// be "") against the restored database.
func p18RestoreOver(t *testing.T, name, dir, sql string) {
	t.Helper()
	ctx := context.Background()
	base := baseDSN()
	dropDB(t, base, name)
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("pg: %v", err)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	pool, err := pgxpool.New(ctx, strings.Replace(base, "/devpipe", "/"+name, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := backup.Restore(ctx, pool, dir, p18Migrations(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sql != "" {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
}

func p18RestoreStatus(t *testing.T, b *browser) service.RestoreStatus {
	t.Helper()
	code, raw := b.api(http.MethodGet, "/api/v1/system/restore", nil)
	var st service.RestoreStatus
	if code != http.StatusOK || json.Unmarshal(raw, &st) != nil {
		t.Fatalf("restore status: %d %s", code, raw)
	}
	return st
}

// p18RestoreRig runs a project partway (the plan dispatched, the unit not
// yet), backs the database up mid-execution and restores it in place.
func p18RestoreRig(t *testing.T, afterRestore string) (*p16Rig, *browser, int) {
	t.Helper()
	r, restart := p16RestartRig(t, p16Options{config: p17Config(nil)})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	ctx := context.Background()
	if _, err := r.eng().Run(ctx, r.execID, 3); err != nil {
		t.Fatal(err)
	}
	sends := len(r.g.Messages())
	if sends == 0 || r.execStatus(t) == "COMPLETED" {
		t.Fatalf("setup: sends %d status %s", sends, r.execStatus(t))
	}
	dir := filepath.Join(t.TempDir(), "backup")
	if _, err := backup.Backup(ctx, r.h.App.DB().Raw(), dir, backup.Options{AppVersion: app.Version, Profile: "test"}); err != nil {
		t.Fatal(err)
	}
	name := r.h.DBName()
	r.h.keepDB = true
	r.h.Close()
	p18RestoreOver(t, name, dir, afterRestore)
	h := restart()
	return r, newBrowser(t, h.BaseURL), sends
}

// T18.44 / T18.45 + decision U4 — a database restored mid-execution starts
// read-only with no dispatch even though every check passes: mutations are
// refused, the engine sends nothing; an operator reviews the report and
// enables execution (audited RESTORE_ENABLE), after which the restored
// execution resumes through normal reconciliation and completes.
func TestT1844RestoreGateEnable(t *testing.T) {
	r, b, sends := p18RestoreRig(t, "")
	ctx := context.Background()
	st := p18RestoreStatus(t, b)
	if !st.Restored || !st.Active || st.State == nil || st.State.Status != service.RestoreValidated {
		t.Fatalf("restore gate: %+v", st)
	}
	raw, _ := json.Marshal(st.State.Detail)
	for _, check := range []string{"schema_compatibility", "integrity_scan", "audit_chain", "recovery_scan"} {
		if !strings.Contains(string(raw), `"name":"`+check+`","ok":true`) {
			t.Fatalf("check %s missing / failing: %s", check, raw)
		}
	}
	if code, _ := b.page("/ready"); code != http.StatusOK {
		t.Fatalf("/ready %d after a validated restore", code)
	}
	// Read-only: a mutation is refused (and audited as REJECTED); reads work.
	code, body := b.api(http.MethodPost, "/api/v1/projects/"+r.pid.String()+"/pause", nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "INSTANCE_READ_ONLY") {
		t.Fatalf("mutation on a held restore: %d %s", code, body)
	}
	if code, _ := b.api(http.MethodGet, "/api/v1/projects/"+r.pid.String(), nil); code != http.StatusOK {
		t.Fatalf("read refused: %d", code)
	}
	// 18.45: the RUNNING execution is not dispatched.
	_, _ = r.eng().Run(ctx, r.execID, 10)
	if n := len(r.g.Messages()); n != sends {
		t.Fatalf("%d new worker requests on a held restore", n-sends)
	}
	var wait string
	_ = r.h.DBScan(ctx, "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &wait)
	if !strings.Contains(wait, "restored database") {
		t.Fatalf("wait reason %q", wait)
	}
	// Enable: reason required; then audited, the gate opens.
	if code, _ := b.api(http.MethodPost, "/api/v1/system/restore/enable", map[string]any{"reason": ""}); code != http.StatusBadRequest {
		t.Fatalf("blank reason: %d", code)
	}
	if code, body := b.api(http.MethodPost, "/api/v1/system/restore/enable", map[string]any{"reason": "reviewed the restore report"}); code != http.StatusOK {
		t.Fatalf("enable: %d %s", code, body)
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'RESTORE_ENABLE' AND result = 'SUCCEEDED'"); n != 1 {
		t.Fatalf("enable audited %d times", n)
	}
	if st := p18RestoreStatus(t, b); st.Active || st.State.Status != service.RestoreEnabled || st.State.EnabledBy == "" {
		t.Fatalf("after enable: %+v", st)
	}
	if code, _ := b.api(http.MethodPost, "/api/v1/system/restore/enable", map[string]any{"reason": "again"}); code != http.StatusConflict {
		t.Fatalf("second enable: %d", code)
	}
	r.runUntil(t, execution.StepCompleted, 200)
	if len(r.g.Messages()) <= sends {
		t.Fatal("the restored execution did not resume")
	}
}

// T18.46 — a corrupt or incompatible restore cannot resume workers: the
// failing checks are recorded, the instance is NOT_READY and read-only,
// enabling is refused, and nothing is dispatched.
func TestT1846CorruptRestoreCannotResume(t *testing.T) {
	r, b, sends := p18RestoreRig(t, `UPDATE restore_state SET schema_version = 99999;
		INSERT INTO phases (id, project_id, name, phase_order, status, created_at, updated_at)
		SELECT gen_random_uuid(), project_id, 'Rogue', 99, 'EXECUTING', now(), now() FROM phases LIMIT 1`)
	st := p18RestoreStatus(t, b)
	if !st.Active || st.State == nil || st.State.Status != service.RestoreFailed {
		t.Fatalf("restore gate: %+v", st)
	}
	raw, _ := json.Marshal(st.State.Detail)
	for _, check := range []string{"schema_compatibility", "integrity_scan"} {
		if !strings.Contains(string(raw), `"name":"`+check+`","ok":false`) {
			t.Fatalf("check %s not failing: %s", check, raw)
		}
	}
	if code, _ := b.page("/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready %d after a failed restore", code)
	}
	if code, _ := b.api(http.MethodPost, "/api/v1/system/restore/enable", map[string]any{"reason": "force"}); code != http.StatusConflict {
		t.Fatalf("enable of a failed restore: %d", code)
	}
	_, _ = r.eng().Run(context.Background(), r.execID, 10)
	if n := len(r.g.Messages()); n != sends {
		t.Fatalf("%d worker requests after a failed restore", n-sends)
	}
}
