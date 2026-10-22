package hermes

import (
	"strconv"
	"strings"
	"time"

	"overseer/internal/worker"
)

// Adapter-local Hermes Gateway wire protocol (PLAN Decision 1). These types
// never leave this package (WBS 12.4.3). The gateway is OpenAI-compatible:
//
//	GET  /v1/models            reachability / credential probe (handshake)
//	POST /v1/chat/completions  every message send:
//	                           {"model", "messages":[{"role","content"}], "stream":false}
//	                           (X-Request-ID, X-Overseer-Dispatch-Key, X-Overseer-Session-ID,
//	                           X-Overseer-Metadata)
//
// Sessions and their history are held by the client: the history is
// resent with each message. A session starts with the prompt
// "change working directory to <path>" (ChangeDirectoryPrompt).
//
// Errors are non-2xx with {"error":{"code","message","type"}}.

// Gateway paths.
const (
	PathModels          = "/v1/models"
	PathChatCompletions = "/v1/chat/completions"
)

// ChangeDirectoryPrompt is the prompt that moves the agent into the
// project's working directory (sent at the start of a session and before
// every workflow command).
func ChangeDirectoryPrompt(path string) string {
	if strings.ContainsAny(path, " \t'\"") {
		path = strconv.Quote(path)
	}
	return "change working directory to " + path
}

// ChatMessage is one chat completions message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the /v1/chat/completions request body.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// ChatUsage is the OpenAI token usage block.
type ChatUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

// ChatResponse is the /v1/chat/completions reply.
type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *ChatUsage `json:"usage"`
}

// gateway maps a chat completion to the adapter's response. The request
// and session ids are the client's own correlation (the API echoes
// neither); content stays nil when the reply carries none, so Normalize
// reports a PROTOCOL_ERROR instead of inventing it.
func (cr ChatResponse) gateway(requestID, sessionID string) GatewayResponse {
	out := GatewayResponse{RequestID: &requestID, SessionID: &sessionID, Metadata: map[string]string{}}
	if cr.ID != "" {
		out.Metadata["completion_id"] = cr.ID
	}
	if cr.Model != "" {
		out.Metadata["model"] = cr.Model
	}
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		out.Content = ch.Message.Content
		if ch.FinishReason != nil {
			out.Metadata["finish_reason"] = *ch.FinishReason
		}
	}
	if u := cr.Usage; u != nil {
		gu := &GatewayUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, ContextTokens: u.TotalTokens}
		if gu.ContextTokens == nil && u.PromptTokens != nil && u.CompletionTokens != nil {
			t := *u.PromptTokens + *u.CompletionTokens
			gu.ContextTokens = &t
		}
		out.Usage = gu
	}
	now := time.Now().UTC()
	out.CompletedAt = &now
	return out
}

// ProtocolVersion is the wire protocol this adapter speaks.
const ProtocolVersion = "1"

type gatewayCapabilities struct {
	Sessions         bool `json:"sessions"`
	NewSession       bool `json:"new_session"`
	ChangeDirectory  bool `json:"change_directory"`
	ContextTelemetry bool `json:"context_telemetry"`
	Streaming        bool `json:"streaming"`
	Cancellation     bool `json:"cancellation"`
	ToolExecution    bool `json:"tool_execution"`
}

// GatewayHandshake is the connection probe outcome.
type GatewayHandshake struct {
	Status          string              `json:"status"`
	ProviderVersion string              `json:"provider_version"`
	ProtocolVersion string              `json:"protocol_version"`
	Capabilities    gatewayCapabilities `json:"capabilities"`
}

// GatewayUsage is gateway-reported telemetry (always actual).
type GatewayUsage struct {
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
	ContextTokens *int64 `json:"context_tokens,omitempty"`
	ContextWindow *int64 `json:"context_window,omitempty"`
}

// GatewaySession is a session description.
type GatewaySession struct {
	SessionID        string        `json:"session_id"`
	CreatedAt        *time.Time    `json:"created_at,omitempty"`
	WorkingDirectory *string       `json:"working_directory"`
	Usage            *GatewayUsage `json:"usage,omitempty"`
}

// GatewayRequest is one message send.
type GatewayRequest struct {
	RequestID string            `json:"request_id"`
	Model     string            `json:"model,omitempty"`
	Content   string            `json:"content"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// GatewayResponse is a completed message reply. Pointer fields detect
// missing data (WBS 12.92).
type GatewayResponse struct {
	RequestID   *string           `json:"request_id"`
	SessionID   *string           `json:"session_id"`
	Content     *string           `json:"content"`
	Usage       *GatewayUsage     `json:"usage,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
}

// GatewayError is the error envelope body.
type GatewayError struct {
	Error struct {
		Code              string `json:"code"`
		Message           string `json:"message"`
		RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
	} `json:"error"`
}

// GatewayStreamEvent is one SSE data payload.
type GatewayStreamEvent struct {
	Seq      int64            `json:"seq"`
	Delta    string           `json:"delta,omitempty"`
	Tool     *GatewayTool     `json:"tool,omitempty"`
	Usage    *GatewayUsage    `json:"usage,omitempty"`
	Response *GatewayResponse `json:"response,omitempty"`
	Error    *GatewayError    `json:"error,omitempty"`
}

// GatewayTool is tool activity telemetry.
type GatewayTool struct {
	Name       string     `json:"name"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS *int64     `json:"duration_ms,omitempty"`
}

// Correlation headers (WBS 12.32/12.33).
const (
	HeaderRequestID = "X-Request-ID"
	// HeaderDispatchKey carries the dispatch idempotency identity. It is
	// deliberately NOT "Idempotency-Key": net/http treats requests with that
	// header as replayable and silently re-sends them after a connection
	// drop, which would duplicate a prompt (WBS 12.34/12.89).
	HeaderDispatchKey = "X-Overseer-Dispatch-Key"
	// HeaderSessionID names the client-held session a message belongs to.
	HeaderSessionID = "X-Overseer-Session-ID"
	// HeaderMetadata carries the request metadata as a JSON object; the
	// body stays the plain chat completions shape.
	HeaderMetadata = "X-Overseer-Metadata"
	// HeaderHermesSessionID files a request under a Hermes session (the
	// gateway's session continuity header). The overseer still sends the
	// full history; Hermes stores it under this id without duplicating it.
	HeaderHermesSessionID = "X-Hermes-Session-Id"
)

// Translate maps a generic request to the gateway request (WBS 12.11):
// prompt content verbatim, configured model, correlation id and metadata.
// Metadata never alters the content (WBS 12.10.4).
func Translate(req worker.WorkerRequest, cfg worker.WorkerConfig) (GatewayRequest, error) {
	if err := req.Validate(); err != nil {
		return GatewayRequest{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, err.Error(), err)
	}
	if req.Metadata.PromptHash != "" && req.Metadata.PromptHash != req.Prompt.Hash {
		return GatewayRequest{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "metadata prompt hash does not match the prompt", nil)
	}
	md := map[string]string{
		"project_id":  req.ProjectID.String(),
		"prompt_id":   req.Prompt.ID.String(),
		"prompt_hash": req.Prompt.Hash,
		"attempt":     strconv.Itoa(req.Metadata.Attempt),
	}
	if req.PhaseID != nil {
		md["phase_id"] = req.PhaseID.String()
	}
	if req.WorkUnitID != nil {
		md["work_unit_id"] = req.WorkUnitID.String()
	}
	if req.Metadata.StateKey != "" {
		md["state_key"] = req.Metadata.StateKey
	}
	if req.Metadata.RoleKey != "" {
		md["role_key"] = req.Metadata.RoleKey
	}
	if req.Metadata.RuntimeStateID.String() != "00000000-0000-0000-0000-000000000000" {
		md["runtime_state_id"] = req.Metadata.RuntimeStateID.String()
	}
	return GatewayRequest{RequestID: req.RequestID.String(), Model: cfg.Model, Content: req.Prompt.Content, Metadata: md}, nil
}

func usageFrom(u *GatewayUsage) worker.WorkerUsage {
	if u == nil {
		return worker.WorkerUsage{}
	}
	return worker.WorkerUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ContextTokens: u.ContextTokens, ContextWindow: u.ContextWindow}
}

func sessionFrom(s GatewaySession) worker.SessionInfo {
	out := worker.SessionInfo{ID: s.SessionID, Provider: worker.ProviderHermesGateway, WorkingDirectory: s.WorkingDirectory, Usage: usageFrom(s.Usage)}
	if s.CreatedAt != nil {
		out.CreatedAt = s.CreatedAt.UTC()
	} else {
		out.CreatedAt = time.Now().UTC()
	}
	return out
}

func capabilitiesFrom(h GatewayHandshake) worker.WorkerCapabilities {
	c := h.Capabilities
	return worker.WorkerCapabilities{Provider: worker.ProviderHermesGateway, ProviderVersion: h.ProviderVersion, ProtocolVersion: h.ProtocolVersion,
		Sessions: c.Sessions, NewSession: c.NewSession, ChangeDirectory: c.ChangeDirectory, ContextTelemetry: c.ContextTelemetry,
		Streaming: c.Streaming, Cancellation: c.Cancellation, ToolExecution: c.ToolExecution}
}

// PathSessions is the Hermes session resource (GET / DELETE {id}).
const PathSessions = "/api/sessions/"

// HermesSessionStats is a Hermes session's counters (GET /api/sessions/{id}).
type HermesSessionStats struct {
	ID               string `json:"id"`
	MessageCount     int64  `json:"message_count"`
	ToolCallCount    int64  `json:"tool_call_count"`
	APICallCount     int64  `json:"api_call_count"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
}
