package worker

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
)

// WorkerProvider names a worker implementation (WBS 12.2). Only
// HERMES_GATEWAY has an adapter in Phase 12; the others are reserved so
// future adapters need no domain change.
type WorkerProvider string

// Known worker providers.
const (
	ProviderHermesGateway WorkerProvider = "HERMES_GATEWAY"
	ProviderClaudeCode    WorkerProvider = "CLAUDE_CODE"
	ProviderLocalAgent    WorkerProvider = "LOCAL_AGENT"
	ProviderRemoteAgent   WorkerProvider = "REMOTE_AGENT"
)

// Known reports whether p is a recognised provider value.
func (p WorkerProvider) Known() bool {
	switch p {
	case ProviderHermesGateway, ProviderClaudeCode, ProviderLocalAgent, ProviderRemoteAgent:
		return true
	}
	return false
}

// Worker is the provider-independent worker contract (WBS 12.1). The
// overseer owns workflow state; a Worker only transports a prepared prompt
// to an execution session and returns the raw response.
type Worker interface {
	Send(ctx context.Context, req WorkerRequest) (WorkerResponse, error)
	NewSession(ctx context.Context, req NewSessionRequest) (SessionInfo, error)
	ChangeDirectory(ctx context.Context, req ChangeDirectoryRequest) error
	Session(ctx context.Context) (SessionInfo, error)
	Capabilities(ctx context.Context) (WorkerCapabilities, error)
	Close(ctx context.Context) error
}

// WorkerRequest is one prompt dispatch (WBS 12.9). It carries the
// prepared, immutable worker prompt — never the project domain model.
type WorkerRequest struct {
	RequestID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	Prompt compression.WorkerPrompt

	Metadata WorkerRequestMetadata
}

// WorkerRequestMetadata is execution correlation only (WBS 12.10); it
// never alters the prompt text.
type WorkerRequestMetadata struct {
	RuntimeStateID uuid.UUID

	StateKey string
	RoleKey  string

	PromptHash string

	Attempt int
}

// WorkerResponse is the transport-level reply (WBS 12.12) — not yet the
// parsed COMPLETE/INCOMPLETE protocol (Phase 13).
type WorkerResponse struct {
	RequestID uuid.UUID

	Content string

	Session SessionInfo

	Usage WorkerUsage

	StartedAt   time.Time
	CompletedAt time.Time

	ProviderMetadata map[string]string
}

// WorkerUsage is token telemetry (WBS 12.13). Nil fields are unknown;
// Estimated marks overseer estimates as opposed to provider-reported facts.
type WorkerUsage struct {
	InputTokens  *int64
	OutputTokens *int64

	ContextTokens *int64
	ContextWindow *int64

	Estimated bool
}

// SessionInfo describes a provider-issued execution session (WBS 12.16).
// ID is opaque and kept byte-exact; a nil WorkingDirectory means UNKNOWN
// (WBS 12.26).
type SessionInfo struct {
	ID string

	Provider WorkerProvider

	CreatedAt time.Time

	WorkingDirectory *string

	Usage WorkerUsage
}

// NewSessionRequest asks for a fresh conversational context (WBS 12.19).
type NewSessionRequest struct {
	ProjectID uuid.UUID
}

// ChangeDirectoryRequest asks the worker session to change its working
// directory (WBS 12.23).
type ChangeDirectoryRequest struct {
	ProjectID uuid.UUID
	Path      string
}

// Int64 returns a pointer to v (telemetry helper).
func Int64(v int64) *int64 { return &v }
