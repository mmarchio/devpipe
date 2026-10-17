package integration

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/app"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// P18-15 (WBS 18.3 / 18.4 / 18.206) — startup configuration snapshots,
// reproducibility metadata on executions, and drift surfaced (never
// applied) for an active execution.
func TestP1815ConfigSnapshotAndDrift(t *testing.T) {
	base15, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 1}}, p17Config(nil))
	base := &p16Rig{p15Rig: base15}
	p16Configure(t, base, p16Options{})
	r := &p17Rig{p16Rig: base, b: newBrowser(t, base.h.BaseURL)}
	execRepo := func() *dbrepo.ExecutionRepository { return dbrepo.NewExecutionRepository(r.h.App.DB().Raw()) }
	ctx := context.Background()
	pid, eid := r.pid.String(), r.execID.String()

	// 18.206 — the execution records app / schema / config versions; the
	// captured snapshot holds the protocol version and worker model.
	var appVersion, cfgHash string
	var schema int
	if err := r.h.DBScan(ctx, "SELECT app_version, schema_version, app_config_hash FROM executions WHERE id = '"+eid+"'", &appVersion, &schema, &cfgHash); err != nil {
		t.Fatal(err)
	}
	if appVersion != app.Version || schema != r.h.App.SchemaVersion() || cfgHash != r.h.App.ConfigSnapshot().Hash || schema < 112 {
		t.Fatalf("reproducibility: %s %d %s", appVersion, schema, cfgHash)
	}
	snap, err := execRepo().GetSnapshot(ctx, r.execID)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := execution.DecodeContent(snap.Canonical)
	if content.ResponseProtocol.Version == 0 || content.Worker.Model != "hermes-worker" {
		t.Fatalf("snapshot lacks protocol / model: %+v %+v", content.ResponseProtocol, content.Worker)
	}
	// 18.3 — the startup snapshot is recorded without credentials.
	var n int
	var cfgJSON string
	_ = r.h.DBScan(ctx, "SELECT count(*), max(config::text) FROM app_config_snapshots", &n, &cfgJSON)
	if n != 1 || strings.Contains(cfgJSON, dsnPassword(baseDSN())) || !strings.Contains(cfgJSON, `"password_set"`) {
		t.Fatalf("snapshot rows %d: %s", n, cfgJSON)
	}
	// No drift yet.
	var d readmodel.ConfigDrift
	r.b.getJSON("/api/v1/projects/"+pid+"/config-drift", &d)
	if d.ExecutionID != eid || len(d.Items) != 0 {
		t.Fatalf("unexpected drift: %+v", d)
	}
	// 18.4 — project-level changes are surfaced, not applied.
	var st map[string]any
	r.b.getJSON("/api/v1/projects/"+pid+"/settings", &st)
	st["worker_model"] = "brand-new-model"
	if code, raw := r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/settings", st); code != 200 {
		t.Fatalf("settings: %d %s", code, raw)
	}
	if code, raw := r.b.api(http.MethodPut, "/api/v1/projects/"+pid+"/settings/engine", map[string]any{"checkpoint_policy": "BEST_EFFORT"}); code != 200 {
		t.Fatalf("engine: %d %s", code, raw)
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/config-drift", &d)
	sections := []string{}
	for _, it := range d.Items {
		sections = append(sections, it.Source+":"+it.Section)
	}
	if strings.Join(sections, ",") != "project:checkpoint_policy,project:worker" {
		t.Fatalf("drift %v", sections)
	}
	if text := textOf(r.b.mustPage("/projects/" + pid)); !strings.Contains(text, "Configuration Drift") || !strings.Contains(text, "brand-new-model") {
		t.Fatalf("overview lacks the drift banner: %s", text)
	}
	if after, _ := execRepo().GetSnapshot(ctx, r.execID); string(after.Canonical) != string(snap.Canonical) {
		t.Fatal("captured snapshot changed")
	}
	// Restart with a changed resource limit → application drift + a new
	// snapshot row; the change is reported, the credential is not.
	r.h.Close()
	r.h = boot(func(c *config.Config) { c.Engine.RecoveryDisabled = false; c.Database.MaxConns = 7 })
	r.b = newBrowser(t, r.h.BaseURL)
	_ = r.h.DBScan(ctx, "SELECT count(*) FROM app_config_snapshots", &n)
	if n != 2 {
		t.Fatalf("snapshot rows after restart: %d", n)
	}
	r.b.getJSON("/api/v1/projects/"+pid+"/config-drift", &d)
	found := false
	for _, it := range d.Items {
		found = found || (it.Source == "application" && it.Section == "resource_limits")
	}
	if !found {
		t.Fatalf("application drift not reported: %+v", d.Items)
	}
}

// dsnPassword extracts the password of a postgres URL DSN.
func dsnPassword(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "\x00no-password"
	}
	p, ok := u.User.Password()
	if !ok || p == "" {
		return "\x00no-password"
	}
	return p
}
