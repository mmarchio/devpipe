package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// EffectiveProfile returns the profile, defaulting to development.
func (c Config) EffectiveProfile() string {
	if c.Profile == "" {
		return ProfileDevelopment
	}
	return c.Profile
}

// IsProduction reports the production profile.
func (c Config) IsProduction() bool { return c.EffectiveProfile() == ProfileProduction }

// LoopbackHost reports whether host binds only to the loopback interface.
func LoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validatePhase18 checks the Phase 18 sections and the profile rules
// (WBS 18.1 / 18.2 / 18.79 / 18.88 / 18.90 / 18.91). Every problem is
// reported (the caller joins them).
func (c Config) validatePhase18() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	switch c.EffectiveProfile() {
	case ProfileDevelopment, ProfileTest, ProfileProduction:
	default:
		add("config: profile %q not supported (development|test|production)", c.Profile)
	}
	if c.Faults != "" && c.EffectiveProfile() != ProfileTest {
		add("config: OVERSEER_FAULTS is only accepted in the test profile")
	}
	// Projects.
	if t := c.Projects.ContextThreshold; t < 0 || t > 1 {
		add("config: projects.default_context_threshold %v out of range (0 < x ≤ 1)", t)
	}
	// Pool / server limits.
	d := c.Database
	if d.MaxConns < 0 || d.MinConns < 0 || (d.MaxConns > 0 && d.MinConns > d.MaxConns) {
		add("config: database.max_conns / min_conns invalid (non-negative, min ≤ max)")
	}
	if d.MaxConnLifetime < 0 || d.MaxConnIdleTime < 0 {
		add("config: database pool durations must not be negative")
	}
	sv := c.Server
	if sv.ReadHeaderTimeout < 0 || sv.ReadTimeout < 0 || sv.WriteTimeout < 0 || sv.IdleTimeout < 0 || sv.MaxHeaderBytes < 0 {
		add("config: server timeouts / max_header_bytes must not be negative")
	}
	e := c.Engine
	if e.MaxConcurrentDispatches < 0 || e.MaxConcurrentValidations < 0 || e.MaxConcurrentCompressions < 0 || e.GitTimeout < 0 || e.StallThreshold < 0 || e.AlertInterval < 0 ||
		e.IntegrityScanInterval < 0 || e.IntegrityScanBatch < 0 {
		add("config: engine concurrency limits / git_timeout / stall_threshold must not be negative")
	}
	// Worker-reserved paths stay inside the project and never cover it.
	for _, rp := range e.WorkerReservedPaths {
		c := filepath.Clean(strings.TrimSpace(rp))
		if c == "." || c == "" || filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") || c == ".git" || strings.HasPrefix(c, ".git/") {
			add("config: engine.worker_reserved_paths: %q must be a relative path inside the project (not the project root or .git)", rp)
		}
	}
	// Secret references: env or file, not both (WBS 18.78).
	for _, name := range c.workerProviderNames() {
		p := c.WorkerProviders[name]
		if p.AuthSecretEnv != "" && p.AuthSecretFile != "" {
			add("config: worker_providers.%s: auth_secret_env and auth_secret_file are mutually exclusive", name)
		}
		if p.AuthSecretFile != "" && !strings.HasPrefix(p.AuthSecretFile, "/") {
			add("config: worker_providers.%s.auth_secret_file must be an absolute path", name)
		}
		errs = append(errs, c.endpointErrors("worker_providers."+name+".endpoint", p.Endpoint)...)
		if p.MaxConcurrent < 0 || p.Circuit.Failures < 0 || p.Circuit.Successes < 0 || p.Circuit.Window < 0 || p.Circuit.Cooldown < 0 {
			add("config: worker_providers.%s max_concurrent / circuit values must not be negative", name)
		}
	}
	for _, name := range c.compressionProviderNames() {
		p := c.CompressionProviders[name]
		if p.APIKeyEnv != "" && p.APIKeyFile != "" {
			add("config: compression_providers.%s: api_key_env and api_key_file are mutually exclusive", name)
		}
		if p.APIKeyFile != "" && !strings.HasPrefix(p.APIKeyFile, "/") {
			add("config: compression_providers.%s.api_key_file must be an absolute path", name)
		}
		errs = append(errs, c.endpointErrors("compression_providers."+name+".base_url", p.BaseURL)...)
		if p.Circuit.Failures < 0 || p.Circuit.Successes < 0 || p.Circuit.Window < 0 || p.Circuit.Cooldown < 0 {
			add("config: compression_providers.%s circuit values must not be negative", name)
		}
	}
	if d := c.Disk; d.WarnPercent < 0 || d.WarnPercent > 100 || d.CriticalPercent < 0 || d.CriticalPercent > 100 || d.Interval < 0 {
		add("config: disk thresholds must be within 0–100 and the interval not negative")
	} else if w, cr, _ := d.Effective(); w >= cr {
		add("config: disk.warn_percent must be below disk.critical_percent")
	}
	if g := c.GC; g.CacheRetention < 0 || g.PrecomputeRetention < 0 || g.ValidationLogRetention < 0 {
		add("config: gc retention periods must not be negative")
	}
	if c.IsProduction() {
		errs = append(errs, c.productionErrors()...)
	}
	return errs
}

// productionErrors are the production profile rules (WBS 18.1 / 18.79):
// no debug exposure, no development-only switches, explicit
// authentication acknowledged for a loopback-only deployment.
func (c Config) productionErrors() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Logging.Level == "debug" {
		add("config: production: logging.level debug is not allowed")
	}
	if c.Web.CookieInsecure {
		add("config: production: web.cookie_insecure is not allowed")
	}
	if c.Engine.RecoveryDisabled || c.Bootstrap.RecoveryDisabled {
		add("config: production: engine / bootstrap recovery must not be disabled")
	}
	if c.Web.AuthMode == "" {
		add("config: production: web.auth_mode is required")
	}
	if c.Web.CSRFKeyEnv == "" {
		add("config: production: web.csrf_key_env is required (stable CSRF key across restarts and instances)")
	}
	// Working directories must be confined (WBS 18.85, user decision U8).
	for _, name := range c.workerProviderNames() {
		if len(c.WorkerProviders[name].AllowedRoots) == 0 {
			add("config: production: worker_providers.%s.allowed_roots is required (project working directories must be confined)", name)
		}
	}
	if c.Web.AuthMode == "local" {
		if !c.Web.TrustedLocal {
			add("config: production: web.auth_mode local requires web.trusted_local: true (documented trusted-local deployment)")
		}
		if !LoopbackHost(c.Server.Host) {
			add("config: production: web.auth_mode local requires a loopback server.host, got %q", c.Server.Host)
		}
	}
	return errs
}

// EndpointAllowed checks a provider URL against the endpoint policy
// without DNS resolution (literal IPs and host allow-list). Production
// always forbids link-local / cloud-metadata targets (WBS 18.88).
func (c Config) EndpointAllowed(raw string) error {
	errs := c.endpointErrors("endpoint", raw)
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (c Config) endpointErrors(field, raw string) []error {
	pol := c.Security.ProviderEndpoints
	if !pol.Enabled && !c.IsProduction() {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil // shape errors are reported by the provider validation
	}
	host := u.Hostname()
	if err := CheckEndpointHost(host, pol, c.IsProduction()); err != nil {
		return []error{fmt.Errorf("config: %s: %w", field, err)}
	}
	return nil
}

// CheckEndpointHost applies the endpoint policy to a host name or IP.
// Without Enabled only the production link-local / metadata rule applies.
func CheckEndpointHost(host string, pol EndpointPolicy, production bool) error {
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return fmt.Errorf("endpoint %s is a link-local / metadata address", host)
	}
	if strings.EqualFold(host, "metadata.google.internal") {
		return fmt.Errorf("endpoint %s is a cloud metadata address", host)
	}
	if !pol.Enabled {
		return nil
	}
	if len(pol.AllowedHosts) > 0 {
		ok := false
		for _, h := range pol.AllowedHosts {
			ok = ok || strings.EqualFold(h, host)
		}
		if !ok {
			return fmt.Errorf("endpoint host %s is not in the allowed host list", host)
		}
	}
	if !pol.AllowPrivate {
		if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) || strings.EqualFold(host, "localhost") {
			return fmt.Errorf("endpoint %s is a private / loopback address", host)
		}
	}
	return nil
}

// CheckEndpointIP applies the policy to a resolved IP (dial-time check).
func CheckEndpointIP(ip net.IP, pol EndpointPolicy, production bool) error {
	return CheckEndpointHost(ip.String(), pol, production)
}

// PoolSettings returns the effective pool settings (defaults applied).
func (d DatabaseConfig) PoolSettings() (maxConns, minConns int, lifetime, idle time.Duration, appName string) {
	maxConns, minConns, lifetime, idle, appName = d.MaxConns, d.MinConns, d.MaxConnLifetime, d.MaxConnIdleTime, d.ApplicationName
	if maxConns == 0 {
		maxConns = 10
	}
	if lifetime == 0 {
		lifetime = time.Hour
	}
	if idle == 0 {
		idle = 30 * time.Minute
	}
	if appName == "" {
		appName = "overseer"
	}
	return
}

// Timeouts returns the effective HTTP server timeouts.
func (s ServerConfig) Timeouts() (readHeader, read, write, idle time.Duration, maxHeader int) {
	readHeader, read, write, idle, maxHeader = s.ReadHeaderTimeout, s.ReadTimeout, s.WriteTimeout, s.IdleTimeout, s.MaxHeaderBytes
	if readHeader == 0 {
		readHeader = 5 * time.Second
	}
	if read == 0 {
		read = 15 * time.Second
	}
	if write == 0 {
		write = 30 * time.Second
	}
	if idle == 0 {
		idle = 60 * time.Second
	}
	if maxHeader == 0 {
		maxHeader = 64 << 10
	}
	return
}

// ProjectContextThreshold returns the default context threshold.
func (c Config) ProjectContextThreshold() float64 {
	if c.Projects.ContextThreshold == 0 {
		return 0.8
	}
	return c.Projects.ContextThreshold
}

// ProductionErrors returns the production-profile rule violations of this
// configuration whatever its profile (production readiness, WBS 18.207).
func (c Config) ProductionErrors() []error { return c.productionErrors() }
