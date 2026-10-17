package worker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// WorkerConfig is the provider-independent worker configuration (WBS
// 12.3). Provider-specific settings stay opaque in ProviderConfig.
type WorkerConfig struct {
	Provider WorkerProvider

	Model string

	RequestTimeout time.Duration
	SessionTimeout time.Duration

	ContextWindow int

	ProviderConfig json.RawMessage
}

// ConfigError lists every detected configuration problem (WBS 12.96.1).
type ConfigError struct {
	Problems []string
}

func (e *ConfigError) Error() string {
	return "worker: invalid configuration: " + strings.Join(e.Problems, "; ")
}

// Validate checks the generic fields; it reports all problems at once.
func (c WorkerConfig) Validate() error {
	var p []string
	if c.Provider == "" {
		p = append(p, "provider not configured")
	} else if !c.Provider.Known() {
		p = append(p, fmt.Sprintf("unknown provider %q", c.Provider))
	}
	if c.RequestTimeout <= 0 {
		p = append(p, "request timeout must be positive")
	}
	if c.SessionTimeout < 0 {
		p = append(p, "session timeout must not be negative")
	}
	if c.ContextWindow <= 0 {
		p = append(p, "context window must be positive")
	}
	if len(c.ProviderConfig) > 0 && !json.Valid(c.ProviderConfig) {
		p = append(p, "provider configuration is not valid JSON")
	}
	if len(p) > 0 {
		return &ConfigError{Problems: p}
	}
	return nil
}

// ReconnectPolicy bounds transport reconnection (WBS 12.40). Reconnecting
// never implies resending a request.
type ReconnectPolicy struct {
	MaxAttempts int

	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// DefaultReconnectPolicy is used when none is configured.
var DefaultReconnectPolicy = ReconnectPolicy{MaxAttempts: 3, InitialDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second}

// Validate checks the policy.
func (p ReconnectPolicy) Validate() error {
	if p.MaxAttempts < 0 || p.InitialDelay < 0 || p.MaxDelay < 0 || (p.MaxDelay > 0 && p.InitialDelay > p.MaxDelay) {
		return fmt.Errorf("worker: invalid reconnect policy")
	}
	return nil
}

// Delay returns the capped exponential backoff before attempt n (1-based).
func (p ReconnectPolicy) Delay(n int) time.Duration {
	if n <= 1 {
		return p.InitialDelay
	}
	d := p.InitialDelay
	for i := 1; i < n; i++ {
		d *= 2
		if p.MaxDelay > 0 && d >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return d
}

// ValidateConfiguration is the pre-execution configuration check (WBS
// 12.96/12.97): generic fields, credential resolvability, model, and the
// capabilities the workflow requires. caps may be nil when the worker has
// not been contacted yet. Every problem is returned together.
func ValidateConfiguration(c WorkerConfig, caps *WorkerCapabilities, req WorkflowRequirements, providerErr error, credentialsResolvable bool) error {
	var p []string
	if err := c.Validate(); err != nil {
		p = append(p, err.(*ConfigError).Problems...)
	}
	if c.Model == "" {
		p = append(p, "model not configured")
	}
	if providerErr != nil {
		p = append(p, providerErr.Error())
	}
	if !credentialsResolvable {
		p = append(p, "credentials not resolvable")
	}
	if caps != nil {
		if req.NewSession && !caps.NewSession {
			p = append(p, "worker cannot create a new session (required by workflow)")
		}
		if req.ChangeDirectory && !caps.ChangeDirectory {
			p = append(p, "worker cannot change working directory (required by workflow)")
		}
		if req.ContextTelemetry && !caps.ContextTelemetry {
			p = append(p, "worker does not report context usage (required by workflow)")
		}
	}
	if len(p) == 0 {
		return nil
	}
	sort.Strings(p)
	return &ConfigError{Problems: p}
}
