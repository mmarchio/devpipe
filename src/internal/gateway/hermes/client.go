package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
)

// HermesGatewayClient is the low-level gateway boundary (WBS 12.4). The
// worker adapter programs against it; tests may substitute it. Every error
// it returns is a *worker.WorkerError carrying the delivery state.
type HermesGatewayClient interface {
	Connect(ctx context.Context) (GatewayHandshake, error)
	New(ctx context.Context, projectID string) (GatewaySession, error)
	Get(ctx context.Context, sessionID string) (GatewaySession, error)
	ChangeDirectory(ctx context.Context, sessionID, path, model string) (GatewaySession, error)
	Send(ctx context.Context, sessionID string, req GatewayRequest, dispatchKey string) (GatewayResponse, []byte, error)
	Stream(ctx context.Context, sessionID string, req GatewayRequest, dispatchKey string) (*EventReader, error)
	Cancel(ctx context.Context, sessionID, requestID string) (bool, error)
	Close(ctx context.Context) error
}

// HTTPClient is the HTTP implementation of HermesGatewayClient.
type HTTPClient struct {
	base *url.URL
	hc   *http.Client
	tr   *http.Transport
	// tokenFn returns the current auth secret ("" = none); it is resolved
	// per request so a rotated credential applies to the next request
	// (WBS 18.78).
	tokenFn func() string
	max     int64
	cfgTTL  HermesGatewayConfig

	mu       sync.Mutex
	sessions map[string]*chatSession
}

var _ HermesGatewayClient = (*HTTPClient)(nil)

// NewHTTPClient builds the client. token is the resolved auth secret ("" =
// none); it is only ever placed in the Authorization header.
func NewHTTPClient(cfg HermesGatewayConfig, token string) (*HTTPClient, error) {
	return NewHTTPClientWithToken(cfg, func() string { return token })
}

// NewHTTPClientWithToken builds the client over a token source resolved
// per request.
func NewHTTPClientWithToken(cfg HermesGatewayConfig, tokenFn func() string) (*HTTPClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base, _ := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: cfg.ConnectTimeout, Control: cfg.DialControl}).DialContext
	tr.TLSHandshakeTimeout = cfg.ConnectTimeout
	// One client per project: idle keep-alive connections of finished
	// projects are released promptly (WBS 18.158: connections return to
	// bounded levels).
	tr.IdleConnTimeout = 15 * time.Second
	if cfg.TLS != nil {
		tc, err := cfg.TLS.build()
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tc
	}
	max := cfg.MaxResponseBytes
	if max <= 0 {
		max = DefaultMaxResponseBytes
	}
	return &HTTPClient{base: base, hc: &http.Client{Transport: tr}, tr: tr, tokenFn: tokenFn, max: max, cfgTTL: cfg,
		sessions: map[string]*chatSession{}}, nil
}

func (c *HTTPClient) tok() string {
	if c.tokenFn == nil {
		return ""
	}
	return c.tokenFn()
}

type result struct {
	status int
	header http.Header
	body   []byte
}

// do performs one request. wrote reports whether the request was fully
// transmitted, which decides DEFINITELY_NOT_SENT vs DELIVERY_UNKNOWN.
func (c *HTTPClient) do(ctx context.Context, method, path string, body any, hdr map[string]string) (result, bool, error) {
	var wrote atomic.Bool
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return result{}, false, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "encode request", err)
		}
		rd = bytes.NewReader(b)
	}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			wrote.Store(true)
		}
	}})
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return result{}, false, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "build request", nil)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := c.tok(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return result{}, wrote.Load(), c.transportError(ctx, err, wrote.Load())
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.max+1))
	if err != nil {
		// Headers arrived but the body did not: the request executed
		// (or is executing) and its outcome is unknown.
		return result{}, true, c.transportError(ctx, err, true)
	}
	if int64(len(b)) > c.max {
		e := worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, fmt.Sprintf("response exceeds %d bytes", c.max), nil)
		e.Diagnostic = worker.Truncate(worker.Redact(string(b[:256]), c.tok()), diagnosticLimit)
		return result{}, true, e
	}
	return result{status: resp.StatusCode, header: resp.Header, body: b}, true, nil
}

func (c *HTTPClient) transportError(ctx context.Context, err error, wrote bool) *worker.WorkerError {
	d := worker.DeliveryNotSent
	if wrote {
		d = worker.DeliveryUnknown
	}
	code := worker.CodeUnavailable
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = worker.CodeTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		code = worker.CodeCancelled
	case wrote:
		code = worker.CodeDeliveryUnknown
	}
	return worker.NewError(code, d, worker.Redact(err.Error(), c.tok()), nil).Sanitize(c.tok())
}

func (c *HTTPClient) decode(r result, out any) error {
	if r.status < 200 || r.status > 299 {
		return NormalizeError(r.status, r.header, r.body, c.tok())
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return protocolError("malformed gateway response: "+err.Error(), r.body, c.tok())
	}
	return nil
}

// Connect verifies the gateway is reachable and accepts the credential
// within the configured connect timeout (WBS 12.5). The OpenAI-compatible
// gateway has no handshake endpoint, so the model listing is the probe:
// it is cheap and never runs the agent. The advertised capabilities are
// what this client provides over /v1/chat/completions.
func (c *HTTPClient) Connect(ctx context.Context) (GatewayHandshake, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfgTTL.ConnectTimeout)
	defer cancel()
	r, _, err := c.do(ctx, http.MethodGet, PathModels, nil, nil)
	if err != nil {
		we := worker.AsWorkerError(err)
		if we.Code == worker.CodeDeliveryUnknown || we.Code == worker.CodeCancelled {
			we.Code = worker.CodeUnavailable
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			we.Code = worker.CodeTimeout
		}
		we.Delivery = worker.DeliveryNotSent
		return GatewayHandshake{}, we
	}
	// A gateway without a model listing (404) still answered and accepted
	// the credential: it is reachable.
	if (r.status < 200 || r.status > 299) && r.status != http.StatusNotFound {
		we := NormalizeError(r.status, r.header, r.body, c.tok())
		we.Delivery = worker.DeliveryNotSent
		return GatewayHandshake{}, we
	}
	return GatewayHandshake{Status: "ok", ProviderVersion: r.header.Get("Server"), ProtocolVersion: ProtocolVersion,
		Capabilities: gatewayCapabilities{Sessions: true, NewSession: true, ChangeDirectory: true, ContextTelemetry: true, ToolExecution: true}}, nil
}

// chatSession is a client-held conversation: chat completions are
// stateless, so the session's history is resent with every message.
type chatSession struct {
	created time.Time
	cwd     *string
	history []ChatMessage
	usage   *GatewayUsage
}

func (s *chatSession) describe(id string) GatewaySession {
	out := GatewaySession{SessionID: id, Usage: s.usage}
	created := s.created
	out.CreatedAt = &created
	if s.cwd != nil {
		d := *s.cwd
		out.WorkingDirectory = &d
	}
	return out
}

func sessionNotFound(id string) *worker.WorkerError {
	pc := "session_not_found"
	e := worker.NewError(worker.CodeSessionLost, worker.DeliveryNotSent, "session "+id+" does not exist", nil)
	e.ProviderCode = &pc
	return e
}

// New creates a session ("new"). Sessions are held by this client; the
// gateway sees only their messages.
func (c *HTTPClient) New(ctx context.Context, projectID string) (GatewaySession, error) {
	if err := ctx.Err(); err != nil {
		return GatewaySession{}, worker.NewError(worker.CodeCancelled, worker.DeliveryNotSent, "cancelled", err)
	}
	id := "hs-" + uuid.NewString()
	s := &chatSession{created: time.Now().UTC()}
	c.mu.Lock()
	c.sessions[id] = s
	out := s.describe(id)
	c.mu.Unlock()
	return out, nil
}

// Get looks a session up. A session this client does not hold — one
// persisted before a restart, or opened by another overseer instance — is
// adopted: same identity, empty history and no confirmed working
// directory, so the next dispatch restores the directory ("cd") first.
// Chat completions keep no server-side session that could be lost.
func (c *HTTPClient) Get(_ context.Context, sessionID string) (GatewaySession, error) {
	if sessionID == "" {
		return GatewaySession{}, sessionNotFound(sessionID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[sessionID]
	if s == nil {
		s = &chatSession{created: time.Now().UTC()}
		c.sessions[sessionID] = s
	}
	return s.describe(sessionID), nil
}

// ChangeDirectory issues "cd": the session's first prompt tells the
// agent to change to the working directory (ChangeDirectoryPrompt). A
// reply confirms it; the exchange stays in the session history, so every
// later message carries it. A failed send leaves the directory unchanged.
func (c *HTTPClient) ChangeDirectory(ctx context.Context, sessionID, path, model string) (GatewaySession, error) {
	c.mu.Lock()
	s := c.sessions[sessionID]
	c.mu.Unlock()
	if s == nil {
		return GatewaySession{}, sessionNotFound(sessionID)
	}
	rid := uuid.NewString()
	if _, _, err := c.send(ctx, sessionID, GatewayRequest{RequestID: rid, Model: model, Content: ChangeDirectoryPrompt(path)}, "cd:"+rid, false); err != nil {
		return GatewaySession{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := path
	s.cwd = &p
	return s.describe(sessionID), nil
}

// Send transmits one message as POST /v1/chat/completions (never retried
// here). Every prompt starts with the session's "change working directory
// to <path>" line, so the agent works in the project directory whatever
// its own default. The exchange joins the session history only on
// success.
func (c *HTTPClient) Send(ctx context.Context, sessionID string, req GatewayRequest, key string) (GatewayResponse, []byte, error) {
	return c.send(ctx, sessionID, req, key, true)
}

// WithWorkingDirectory prefixes a prompt with the change-directory line.
func WithWorkingDirectory(path, prompt string) string {
	return ChangeDirectoryPrompt(path) + "\n\n" + prompt
}

func (c *HTTPClient) send(ctx context.Context, sessionID string, req GatewayRequest, key string, prefix bool) (GatewayResponse, []byte, error) {
	c.mu.Lock()
	s := c.sessions[sessionID]
	var msgs []ChatMessage
	var cwd *string
	if s != nil {
		msgs = append(msgs, s.history...)
		cwd = s.cwd
	}
	c.mu.Unlock()
	if s == nil {
		return GatewayResponse{}, nil, sessionNotFound(sessionID)
	}
	content := req.Content
	if prefix && cwd != nil {
		content = WithWorkingDirectory(*cwd, content)
	}
	user := ChatMessage{Role: "user", Content: content}
	body := ChatRequest{Model: req.Model, Messages: append(msgs, user), Stream: false}
	hdr := map[string]string{HeaderRequestID: req.RequestID, HeaderDispatchKey: key, HeaderSessionID: sessionID}
	if len(req.Metadata) > 0 {
		md, _ := json.Marshal(req.Metadata)
		hdr[HeaderMetadata] = string(md)
	}
	r, _, err := c.do(ctx, http.MethodPost, PathChatCompletions, body, hdr)
	if err != nil {
		return GatewayResponse{}, nil, err
	}
	var cr ChatResponse
	if err := c.decode(r, &cr); err != nil {
		return GatewayResponse{}, r.body, err
	}
	gr := cr.gateway(req.RequestID, sessionID)
	if gr.Content != nil {
		c.mu.Lock()
		s.history = append(s.history, user, ChatMessage{Role: "assistant", Content: *gr.Content})
		if gr.Usage != nil {
			s.usage = gr.Usage
		}
		c.mu.Unlock()
	}
	return gr, r.body, nil
}

// Cancel is not offered by the chat completions API: an in-flight request
// is never confirmed cancelled.
func (c *HTTPClient) Cancel(context.Context, string, string) (bool, error) {
	return false, nil
}

// Close releases pooled connections. The external session is untouched.
func (c *HTTPClient) Close(context.Context) error {
	c.tr.CloseIdleConnections()
	return nil
}

// Stream is not offered: the client advertises no streaming capability,
// so every message goes through Send.
func (c *HTTPClient) Stream(context.Context, string, GatewayRequest, string) (*EventReader, error) {
	return nil, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "gateway does not support streaming", nil)
}

// EventReader reads gateway SSE events.
type EventReader struct {
	body  io.ReadCloser
	sc    *bufio.Scanner
	token string
	read  int64
	max   int64
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Next returns the next event (name, payload); io.EOF at end of stream.
func (r *EventReader) Next() (string, GatewayStreamEvent, []byte, error) {
	var name string
	var data []byte
	for r.sc.Scan() {
		line := r.sc.Text()
		r.read += int64(len(line))
		if r.read > r.max {
			return "", GatewayStreamEvent{}, nil, worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, "stream exceeds the response size limit", nil)
		}
		switch {
		case line == "":
			if name == "" && data == nil {
				continue
			}
			var ev GatewayStreamEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return name, ev, data, protocolError("malformed stream event", data, r.token)
			}
			return name, ev, data, nil
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))...)
		}
	}
	if err := r.sc.Err(); err != nil {
		return "", GatewayStreamEvent{}, nil, worker.NewError(worker.CodeDeliveryUnknown, worker.DeliveryUnknown, "stream interrupted", nil)
	}
	return "", GatewayStreamEvent{}, nil, io.EOF
}

// Close closes the stream body.
func (r *EventReader) Close() error { return r.body.Close() }
