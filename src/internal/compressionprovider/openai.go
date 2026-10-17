// Package compressionprovider holds the concrete prompt-compression provider
// adapters (WBS 9.39/9.40). It is infrastructure: it may use net/http, and
// no domain package may import it. All provider-specific request/response
// construction stays here; callers see only compression.Compressor /
// compression.SemanticValidator and the generic compression errors.
package compressionprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"overseer/internal/compression"
)

// Options configures one OpenAI-compatible endpoint.
type Options struct {
	// Name is the configured provider name (recorded on responses).
	Name string
	// BaseURL is the API root, e.g. https://api.example.com/v1 or
	// http://127.0.0.1:8080/v1 (llama.cpp / Ollama / vLLM).
	BaseURL string
	// APIKey is the resolved credential; empty = no Authorization header
	// (local servers, T9.40.2). Never logged or returned in errors.
	APIKey string
	// MaxOutputTokens caps the completion (0 = provider default).
	MaxOutputTokens int
	// HTTPClient overrides the client (tests); nil = a default client.
	HTTPClient *http.Client
}

// OpenAICompatible is a chat-completions adapter satisfying
// compression.Compressor (WBS 9.39). The same adapter serves cloud, LAN and
// local servers — the core does not care where the model runs (WBS 9.40).
type OpenAICompatible struct {
	opts   Options
	client *http.Client
}

var _ compression.Compressor = (*OpenAICompatible)(nil)

// New builds the adapter.
func New(opts Options) (*OpenAICompatible, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, compression.NewError(compression.ErrorConfiguration, "compression provider base URL is required")
	}
	c := opts.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	return &OpenAICompatible{opts: opts, client: c}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type chatResult struct {
	Content      string
	Model        string
	ID           string
	FinishReason string
	InputTokens  int
	OutputTokens int
	Duration     time.Duration
}

// BuildUserMessage is the user message of a compression request: the
// structured input, followed by the retry feedback when present.
func BuildUserMessage(req compression.CompressionRequest) string {
	msg := req.Content
	if req.Feedback != "" {
		msg += "\n\n" + req.Feedback
	}
	return msg
}

// Compress implements compression.Compressor (T9.39.1-T9.39.3).
func (o *OpenAICompatible) Compress(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
	res, err := o.chat(ctx, req.Model, req.InstructionContent, BuildUserMessage(req))
	if err != nil {
		return compression.CompressionResponse{}, err
	}
	return compression.CompressionResponse{
		Content:           res.Content,
		Provider:          o.opts.Name,
		Model:             firstNonEmpty(res.Model, req.Model),
		InputTokens:       res.InputTokens,
		OutputTokens:      res.OutputTokens,
		FinishReason:      res.FinishReason,
		ProviderRequestID: res.ID,
		Duration:          res.Duration,
	}, nil
}

// Ask sends one user prompt to model and returns the reply (provider
// connection test).
func (o *OpenAICompatible) Ask(ctx context.Context, model, prompt string) (string, error) {
	res, err := o.chat(ctx, model, "", prompt)
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// chat performs one chat-completions call and translates every failure into
// a generic compression error with credentials redacted (T9.39.4/T9.39.5).
func (o *OpenAICompatible) chat(ctx context.Context, model, system, user string) (chatResult, error) {
	body := chatRequest{Model: model, Temperature: 0, MaxTokens: o.opts.MaxOutputTokens}
	if system != "" {
		body.Messages = append(body.Messages, chatMessage{Role: "system", Content: system})
	}
	body.Messages = append(body.Messages, chatMessage{Role: "user", Content: user})
	raw, err := json.Marshal(body)
	if err != nil {
		return chatResult{}, compression.NewError(compression.ErrorMalformed, "encode request")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.opts.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return chatResult{}, compression.NewError(compression.ErrorConfiguration, o.redact(err.Error()))
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if o.opts.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.opts.APIKey)
	}
	start := time.Now()
	resp, err := o.client.Do(hreq)
	if err != nil {
		return chatResult{}, o.transportError(ctx, err)
	}
	defer resp.Body.Close()
	payload, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	dur := time.Since(start)
	if rerr != nil {
		return chatResult{}, o.transportError(ctx, rerr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return chatResult{}, o.statusError(resp.StatusCode, payload)
	}
	var cr chatResponse
	if err := json.Unmarshal(payload, &cr); err != nil {
		return chatResult{}, compression.NewError(compression.ErrorMalformed, "provider returned a malformed response")
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return chatResult{}, compression.NewError(compression.ErrorEmpty, "provider returned no content")
	}
	out := chatResult{
		Content: cr.Choices[0].Message.Content, Model: cr.Model, ID: cr.ID,
		FinishReason: cr.Choices[0].FinishReason, Duration: dur,
	}
	if cr.Usage != nil {
		out.InputTokens, out.OutputTokens = cr.Usage.PromptTokens, cr.Usage.CompletionTokens
	}
	return out, nil
}

func (o *OpenAICompatible) transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return compression.NewError(compression.ErrorTimeout, "provider request timed out")
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return compression.NewError(compression.ErrorCancelled, "provider request cancelled")
	default:
		return compression.NewError(compression.ErrorNetwork, o.redact(err.Error()))
	}
}

// statusError maps an HTTP status (and error body) to a generic kind.
func (o *OpenAICompatible) statusError(status int, body []byte) error {
	msg := o.redact(providerMessage(body))
	lower := strings.ToLower(string(body))
	var kind compression.ErrorKind
	switch {
	case status == http.StatusTooManyRequests:
		kind = compression.ErrorRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = compression.ErrorAuth
	case strings.Contains(lower, "context_length_exceeded") || strings.Contains(lower, "context length") ||
		strings.Contains(lower, "context window") || strings.Contains(lower, "too many tokens") || status == http.StatusRequestEntityTooLarge:
		kind = compression.ErrorContextOverflow
	case status >= 500:
		kind = compression.ErrorUnavailable
	case status == http.StatusRequestTimeout:
		kind = compression.ErrorTimeout
	default:
		kind = compression.ErrorConfiguration
	}
	return &compression.CompressionError{Kind: kind, Message: msg, StatusCode: status}
}

// providerMessage extracts a short message from an error body.
func providerMessage(body []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil {
		switch v := e.Error.(type) {
		case string:
			return shorten(v)
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return shorten(m)
			}
		}
	}
	return shorten(strings.TrimSpace(string(body)))
}

func shorten(s string) string {
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// redact removes the credential from any text (T9.41.4).
func (o *OpenAICompatible) redact(s string) string {
	if o.opts.APIKey != "" {
		s = strings.ReplaceAll(s, o.opts.APIKey, "REDACTED")
	}
	return s
}

// String never includes the credential.
func (o *OpenAICompatible) String() string {
	auth := "none"
	if o.opts.APIKey != "" {
		auth = "REDACTED"
	}
	return fmt.Sprintf("openai_compatible(%s %s key=%s)", o.opts.Name, o.opts.BaseURL, auth)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
