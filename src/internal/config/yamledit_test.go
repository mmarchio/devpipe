package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// EditFile changes only the keys it is told to, keeps comments and every
// other key, keeps a backup, and the result loads.
func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overseer.yaml")
	orig := `# Overseer configuration.
worker_providers:
  hermes:
    kind: hermes_gateway
    # the gateway
    endpoint: http://old:8642
    auth_secret_env: API_SERVER_KEY
    circuit:
      failures: 7
compression_providers:
  local:
    kind: openai_compatible
    base_url: http://llm:8080/v1
`
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	err := EditFile(path, func(root *yaml.Node) error {
		h := MappingAt(root, "worker_providers", "hermes")
		SetString(h, "endpoint", "http://new:8642")
		SetStrings(h, "models", []string{"m1", "m2"})
		SetString(h, "auth_secret_env", "")
		SetString(h, "auth_secret_file", "/run/secrets/hermes")
		db := MappingAt(root, "database")
		SetString(db, "host", "db.internal")
		SetInt(db, "port", 6543)
		c := MappingAt(root, "compression_providers", "cloud")
		SetString(c, "kind", "openai_compatible")
		SetString(c, "base_url", "https://api.example.com/v1")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"# Overseer configuration.", "# the gateway", "endpoint: http://new:8642", "models: [m1, m2]",
		"auth_secret_file: /run/secrets/hermes", "failures: 7", "base_url: http://llm:8080/v1", "host: db.internal", "port: 6543",
		"base_url: https://api.example.com/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("edited file lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "API_SERVER_KEY") {
		t.Errorf("removed key still present:\n%s", got)
	}
	if bak, _ := os.ReadFile(path + ".bak"); string(bak) != orig {
		t.Errorf("backup %q", bak)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", st.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("edited file does not load: %v", err)
	}
	if p := cfg.WorkerProviders["hermes"]; p.Endpoint != "http://new:8642" || len(p.Models) != 2 || p.Circuit.Failures != 7 {
		t.Fatalf("loaded worker provider %+v", p)
	}
	if cfg.Database.Host != "db.internal" || cfg.Database.Port != 6543 || len(cfg.CompressionProviders) != 2 {
		t.Fatalf("loaded %+v %+v", cfg.Database, cfg.CompressionProviders)
	}
}
