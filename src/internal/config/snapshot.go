package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Snapshot is the redacted, hashed application configuration persisted at
// startup (WBS 18.3). Secrets never appear: only whether a password is set
// and the *names* of secret environment variables / files.
type Snapshot struct {
	Canonical []byte
	Hash      string
	// Sections maps a section name to the hash of its content, so drift
	// can be reported per area (WBS 18.4).
	Sections map[string]string
}

// DriftRelevantSections are the areas whose change may affect an active
// execution (WBS 18.4).
var DriftRelevantSections = []string{"worker", "compression", "repository", "resource_limits"}

func hashJSON(v any) (string, []byte) {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b
}

// sections returns the redacted configuration grouped by area.
func (c Config) sections() map[string]any {
	workers := map[string]any{}
	roots := map[string][]string{}
	for _, n := range c.workerProviderNames() {
		p := c.WorkerProviders[n]
		workers[n] = map[string]any{"kind": p.Kind, "endpoint": p.Endpoint, "auth_secret_env": p.AuthSecretEnv,
			"auth_secret_file": p.AuthSecretFile, "connect_timeout": p.ConnectTimeout.String(), "session_timeout": p.SessionTimeout.String(),
			"max_response_bytes": p.MaxResponseBytes, "reconnect": p.Reconnect, "tls": p.TLS}
		r := append([]string(nil), p.AllowedRoots...)
		sort.Strings(r)
		roots[n] = r
	}
	comps := map[string]any{}
	for _, n := range c.compressionProviderNames() {
		p := c.CompressionProviders[n]
		comps[n] = map[string]any{"kind": p.Kind, "base_url": p.BaseURL, "api_key_env": p.APIKeyEnv, "api_key_file": p.APIKeyFile,
			"timeout": p.Timeout.String(), "max_output_tokens": p.MaxOutputTokens}
	}
	maxC, minC, life, idle, app := c.Database.PoolSettings()
	rh, rd, wr, id, mh := c.Server.Timeouts()
	return map[string]any{
		"profile": c.EffectiveProfile(),
		"server":  map[string]any{"host": c.Server.Host, "port": c.Server.Port},
		"database": map[string]any{"host": c.Database.Host, "port": c.Database.Port, "name": c.Database.Name, "user": c.Database.User,
			"ssl_mode": c.Database.SSLMode, "password_set": c.Database.Password != ""},
		"logging": map[string]any{"level": c.Logging.Level, "format": c.Logging.Format},
		"backup":  map[string]any{"dir": c.Backup.Dir},
		"gc": map[string]any{"cache_retention": c.GC.CacheRetention.String(), "precompute_retention": c.GC.PrecomputeRetention.String(),
			"validation_log_retention": c.GC.ValidationLogRetention.String()},
		"disk":        map[string]any{"warn_percent": c.Disk.WarnPercent, "critical_percent": c.Disk.CriticalPercent, "interval": c.Disk.Interval.String()},
		"worker":      workers,
		"compression": comps,
		"repository":  roots,
		"resource_limits": map[string]any{"db_max_conns": maxC, "db_min_conns": minC, "db_conn_lifetime": life.String(), "db_conn_idle": idle.String(),
			"db_application_name": app, "http_read_header_timeout": rh.String(), "http_read_timeout": rd.String(),
			"http_write_timeout": wr.String(), "http_idle_timeout": id.String(), "http_max_header_bytes": mh, "web_max_body": c.Web.MaxBody,
			"web_max_document_body": c.Web.MaxDocumentBody, "web_rate_per_second": c.Web.RatePerSecond, "web_rate_burst": c.Web.RateBurst,
			"engine_workers": c.Engine.Workers, "continuation_workers": c.Continuation.Workers, "shutdown_timeout": c.Shutdown.Timeout.String()},
		"engine":       map[string]any{"scheduler_disabled": c.Engine.SchedulerDisabled, "recovery_disabled": c.Engine.RecoveryDisabled, "interval": c.Engine.Interval.String()},
		"continuation": map[string]any{"disabled": c.Continuation.Disabled, "poll_interval": c.Continuation.PollInterval.String()},
		"bootstrap":    map[string]any{"recovery_disabled": c.Bootstrap.RecoveryDisabled},
		"mcp": map[string]any{"enabled": c.MCP.Enabled, "addr": c.MCP.Effective().Addr, "auth_secret_env": c.MCP.AuthSecretEnv,
			"token_ttl": c.MCP.Effective().TokenTTL.String(), "max_response_bytes": c.MCP.Effective().MaxResponseBytes,
			"request_timeout": c.MCP.Effective().RequestTimeout.String()},
		"security": map[string]any{"auth_mode": c.Web.AuthMode, "trusted_local": c.Web.TrustedLocal, "cookie_insecure": c.Web.CookieInsecure,
			"csrf_key_env": c.Web.CSRFKeyEnv, "session_ttl": c.Web.SessionTTL.String(), "session_rotate": c.Web.SessionRotate.String(),
			"provider_endpoints": c.Security.ProviderEndpoints},
		"projects": map[string]any{"default_context_threshold": c.ProjectContextThreshold()},
	}
}

// Snapshot builds the redacted canonical configuration and its hashes.
func (c Config) Snapshot() Snapshot {
	secs := c.sections()
	out := Snapshot{Sections: map[string]string{}}
	for k, v := range secs {
		out.Sections[k], _ = hashJSON(v)
	}
	out.Hash, out.Canonical = hashJSON(secs)
	return out
}

// ChangedSections lists the sections whose hashes differ (sorted).
func ChangedSections(prev, cur map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for k, v := range cur {
		seen[k] = true
		if prev[k] != v {
			out = append(out, k)
		}
	}
	for k := range prev {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
