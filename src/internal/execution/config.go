package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/continuation"
	"overseer/internal/hardening"
	"overseer/internal/role"
	"overseer/internal/state"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// PromptTemplateSnapshot pins one template version (WBS 15.35).
type PromptTemplateSnapshot struct {
	Key         string    `json:"key"`
	TemplateID  uuid.UUID `json:"template_id"`
	Version     int       `json:"version"`
	ContentHash string    `json:"content_hash"`
}

// CompressionConfigurationSnapshot captures Phase 9 configuration
// (WBS 15.36). APIKeyEnv is a secret REFERENCE (env-var name), never a value.
type CompressionConfigurationSnapshot struct {
	Provider   string               `json:"provider"`
	Model      string               `json:"model"`
	Kind       string               `json:"kind,omitempty"`
	BaseURL    string               `json:"base_url,omitempty"`
	APIKeyEnv  string               `json:"api_key_env,omitempty"`
	Settings   compression.Settings `json:"settings"`
	ConfigHash string               `json:"config_hash"`
}

// WorkerConfigurationSnapshot captures Phase 12 configuration (WBS 15.37).
// AuthSecretEnv is a secret REFERENCE; Endpoint has userinfo stripped.
type WorkerConfigurationSnapshot struct {
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`
	Kind             string          `json:"kind,omitempty"`
	Endpoint         string          `json:"endpoint,omitempty"`
	AuthSecretEnv    string          `json:"auth_secret_env,omitempty"`
	ConnectTimeout   time.Duration   `json:"connect_timeout"`
	SessionTimeout   time.Duration   `json:"session_timeout"`
	WorkerTimeout    time.Duration   `json:"worker_timeout"`
	MaxRetries       int             `json:"max_retries"`
	MaxResponseBytes int64           `json:"max_response_bytes"`
	AllowedRoots     []string        `json:"allowed_roots"`
	Capabilities     []string        `json:"capabilities"`
	Settings         worker.Settings `json:"settings"`
}

// ResponseProtocolSnapshot captures Phase 13 configuration (WBS 15.38).
type ResponseProtocolSnapshot struct {
	ProtocolID  *uuid.UUID            `json:"protocol_id"`
	Version     int                   `json:"version"`
	ContentHash string                `json:"content_hash"`
	Settings    workerresult.Settings `json:"settings"`
}

// ContinuationConfigurationSnapshot captures Phase 14 configuration
// (WBS 15.39).
type ContinuationConfigurationSnapshot struct {
	ContextThreshold float64               `json:"context_threshold"`
	ContextWindow    int                   `json:"context_window"`
	Settings         continuation.Settings `json:"settings"`
}

// WBSSnapshot binds the current phase's WBS (WBS 15.25).
type WBSSnapshot struct {
	PhaseID uuid.UUID `json:"phase_id"`
	Version int       `json:"version"`
	Hash    string    `json:"hash"`
}

// ConfigurationContent is the hashed configuration aggregate (WBS 15.40).
type ConfigurationContent struct {
	ProjectVersion   int                               `json:"project_version"`
	WorkingDirectory string                            `json:"working_directory"`
	WBS              WBSSnapshot                       `json:"wbs"`
	StateMachine     state.StateMachineSnapshot        `json:"state_machine"`
	Roles            []role.RoleSnapshot               `json:"roles"`
	PromptTemplates  []PromptTemplateSnapshot          `json:"prompt_templates"`
	Compression      CompressionConfigurationSnapshot  `json:"compression"`
	Worker           WorkerConfigurationSnapshot       `json:"worker"`
	ResponseProtocol ResponseProtocolSnapshot          `json:"response_protocol"`
	Continuation     ContinuationConfigurationSnapshot `json:"continuation"`
	Execution        Settings                          `json:"execution"`
	// Engine / Hardening are the Phase 16 engine and final-hardening
	// configuration (WBS 16.90, P16-77). Snapshots taken before Phase 16
	// carry neither; EngineSettingsOrDefault / HardeningOrDefault apply
	// the defaults deterministically.
	Engine    *EngineSettings     `json:"engine,omitempty"`
	Hardening *hardening.Settings `json:"hardening,omitempty"`
}

// EngineSettingsOrDefault returns the captured engine settings.
func (c ConfigurationContent) EngineSettingsOrDefault() EngineSettings {
	if c.Engine == nil {
		return DefaultEngineSettings()
	}
	return *c.Engine
}

// HardeningOrDefault returns the captured hardening settings.
func (c ConfigurationContent) HardeningOrDefault() hardening.Settings {
	if c.Hardening == nil {
		return hardening.DefaultSettings()
	}
	return *c.Hardening
}

// ExecutionConfigurationSnapshot is the immutable execution configuration
// (WBS 15.40). Hash covers Content only (identity/timestamps excluded) so
// the same configuration hashes identically (WBS 15.41).
type ExecutionConfigurationSnapshot struct {
	ID          uuid.UUID
	ExecutionID uuid.UUID
	ProjectID   uuid.UUID
	Content     ConfigurationContent
	CreatedAt   time.Time
}

// Normalize sorts the order-insensitive lists so encoding is canonical.
func (c *ConfigurationContent) Normalize() {
	sort.SliceStable(c.Roles, func(i, j int) bool { return c.Roles[i].RoleKey < c.Roles[j].RoleKey })
	sort.SliceStable(c.PromptTemplates, func(i, j int) bool { return c.PromptTemplates[i].Key < c.PromptTemplates[j].Key })
	if c.Roles == nil {
		c.Roles = []role.RoleSnapshot{}
	}
	if c.PromptTemplates == nil {
		c.PromptTemplates = []PromptTemplateSnapshot{}
	}
	if c.Worker.AllowedRoots == nil {
		c.Worker.AllowedRoots = []string{}
	}
	if c.Worker.Capabilities == nil {
		c.Worker.Capabilities = []string{}
	}
	sort.Strings(c.Worker.Capabilities)
	if c.Engine != nil {
		c.Engine.Normalize()
	}
	if c.Hardening != nil && c.Hardening.Exclusions == nil {
		c.Hardening.Exclusions = []string{}
	}
}

// EncodeContent returns the canonical JSON encoding (byte-stable).
func EncodeContent(c ConfigurationContent) ([]byte, error) {
	c.Normalize()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("execution: encode configuration: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DecodeContent reconstructs the content from its canonical encoding.
func DecodeContent(b []byte) (ConfigurationContent, error) {
	var c ConfigurationContent
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return ConfigurationContent{}, fmt.Errorf("execution: decode configuration: %w", err)
	}
	return c, nil
}

// HashCanonical is SHA-256 over canonical bytes (WBS 15.41).
func HashCanonical(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ContentHash encodes and hashes the content.
func ContentHash(c ConfigurationContent) (string, error) {
	b, err := EncodeContent(c)
	if err != nil {
		return "", err
	}
	return HashCanonical(b), nil
}

// StripUserinfo removes credentials embedded in a URL (WBS 15.42); a
// non-URL value is returned unchanged.
func StripUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
