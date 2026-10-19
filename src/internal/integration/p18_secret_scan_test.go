package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/gateway/hermes/hermestest"
)

// T18.74–T18.77 — configured secrets (worker gateway token, compression
// API key, CSRF key) never reach prompts, the database (every text / JSON
// column, audit included), the log or any API response — even when the
// providers echo them back in error bodies.
func TestT1874SecretSentinelsNeverLeak(t *testing.T) {
	const (
		gatewayToken = "SENTINEL-gw-token-7c41e2"
		compressKey  = "SENTINEL-compress-key-9b03"
		csrfKey      = "SENTINEL-csrf-key-0123456789abcdef0123456789abcdef"
	)
	sentinels := []string{gatewayToken, compressKey, csrfKey}
	logs := captureLogs(t)
	t.Setenv("P18_COMPRESS_KEY", compressKey)
	t.Setenv("P18_CSRF_KEY", csrfKey)
	tokenFile := filepath.Join(t.TempDir(), "gateway.token")
	if err := os.WriteFile(tokenFile, []byte(gatewayToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := p17NewRigWith(t, p16Options{noBootstrap: true, config: func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.AuthSecretEnv, p.AuthSecretFile = "", tokenFile
		c.WorkerProviders["hermes"] = p
		cp := c.CompressionProviders["fake"]
		cp.APIKeyEnv = "P18_COMPRESS_KEY"
		c.CompressionProviders["fake"] = cp
		c.Web.CSRFKeyEnv = "P18_CSRF_KEY"
	}})
	// The gateway must accept the file token before bootstrap, which
	// checks the worker session.
	r.g.SetToken(gatewayToken)
	r.boot(t)
	ctx := context.Background()

	// Compression is forced and the provider rejects the key, echoing it.
	st := compression.DefaultSettings()
	st.Mode, st.MaxAttempts, st.BackoffBase, st.BackoffMax = compression.ModeAlways, 1, time.Millisecond, time.Millisecond
	if _, err := r.h.App.Compression().UpdateCompressionSettings(ctx, r.pid, st); err != nil {
		t.Fatal(err)
	}
	r.srv.Script(providertest.Reply{Status: 401, Body: `{"error":{"message":"invalid api key ` + compressKey + `"}}`})
	// The planner succeeds; the unit dispatch fails with a provider error
	// body echoing the Authorization header.
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.g.Script(hermestest.Behavior{Status: 500, ErrorCode: "internal", Message: "upstream rejected Authorization: Bearer " + gatewayToken})
	_, _ = r.eng().Run(ctx, r.execID, 100)
	if len(r.g.Messages()) == 0 {
		t.Fatal("setup: nothing was sent")
	}

	found := func(where, text string) {
		for _, s := range sentinels {
			if strings.Contains(text, s) {
				t.Errorf("secret %q found in %s", s, where)
			}
		}
	}
	// Prompts.
	for i, m := range r.g.Messages() {
		found("worker prompt "+string(rune('0'+i)), m.Content)
	}
	// Every text / JSON column of every table.
	conn := r.h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT c.table_name, c.column_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_name = c.table_name AND t.table_schema = 'public' AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = 'public' AND c.data_type IN ('text', 'character varying', 'json', 'jsonb') ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		_ = rows.Scan(&c.table, &c.column)
		cols = append(cols, c)
	}
	rows.Close()
	hits := func(s string) []string {
		var where []string
		for _, c := range cols {
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM "`+c.table+`" WHERE "`+c.column+`"::text LIKE '%' || $1 || '%'`, s).Scan(&n); err != nil {
				t.Fatalf("%s.%s: %v", c.table, c.column, err)
			}
			if n > 0 {
				where = append(where, c.table+"."+c.column)
			}
		}
		return where
	}
	for _, s := range sentinels {
		if w := hits(s); len(w) > 0 {
			t.Errorf("secret %q stored in %v", s, w)
		}
	}
	// Controls: the scenario exercised both provider error paths, and the
	// scan does find a value deliberately stored.
	if n := r.count(t, "SELECT count(*) FROM compression_attempts a JOIN compression_runs cr ON cr.id = a.run_id WHERE cr.project_id = '"+
		r.pid.String()+"' AND a.error_kind <> ''"); n == 0 {
		t.Fatal("control: no compression provider error was recorded")
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND status = 'FAILED'"); n == 0 {
		t.Fatal("control: no failed worker dispatch was recorded")
	}
	r.exec(t, "UPDATE projects SET description = 'CONTROL-SENTINEL-55aa' WHERE id = '"+r.pid.String()+"'")
	if w := hits("CONTROL-SENTINEL-55aa"); len(w) == 0 {
		t.Fatal("control: the scan cannot find a stored value")
	}
	// Every GET API response.
	ids := map[string]string{"project": r.pid.String(), "id": r.pid.String(), "execution": r.execID.String(),
		"phase": r.phases[1].String(), "phaseID": r.phases[1].String(),
		"dispatch": p18FirstID(t, r.h, "SELECT id::text FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' ORDER BY created_at DESC LIMIT 1"),
		"run":      p18FirstID(t, r.h, "SELECT id::text FROM compression_runs WHERE project_id = '"+r.pid.String()+"' LIMIT 1"),
		"prompt":   p18FirstID(t, r.h, "SELECT id::text FROM worker_prompts WHERE project_id = '"+r.pid.String()+"' LIMIT 1"),
	}
	for route := range r.h.App.Router().Routes() {
		if !strings.HasPrefix(route, "GET /api/v1/") || strings.HasSuffix(route, "/stream") {
			continue
		}
		path := regexp.MustCompile(`\{([a-zA-Z]+)\}`).ReplaceAllStringFunc(strings.TrimPrefix(route, "GET "), func(m string) string {
			if v := ids[strings.Trim(m, "{}")]; v != "" {
				return v
			}
			return "00000000-0000-0000-0000-000000000000"
		})
		_, body := r.b.api(http.MethodGet, path, nil)
		found("GET "+path, string(body))
	}
	// The log.
	found("the log", logs.String())
	if !strings.Contains(logs.String(), "[REDACTED]") && !strings.Contains(logs.String(), "worker") {
		t.Log("note: no redaction marker in the log (nothing secret was logged)")
	}
}
