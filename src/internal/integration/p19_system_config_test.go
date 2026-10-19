package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"overseer/internal/config"
	"overseer/internal/gateway/hermes/hermestest"
)

// The System and Models configuration categories edit the configuration
// file: providers apply at once (a changed worker endpoint takes the next
// request to the new gateway, no restart), a provider in use cannot be
// deleted, the database connection is tested for real and its settings
// apply at the next restart.
func TestP19SystemConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overseer.yaml")
	harnessConfigPath = path
	t.Cleanup(func() { harnessConfigPath = "" })
	r := p16NewRigWith(t, p16Options{statefile: true, noBootstrap: true, config: func(c *config.Config) {
		// The file holds the harness database (its password comes from
		// the environment, as in a deployment) and providers.
		t.Setenv("OVERSEER_DATABASE_PASSWORD", c.Database.Password)
		db := map[string]any{"host": c.Database.Host, "port": c.Database.Port, "name": c.Database.Name, "user": c.Database.User,
			"ssl_mode": c.Database.SSLMode}
		b, err := yaml.Marshal(map[string]any{"database": db, "worker_providers": c.WorkerProviders, "compression_providers": c.CompressionProviders})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append([]byte("# integration test configuration\n"), b...), 0o600); err != nil {
			t.Fatal(err)
		}
	}})
	base := r.h.BaseURL + "/api/v1/system/config"
	proj := r.h.BaseURL + "/api/v1/projects/" + r.pid.String()

	// Providers read from the file, with status and usage.
	code, raw, _ := p9HTTP(t, http.MethodGet, base+"/providers", "")
	if code != 200 || !strings.Contains(raw, `"name":"hermes"`) || !strings.Contains(raw, `"secret_configured":true`) || !strings.Contains(raw, `"name":"fake"`) {
		t.Fatalf("providers %d %s", code, raw)
	}

	// Live reload: the worker endpoint moves to a second gateway.
	if code, raw, _ := p9HTTP(t, http.MethodPost, proj+"/worker/session/new", ""); code != 200 && code != 201 {
		t.Fatalf("new session: %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, proj+"/worker/session/cwd", `{"path":"`+r.dir+`"}`); code != 200 {
		t.Fatalf("cwd on the first gateway: %d %s", code, raw)
	}
	if len(r.g.CDs()) == 0 {
		t.Fatal("the first gateway saw no request")
	}
	g2 := hermestest.New()
	t.Cleanup(g2.Close)
	g2.Token = p12Token
	body, _ := json.Marshal(map[string]any{"endpoint": g2.URL, "secret_env": "P12_GATEWAY_TOKEN", "connect_timeout": "2s"})
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/providers/worker/hermes", string(body)); code != 200 {
		t.Fatalf("update worker provider: %d %s", code, raw)
	}
	before := len(r.g.CDs())
	if code, raw, _ := p9HTTP(t, http.MethodPost, proj+"/worker/session/new", ""); code != 200 && code != 201 {
		t.Fatalf("new session: %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, proj+"/worker/session/cwd", `{"path":"`+r.dir+`"}`); code != 200 {
		t.Fatalf("cwd after the change: %d %s", code, raw)
	}
	if len(g2.CDs()) == 0 || len(r.g.CDs()) != before {
		t.Fatalf("after the change: new gateway %d requests, old gateway %d new requests", len(g2.CDs()), len(r.g.CDs())-before)
	}
	file, _ := os.ReadFile(path)
	if !strings.Contains(string(file), "# integration test configuration") || !strings.Contains(string(file), g2.URL) {
		t.Fatalf("configuration file:\n%s", file)
	}

	// Compressors: add, apply, delete; a provider in use stays.
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/providers/compression/cloud", `{"endpoint":"https://api.example.com/v1","secret_env":"CLOUD_KEY","timeout":"2m"}`); code != 200 {
		t.Fatalf("add compressor: %d %s", code, raw)
	}
	if _, raw, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/providers", ""); !strings.Contains(raw, `"cloud"`) {
		t.Fatalf("new compressor not live: %s", raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/providers/compression/Bad", `{"endpoint":"nope"}`); code != 400 || !strings.Contains(raw, "endpoint") {
		t.Fatalf("invalid compressor: %d %s", code, raw)
	}
	if code, _, _ := p9HTTP(t, http.MethodDelete, base+"/providers/compression/cloud", ""); code != 200 {
		t.Fatal("delete compressor")
	}
	if _, raw, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/providers", ""); strings.Contains(raw, `"cloud"`) {
		t.Fatalf("deleted compressor still live: %s", raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodDelete, base+"/providers/worker/hermes", ""); code != 409 || !strings.Contains(raw, "used by") {
		t.Fatalf("delete a provider in use: %d %s", code, raw)
	}

	// Database: a real connection test, then settings saved for the next
	// restart.
	_, raw, db := p9HTTP(t, http.MethodGet, base+"/database", "")
	eff, _ := db["effective"].(map[string]any)
	if eff == nil || eff["name"] != r.h.App.Config().Database.Name || db["restart_required"] != false || db["password_source"] != "environment" {
		t.Fatalf("database view %s", raw)
	}
	test, _ := json.Marshal(map[string]any{"host": eff["host"], "port": eff["port"], "name": eff["name"], "user": eff["user"], "ssl_mode": eff["ssl_mode"],
		"password": r.h.App.Config().Database.Password})
	if code, raw, res := p9HTTP(t, http.MethodPost, base+"/database/test", string(test)); code != 200 || res["ok"] != true || res["schema_version"].(float64) <= 0 {
		t.Fatalf("database test: %d %s", code, raw)
	}
	if code, raw, res := p9HTTP(t, http.MethodPost, base+"/database/test", `{"host":"127.0.0.1","port":1,"name":"x","user":"u","ssl_mode":"disable"}`); code != 200 || res["ok"] != false {
		t.Fatalf("database test against a closed port: %d %s", code, raw)
	}
	save, _ := json.Marshal(map[string]any{"host": eff["host"], "port": 6543, "name": eff["name"], "user": eff["user"], "ssl_mode": eff["ssl_mode"]})
	if code, raw, res := p9HTTP(t, http.MethodPut, base+"/database", string(save)); code != 200 || res["restart_required"] != true {
		t.Fatalf("save database: %d %s", code, raw)
	}
	if file, _ := os.ReadFile(path); !strings.Contains(string(file), "port: 6543") || !strings.Contains(string(file), g2.URL) {
		t.Fatalf("configuration file after the database save:\n%s", file)
	}
}
