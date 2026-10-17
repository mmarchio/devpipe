package config

import (
	"errors"
	"strings"
	"testing"
)

func prodConfig() Config {
	c := Default()
	c.Profile = ProfileProduction
	c.Server.Host = "127.0.0.1"
	c.Web.AuthMode = "local"
	c.Web.TrustedLocal = true
	c.Web.CSRFKeyEnv = "OVERSEER_CSRF_KEY"
	return c
}

// T18.1.1 — the production profile disables debug exposure.
func TestT1811ProductionDisablesDebug(t *testing.T) {
	c := prodConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
	c.Logging.Level = "debug"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "logging.level debug") {
		t.Fatalf("debug accepted: %v", err)
	}
}

// T18.1.2 — missing required production settings fail startup.
func TestT1812RequiredProductionSettings(t *testing.T) {
	c := Default()
	c.Profile = ProfileProduction
	err := c.Validate()
	for _, want := range []string{"web.auth_mode is required", "web.csrf_key_env is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
}

// T18.1.3 — development defaults cannot silently leak into production.
func TestT1813NoDevelopmentLeak(t *testing.T) {
	c := prodConfig()
	c.Web.CookieInsecure = true
	c.Engine.RecoveryDisabled = true
	c.Faults = "fs_write:crash"
	c.Server.Host = Default().Server.Host // 0.0.0.0 with local auth
	err := c.Validate()
	for _, want := range []string{"cookie_insecure", "recovery must not be disabled", "OVERSEER_FAULTS", "loopback server.host"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	d := Default()
	d.Web.CookieInsecure = true
	if d.Validate() != nil || d.EffectiveProfile() != ProfileDevelopment {
		t.Fatal("development profile must allow development settings")
	}
	d.Faults = "fs_write:crash"
	if d.Validate() == nil {
		t.Fatal("faults accepted outside the test profile")
	}
	d.Profile = ProfileTest
	if d.Validate() != nil {
		t.Fatal("faults rejected in the test profile")
	}
	d.Profile = "staging"
	if d.Validate() == nil {
		t.Fatal("unknown profile accepted")
	}
}

// T18.2 — an invalid worker endpoint, invalid DB config and invalid
// context threshold are all reported together.
func TestT182AllErrorsReported(t *testing.T) {
	c := Default()
	c.Database.Port = 0
	c.Database.MaxConns = -1
	c.Projects.ContextThreshold = 1.5
	c.WorkerProviders = map[string]WorkerProviderConfig{"hermes": {Kind: WorkerKindHermesGateway, Endpoint: "ftp://x"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"database.port", "database.max_conns", "default_context_threshold", "worker_providers.hermes.endpoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) < 4 {
		t.Fatal("errors not joined individually")
	}
}

// T18.79 — unsafe public production configuration is rejected.
func TestT1879ProductionAuth(t *testing.T) {
	c := prodConfig()
	c.Web.TrustedLocal = false
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "trusted_local") {
		t.Fatalf("%v", err)
	}
	for _, h := range []string{"127.0.0.1", "::1", "localhost", "127.0.0.2"} {
		if !LoopbackHost(h) {
			t.Fatalf("%s not loopback", h)
		}
	}
	if LoopbackHost("0.0.0.0") || LoopbackHost("10.0.0.1") {
		t.Fatal("public host treated as loopback")
	}
}

// T18.88 — disallowed endpoints are rejected where the policy is enabled;
// production always rejects metadata / link-local targets.
func TestT1888EndpointPolicy(t *testing.T) {
	c := Default()
	c.WorkerProviders = map[string]WorkerProviderConfig{"h": {Kind: WorkerKindHermesGateway, Endpoint: "http://169.254.169.254/x"}}
	if c.Validate() != nil {
		t.Fatal("policy applied while disabled in development")
	}
	p := prodConfig()
	p.WorkerProviders = c.WorkerProviders
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("production metadata endpoint: %v", err)
	}
	c.Security.ProviderEndpoints = EndpointPolicy{Enabled: true}
	c.WorkerProviders = map[string]WorkerProviderConfig{"h": {Kind: WorkerKindHermesGateway, Endpoint: "http://10.1.2.3:8642"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("private endpoint: %v", err)
	}
	c.Security.ProviderEndpoints.AllowPrivate = true
	if c.Validate() != nil {
		t.Fatal("allow_private ignored")
	}
	c.Security.ProviderEndpoints.AllowedHosts = []string{"gateway.internal"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "allowed host list") {
		t.Fatalf("allow-list: %v", err)
	}
	if CheckEndpointHost("gateway.internal", c.Security.ProviderEndpoints, false) != nil {
		t.Fatal("allowed host rejected")
	}
}

// T18.78 (config part) — a secret is referenced by env or file, never both.
func TestT1878SecretReferences(t *testing.T) {
	c := Default()
	c.WorkerProviders = map[string]WorkerProviderConfig{"h": {Kind: WorkerKindHermesGateway, Endpoint: "http://h:1", AuthSecretEnv: "T", AuthSecretFile: "/run/t"}}
	c.CompressionProviders = map[string]CompressionProviderConfig{"c": {Kind: CompressionKindOpenAICompatible, BaseURL: "http://c:1", APIKeyFile: "relative"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") || !strings.Contains(err.Error(), "api_key_file must be an absolute path") {
		t.Fatalf("%v", err)
	}
}

// T18.90 / T18.91 (config part) — effective pool and server limits.
func TestPoolAndTimeoutDefaults(t *testing.T) {
	max, min, life, idle, app := DatabaseConfig{}.PoolSettings()
	if max != 10 || min != 0 || life == 0 || idle == 0 || app != "overseer" {
		t.Fatal("pool defaults")
	}
	rh, r, w, i, mh := ServerConfig{}.Timeouts()
	if rh != 5e9 || r == 0 || w == 0 || i == 0 || mh == 0 {
		t.Fatal("timeouts")
	}
}

// T18.3 — configuration changes are detectable between restarts without
// revealing credentials.
func TestT183ConfigSnapshot(t *testing.T) {
	a := Default()
	a.Database.Password = "s3cret-DB-pass"
	a.WorkerProviders = map[string]WorkerProviderConfig{"h": {Kind: WorkerKindHermesGateway, Endpoint: "http://h:1", AuthSecretEnv: "HERMES_TOKEN"}}
	s1 := a.Snapshot()
	if strings.Contains(string(s1.Canonical), "s3cret") || !strings.Contains(string(s1.Canonical), "HERMES_TOKEN") {
		t.Fatalf("snapshot leaks / misses refs: %s", s1.Canonical)
	}
	if a.Snapshot().Hash != s1.Hash {
		t.Fatal("snapshot not deterministic")
	}
	b := a
	b.Database.Password = "rotated"
	if b.Snapshot().Hash != s1.Hash {
		t.Fatal("password rotation changed the hash (value leaked into the snapshot)")
	}
	b.WorkerProviders = map[string]WorkerProviderConfig{"h": {Kind: WorkerKindHermesGateway, Endpoint: "http://h:2", AuthSecretEnv: "HERMES_TOKEN"}}
	b.Database.MaxConns = 20
	if got := ChangedSections(s1.Sections, b.Snapshot().Sections); strings.Join(got, ",") != "resource_limits,worker" {
		t.Fatalf("changed %v", got)
	}
}

// T18.94 — compression concurrency and circuit settings are validated.
func TestT1894CompressionLimitsValidated(t *testing.T) {
	c := Default()
	c.Engine.MaxConcurrentCompressions = 2
	c.CompressionProviders = map[string]CompressionProviderConfig{"local": {Kind: CompressionKindOpenAICompatible,
		BaseURL: "http://127.0.0.1:8080/v1", Circuit: CircuitConfig{Failures: 3}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
	c.Engine.MaxConcurrentCompressions = -1
	p := c.CompressionProviders["local"]
	p.Circuit.Cooldown = -1
	c.CompressionProviders["local"] = p
	err := c.Validate()
	for _, want := range []string{"engine concurrency limits", "compression_providers.local circuit"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
}

// Worker-reserved paths default to .hermes and must stay inside the
// project (never the root or .git).
func TestWorkerReservedPaths(t *testing.T) {
	c := Default()
	if got := c.Engine.ReservedPaths(); len(got) != 1 || got[0] != ".hermes" {
		t.Fatalf("default %v", got)
	}
	c.Engine.WorkerReservedPaths = []string{".hermes/", "tools/agent-notes"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid paths rejected: %v", err)
	}
	if got := c.Engine.ReservedPaths(); got[0] != ".hermes" {
		t.Fatalf("not cleaned: %v", got)
	}
	for _, bad := range []string{".", "/abs", "../outside", ".git", ".git/hooks", ""} {
		c.Engine.WorkerReservedPaths = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "worker_reserved_paths") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

// T18.85 / U8 — production requires every worker provider to confine
// working directories with allowed roots.
func TestT1885ProductionRequiresAllowedRoots(t *testing.T) {
	c := prodConfig()
	c.WorkerProviders = map[string]WorkerProviderConfig{"hermes": {Kind: WorkerKindHermesGateway, Endpoint: "http://127.0.0.1:9000"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "worker_providers.hermes.allowed_roots is required") {
		t.Fatalf("unconfined production provider accepted: %v", err)
	}
	p := c.WorkerProviders["hermes"]
	p.AllowedRoots = []string{"/srv/projects"}
	c.WorkerProviders["hermes"] = p
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "allowed_roots") {
		t.Fatalf("confined provider rejected: %v", err)
	}
	d := Default()
	d.WorkerProviders = map[string]WorkerProviderConfig{"hermes": {Kind: WorkerKindHermesGateway, Endpoint: "http://127.0.0.1:9000"}}
	if err := d.Validate(); err != nil && strings.Contains(err.Error(), "allowed_roots") {
		t.Fatalf("development requires allowed roots: %v", err)
	}
}
