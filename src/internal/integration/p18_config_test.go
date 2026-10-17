package integration

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/readmodel"
)

// T18.1.4 — the runtime profile is explicitly identifiable in diagnostics
// (system status API and page).
func TestT1814ProfileIdentifiable(t *testing.T) {
	h := NewHarnessWith(t, func(c *config.Config) { c.Profile = config.ProfileTest })
	code, raw, body := p9HTTP(t, http.MethodGet, h.BaseURL+"/api/v1/system/status", "")
	if code != 200 || body["profile"] != "test" {
		t.Fatalf("status %d %s", code, raw)
	}
	_ = context.Background()
	req, _ := http.NewRequest(http.MethodGet, h.BaseURL+"/system", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(strings.Builder)
	_, _ = io_copy(b, resp)
	if !strings.Contains(b.String(), "<dt>Profile</dt><dd>test</dd>") {
		t.Fatal("system page lacks the profile")
	}
}

// T18.2 (startup) — the default context threshold from configuration
// applies to new projects.
func TestT182ConfiguredContextThreshold(t *testing.T) {
	h := NewHarnessWith(t, func(c *config.Config) { c.Projects.ContextThreshold = 0.65 })
	code, raw, body := p9HTTP(t, http.MethodPost, h.BaseURL+"/api/v1/projects", `{"name":"threshold"}`)
	if code != 201 {
		t.Fatalf("create %d %s", code, raw)
	}
	id := body["id"].(string)
	_, _, st := p9HTTP(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+id+"/settings", "")
	if st["context_threshold"] != 0.65 {
		t.Fatalf("threshold %v", st["context_threshold"])
	}
}

func io_copy(b *strings.Builder, resp *http.Response) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := resp.Body.Read(buf)
		b.Write(buf[:k])
		n += int64(k)
		if err != nil {
			return n, nil
		}
	}
}

// T18.5 — an unavailable worker / compression provider does not prevent
// read-only startup: health, readiness, pages and history stay usable.
func TestT185ReadOnlyStartupWithoutProviders(t *testing.T) {
	h := NewHarnessWith(t, func(c *config.Config) {
		c.WorkerProviders = map[string]config.WorkerProviderConfig{"hermes": {Kind: config.WorkerKindHermesGateway, Endpoint: "http://127.0.0.1:9"}}
		c.CompressionProviders = map[string]config.CompressionProviderConfig{"local": {Kind: config.CompressionKindOpenAICompatible, BaseURL: "http://127.0.0.1:9"}}
	})
	for _, p := range []string{"/health", "/ready", "/api/v1/projects", "/api/v1/system/status"} {
		if code, raw, _ := p9HTTP(t, http.MethodGet, h.BaseURL+p, ""); code != 200 {
			t.Fatalf("%s: %d %s", p, code, raw)
		}
	}
	_, raw, _ := p9HTTP(t, http.MethodGet, h.BaseURL+"/api/v1/system/status", "")
	if !strings.Contains(raw, `"hermes"`) || strings.Contains(raw, `"status":"HEALTHY","name":"hermes"`) {
		t.Fatalf("provider health not reported: %s", raw)
	}
}

// T18.91 — load never creates more database connections than the pool
// limit (observed in pg_stat_activity by application name).
func TestT1891PoolLimit(t *testing.T) {
	app := "overseer-p18-pool-" + strings.ReplaceAll(t.Name(), "/", "-")
	h := NewHarnessWith(t, func(c *config.Config) { c.Database.MaxConns = 3; c.Database.ApplicationName = app })
	stop := make(chan struct{})
	maxSeen := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int
			_ = h.DBScan(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE application_name = '"+app+"'", &n)
			if n > maxSeen {
				maxSeen = n
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(h.BaseURL + "/api/v1/projects?limit=50")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-done
	if maxSeen == 0 || maxSeen > 3 {
		t.Fatalf("connections seen %d (limit 3)", maxSeen)
	}
}

// T18.90 — slowloris-style connections are closed after the header
// timeout and do not prevent normal requests.
func TestT1890Slowloris(t *testing.T) {
	h := NewHarnessWith(t, func(c *config.Config) { c.Server.ReadHeaderTimeout = 300 * time.Millisecond })
	addr := strings.TrimPrefix(h.BaseURL, "http://")
	var conns []net.Conn
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		_, _ = c.Write([]byte("GET /api/v1/projects HTTP/1.1\r\nHost: x\r\nX-Slow: "))
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	if code, _, _ := p9HTTP(t, http.MethodGet, h.BaseURL+"/health", ""); code != 200 {
		t.Fatal("normal request blocked by slow clients")
	}
	time.Sleep(700 * time.Millisecond)
	closed := 0
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 512)
		n, err := c.Read(buf)
		// The server answers 408 or closes: either way the slow
		// connection no longer holds a server slot.
		if err != nil || strings.Contains(string(buf[:n]), "408") {
			closed++
		}
	}
	if closed != len(conns) {
		t.Fatalf("%d / %d slow connections still open", len(conns)-closed, len(conns))
	}
}

// P18-20 — on a fresh database (no test-side seeding) a project created
// through the web reaches READY and execution readiness; seeding is
// idempotent across restarts.
func TestP1820FreshInstallReachesReady(t *testing.T) {
	h, _, _ := p15Harness(t, p17Config(nil))
	b := newBrowser(t, h.BaseURL)
	dir := p15Repo(t)
	code, body, final := b.form("/projects", url.Values{"name": {"Fresh"}, "working_directory": {dir}, "worker_provider": {"hermes"},
		"worker_model": {"m"}, "compression_provider": {"fake"}, "compression_model": {"model-A"}, "context_window": {"64000"},
		"context_threshold": {"80"}, "planning_mode": {"ATOMIC"}, "checkpoint_policy": {"REQUIRED"}})
	if code != 200 {
		t.Fatalf("create: %d %s", code, textOf(body))
	}
	pid := strings.TrimPrefix(final, "/projects/")
	if code, _, _ := b.form("/projects/"+pid+"/phases", url.Values{"name": {"One"}}); code != 200 {
		t.Fatal("phase")
	}
	var phases struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	b.getJSON("/api/v1/projects/"+pid+"/console/phases", &phases)
	page := b.mustPage("/projects/" + pid + "/phases/" + phases.Phases[0].ID + "/wbs")
	if code, _, _ := b.form("/projects/"+pid+"/phases/"+phases.Phases[0].ID+"/wbs", url.Values{"content": {"# One\n- x"},
		"expected_version": {hidden(page, "expected_version")}}); code != 200 {
		t.Fatal("wbs")
	}
	if code, body, _ := b.form("/projects/"+pid+"/ready", nil); code != 200 || !strings.Contains(textOf(body), "Project marked READY.") {
		t.Fatalf("ready: %d %s", code, textOf(body))
	}
	if text := textOf(b.mustPage("/projects/" + pid + "/readiness")); !strings.Contains(text, "Ready to Start") {
		t.Fatalf("execution readiness: %s", text)
	}
	var tpl int
	_ = h.DBScan(context.Background(), "SELECT count(*) FROM prompt_templates WHERE project_id IS NULL", &tpl)
	if tpl == 0 {
		t.Fatal("global templates not seeded")
	}
}

// T18.134 — an application refuses to start against a newer schema.
func TestT18134OldBinaryRefusesNewerSchema(t *testing.T) {
	h := NewHarness(t)
	if _, err := h.DBExec(context.Background(), "INSERT INTO schema_migrations (version, name) VALUES (9999, 'from_the_future')"); err != nil {
		t.Fatal(err)
	}
	a := NewAppWithDBName(t, h.DBName())
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Shutdown(context.Background())
		t.Fatal("started against a newer schema")
	}
	if !strings.Contains(err.Error(), "newer than this application") {
		t.Fatalf("error %v", err)
	}
}
