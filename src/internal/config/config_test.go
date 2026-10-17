package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T1.2.1 — Defaults load with no file and no env overrides.
func TestDefaultsLoad(t *testing.T) {
	for _, k := range envKeys() {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if c.Server.Host != "0.0.0.0" || c.Server.Port != 8080 {
		t.Errorf("server defaults = %+v", c.Server)
	}
	if c.Database.Host != "172.17.0.1" || c.Database.Port != 5432 ||
		c.Database.Name != "overseer" || c.Database.User != "overseer" ||
		c.Database.SSLMode != "disable" {
		t.Errorf("database defaults = %+v", c.Database)
	}
	if c.Logging.Level != "info" || c.Logging.Format != "json" {
		t.Errorf("logging defaults = %+v", c.Logging)
	}
	if c.Shutdown.Timeout.String() != "30s" {
		t.Errorf("shutdown default = %v", c.Shutdown.Timeout)
	}
}

// T1.2.2 — Environment overrides configuration.
func TestEnvOverridesConfiguration(t *testing.T) {
	t.Setenv("OVERSEER_SERVER_PORT", "9000")
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 9000 {
		t.Fatalf("Server.Port = %d, want 9000", c.Server.Port)
	}
}

// T1.2.3 — Database configuration loads from file and env.
func TestDatabaseConfigurationLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "database:\n  host: dbhost\n  port: 5433\n  name: otherdb\n  user: otheruser\n  password: secret\n  ssl_mode: require\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERSEER_DATABASE_PASSWORD", "envsecret")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Database
	if d.Host != "dbhost" || d.Port != 5433 || d.Name != "otherdb" || d.User != "otheruser" || d.SSLMode != "require" {
		t.Errorf("database = %+v", d)
	}
	if d.Password != "envsecret" {
		t.Errorf("password = %q, want env override", d.Password)
	}
}

// T1.2.4 — Invalid server port rejected.
func TestInvalidServerPortRejected(t *testing.T) {
	for _, port := range []string{"0", "-1", "65536", "notaport"} {
		t.Setenv("OVERSEER_SERVER_PORT", port)
		if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
			t.Errorf("port %s: expected validation error", port)
		}
		t.Setenv("OVERSEER_SERVER_PORT", "")
		os.Unsetenv("OVERSEER_SERVER_PORT")
	}
}

// T1.2.5 — Invalid shutdown duration rejected.
func TestInvalidDurationRejected(t *testing.T) {
	t.Setenv("OVERSEER_SHUTDOWN_TIMEOUT", "soon")
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("invalid duration: expected validation error")
	}
}

// T1.2.6 — Invalid logging level rejected.
func TestInvalidLogLevelRejected(t *testing.T) {
	t.Setenv("OVERSEER_LOGGING_LEVEL", "chatty")
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("invalid level: expected validation error")
	}
}

// T1.2.7 — Secrets excluded from formatted configuration.
func TestSecretsExcludedFromFormattedConfig(t *testing.T) {
	t.Setenv("OVERSEER_DATABASE_PASSWORD", "hunter2")
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.String(), "hunter2") {
		t.Errorf("formatted config leaks password: %s", c.String())
	}
	if !strings.Contains(c.String(), "password=") || !strings.Contains(c.String(), "REDACTED") {
		t.Errorf("formatted config should show a redacted marker: %s", c.String())
	}
}

// T1.2.8 — Validation occurs before startup: Load returns the validation
// error so no infrastructure is initialized downstream.
func TestValidationOccursBeforeStartup(t *testing.T) {
	t.Setenv("OVERSEER_SERVER_PORT", "99999")
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "port") {
		t.Errorf("error should identify the failing field: %v", err)
	}
}

func envKeys() []string {
	return []string{
		"OVERSEER_SERVER_HOST", "OVERSEER_SERVER_PORT",
		"OVERSEER_DATABASE_HOST", "OVERSEER_DATABASE_PORT", "OVERSEER_DATABASE_NAME",
		"OVERSEER_DATABASE_USER", "OVERSEER_DATABASE_PASSWORD", "OVERSEER_DATABASE_SSL_MODE",
		"OVERSEER_LOGGING_LEVEL", "OVERSEER_LOGGING_FORMAT",
		"OVERSEER_SHUTDOWN_TIMEOUT",
	}
}

// WBS 9.40/9.41 (config half): compression providers are configured by
// name; the API key is referenced by environment-variable NAME only, and
// the rendering never contains a key value.
func TestCompressionProvidersConfig(t *testing.T) {
	cfg := Default()
	cfg.CompressionProviders = map[string]CompressionProviderConfig{
		"local":  {Kind: CompressionKindOpenAICompatible, BaseURL: "http://127.0.0.1:8081/v1"},
		"remote": {Kind: CompressionKindOpenAICompatible, BaseURL: "https://api.example.com/v1", APIKeyEnv: "OVERSEER_TEST_COMPRESSION_KEY"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid providers rejected: %v", err)
	}
	t.Setenv("OVERSEER_TEST_COMPRESSION_KEY", "sk-SENTINEL-SECRET")
	if s := cfg.String(); strings.Contains(s, "sk-SENTINEL-SECRET") || !strings.Contains(s, "env:OVERSEER_TEST_COMPRESSION_KEY") || !strings.Contains(s, "key=none") {
		t.Fatalf("rendering: %s", s)
	}
	for name, bad := range map[string]CompressionProviderConfig{
		"kind": {Kind: "grpc", BaseURL: "http://x"},
		"url":  {Kind: CompressionKindOpenAICompatible, BaseURL: "ftp://x"},
		"env":  {Kind: CompressionKindOpenAICompatible, BaseURL: "http://x", APIKeyEnv: "sk-not-a-name!"},
	} {
		c := Default()
		c.CompressionProviders = map[string]CompressionProviderConfig{"p": bad}
		if c.Validate() == nil {
			t.Fatalf("%s: invalid provider accepted", name)
		}
	}
}

func TestCompressionProviderFromEnv(t *testing.T) {
	t.Setenv("OVERSEER_COMPRESSION_PROVIDER_NAME", "envp")
	t.Setenv("OVERSEER_COMPRESSION_PROVIDER_BASE_URL", "http://127.0.0.1:9/v1")
	t.Setenv("OVERSEER_COMPRESSION_PROVIDER_API_KEY_ENV", "MY_KEY")
	cfg, err := Load("/nonexistent/overseer.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.CompressionProviders["envp"]
	if !ok || p.Kind != CompressionKindOpenAICompatible || p.BaseURL != "http://127.0.0.1:9/v1" || p.APIKeyEnv != "MY_KEY" {
		t.Fatalf("env provider %+v", cfg.CompressionProviders)
	}
}

// P12-42 (WBS 12.3/12.94): worker providers from YAML and environment;
// the token is referenced by environment-variable name only and never
// rendered.
func TestP1242WorkerProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte(`
worker_providers:
  hermes:
    kind: hermes_gateway
    endpoint: https://gw.local:8443
    auth_secret_env: HERMES_GATEWAY_TOKEN
    connect_timeout: 3s
    reconnect: {max_attempts: 4, initial_delay: 100ms, max_delay: 2s}
    tls: {min_version: "1.3"}
    allowed_roots: [/repos]
`), 0o600)
	t.Setenv("HERMES_GATEWAY_TOKEN", "tok-CONFIG-SENTINEL")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.WorkerProviders["hermes"]
	if p.Endpoint != "https://gw.local:8443" || p.Reconnect.MaxAttempts != 4 || p.TLS.MinVersion != "1.3" || p.ConnectTimeout.String() != "3s" {
		t.Fatalf("worker provider = %+v", p)
	}
	if s := c.String(); strings.Contains(s, "tok-CONFIG-SENTINEL") || !strings.Contains(s, "worker_provider=hermes(hermes_gateway https://gw.local:8443 auth=env:HERMES_GATEWAY_TOKEN)") {
		t.Fatalf("T12.94.3 config line: %s", s)
	}

	t.Setenv("OVERSEER_WORKER_PROVIDER_NAME", "envgw")
	t.Setenv("OVERSEER_WORKER_PROVIDER_ENDPOINT", "http://127.0.0.1:9999")
	t.Setenv("OVERSEER_WORKER_PROVIDER_ALLOWED_ROOTS", "/repos:/work")
	c, err = Load(filepath.Join(dir, "missing.yaml"))
	if err != nil || c.WorkerProviders["envgw"].Kind != WorkerKindHermesGateway || len(c.WorkerProviders["envgw"].AllowedRoots) != 2 {
		t.Fatalf("env worker provider: %+v %v", c.WorkerProviders, err)
	}
	for _, bad := range []string{"kind: other\n    endpoint: http://x", "kind: hermes_gateway\n    endpoint: ftp://x",
		"kind: hermes_gateway\n    endpoint: http://x\n    tls: {min_version: \"1.3\"}", "kind: hermes_gateway\n    endpoint: http://x\n    auth_secret_env: \"not-a-var\""} {
		t.Setenv("OVERSEER_WORKER_PROVIDER_NAME", "")
		os.WriteFile(path, []byte("worker_providers:\n  w:\n    "+bad+"\n"), 0o600)
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid worker provider accepted: %s", bad)
		}
	}
}
