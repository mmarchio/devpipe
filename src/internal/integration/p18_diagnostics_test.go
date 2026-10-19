package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/service"
)

// T18.185–T18.187 — diagnostic bundles: no configured secret appears;
// the project bundle identifies everything by ids and hashes (enough to
// look artifacts up) without mutating the project; a huge project's
// bundle stays bounded.
func TestT18185DiagnosticBundles(t *testing.T) {
	const token = "SENTINEL-diag-token-5f2a"
	const csrf = "SENTINEL-diag-csrf-0123456789abcdef0123456789abcdef"
	tokenFile := filepath.Join(t.TempDir(), "gw.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P18_DIAG_CSRF", csrf)
	r := p17NewRigWith(t, p16Options{noBootstrap: true, config: func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.AuthSecretEnv, p.AuthSecretFile = "", tokenFile
		c.WorkerProviders["hermes"] = p
		c.Web.CSRFKeyEnv = "P18_DIAG_CSRF"
	}})
	// The gateway must accept the file token before bootstrap, which
	// checks the worker session.
	r.g.SetToken(token)
	r.boot(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	pid := r.pid.String()

	// 18.187 fixture: a project with a very long history.
	r.exec(t, `INSERT INTO execution_events (id, project_id, project_seq, execution_id, type, payload, created_at)
		SELECT gen_random_uuid(), '`+pid+`', (SELECT max(project_seq) FROM execution_events WHERE project_id = '`+pid+`') + g, '`+r.execID.String()+`',
			'EXECUTION_WAITING', jsonb_build_object('reason', repeat('x', 500)), now()
		FROM generate_series(1, 5000) g`)

	before := dbFingerprint(t, r)
	code, sysRaw := r.b.api(http.MethodGet, "/api/v1/system/diagnostics", nil)
	if code != http.StatusOK {
		t.Fatalf("system diagnostics %d %s", code, sysRaw)
	}
	var sys service.SystemBundle
	if err := json.Unmarshal(sysRaw, &sys); err != nil || sys.Version == "" || sys.ConfigHash == "" || sys.Health == nil || sys.Facts == nil ||
		sys.Facts.ExecutionCount == 0 || len(sys.ConfigSections) == 0 {
		t.Fatalf("system bundle: %v %.400s", err, sysRaw)
	}
	code, projRaw := r.b.api(http.MethodGet, "/api/v1/projects/"+pid+"/diagnostics", nil)
	if code != http.StatusOK {
		t.Fatalf("project diagnostics %d %s", code, projRaw)
	}
	var proj service.ProjectBundle
	if err := json.Unmarshal(projRaw, &proj); err != nil {
		t.Fatal(err)
	}
	// 18.185: no secret anywhere.
	for _, s := range []string{token, csrf} {
		if strings.Contains(string(sysRaw), s) || strings.Contains(string(projRaw), s) {
			t.Fatalf("secret %q in a diagnostic bundle", s)
		}
	}
	// 18.186: ids and hashes suffice; nothing was mutated.
	f := proj.Facts
	if len(f.Dispatches) == 0 || f.Dispatches[0]["prompt_hash"] == "" || len(f.Plans) == 0 || f.Plans[0]["wbs_hash"] == "" ||
		len(f.Units) == 0 || f.Units[0]["objective_hash"] == "" || len(proj.LineagePaths) == 0 {
		t.Fatalf("project bundle lacks identifiers: %.600s", projRaw)
	}
	if c := r.b.getJSON(proj.LineagePaths[0], nil); c != http.StatusOK {
		t.Fatalf("referenced lineage not reachable: %d", c)
	}
	if strings.Contains(string(projRaw), "Planned the phase") || strings.Contains(string(projRaw), strings.Repeat("x", 500)) {
		t.Fatal("the bundle carries content")
	}
	if dbFingerprint(t, r) != before {
		t.Fatal("diagnostics mutated the project")
	}
	// 18.187: bounded.
	if len(f.EventTail) > 200 || len(projRaw) > service.MaxBundleBytes {
		t.Fatalf("bundle not bounded: %d events, %d bytes", len(f.EventTail), len(projRaw))
	}
}
