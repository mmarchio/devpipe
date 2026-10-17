// Package hermestest is a deterministic fake Hermes Gateway (WBS 12.68)
// speaking the adapter's OpenAI-compatible wire protocol (GET /v1/models,
// POST /v1/chat/completions). Sessions are client-held; the fake tracks
// them by the X-Overseer-Session-ID header. Tests script per-message
// behaviours; every received request is logged for assertions.
package hermestest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage is gateway telemetry.
type Usage struct {
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
	ContextTokens *int64 `json:"context_tokens,omitempty"`
	ContextWindow *int64 `json:"context_window,omitempty"`
}

// I64 returns a pointer.
func I64(v int64) *int64 { return &v }

// Behavior scripts the reply to one message send.
type Behavior struct {
	// Delay before replying (honours client disconnect).
	Delay time.Duration
	// Status + ErrorCode produce an error envelope.
	Status     int
	ErrorCode  string
	Message    string
	RetryAfter int
	// Drop reads the request then closes the connection without a reply
	// (delivery unknown).
	Drop bool
	// Raw replaces the body (malformed payloads) with status 200.
	Raw string
	// Content overrides the reply content.
	Content *string
	// Usage overrides the reply usage (NoUsage = none at all).
	Usage   *Usage
	NoUsage bool
	// Oversize pads the content to this many bytes.
	Oversize int
	// NoChoices answers a completion without any choice.
	NoChoices bool
	// Hold blocks the reply until Release is called (concurrency tests).
	Hold chan struct{}
}

// Message is a logged message send.
type Message struct {
	SessionID   string
	RequestID   string
	DispatchKey string
	Auth        string
	// Content is the last (user) message without its leading
	// "change working directory to <path>" line, Dir that path ("" when
	// the prompt had none); Messages the full body list.
	Content  string
	Dir      string
	Model    string
	Stream   bool
	Messages []ChatMessage
	// Metadata is the X-Overseer-Metadata header object.
	Metadata map[string]string
	At       time.Time
}

// ChatMessage is one request message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type session struct {
	id    string
	cwd   *string
	busy  bool
	usage Usage
}

// Gateway is the fake server.
type Gateway struct {
	*httptest.Server

	mu        sync.Mutex
	sessions  map[string]*session
	killed    map[string]bool
	cdFail    map[string]bool
	cds       []string
	seen      map[string]bool
	ops       []string
	script    []Behavior
	messages  []Message
	healthErr int
	healthHit int
	paths     []string

	// Token, when set, is required as the bearer token.
	Token string
	// ProviderVersion reported as the Server header (empty = omitted).
	ProviderVersion string
	// ContextPerMessage grows the session context per message.
	ContextPerMessage int64
	// Responder, when set, decides the reply of a message the script does
	// not cover (concurrent projects: the reply depends on the request).
	Responder func(m Message) *Behavior
}

// New starts a gateway.
func New() *Gateway {
	g := &Gateway{sessions: map[string]*session{}, killed: map[string]bool{}, cdFail: map[string]bool{}, seen: map[string]bool{}, ProviderVersion: "hermes-fake-1.0", ContextPerMessage: 1000}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// Script queues behaviours for the next message sends (FIFO).
func (g *Gateway) Script(b ...Behavior) {
	g.mu.Lock()
	g.script = append(g.script, b...)
	g.mu.Unlock()
}

// FailHealth makes the model listing (the handshake probe) answer status
// (0 = healthy).
func (g *Gateway) FailHealth(status int) { g.mu.Lock(); g.healthErr = status; g.mu.Unlock() }

// FailCD makes the "cd path" prompt fail.
func (g *Gateway) FailCD(path string) { g.mu.Lock(); g.cdFail[path] = true; g.mu.Unlock() }

// Ops returns the ordered operations: "new" (a session's first request),
// "cd" (a session-start prompt) and "msg" (a logged message).
func (g *Gateway) Ops() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.ops...)
}

// opLocked records an operation of session id (caller holds mu).
func (g *Gateway) opLocked(id, op string) {
	if !g.seen[id] {
		g.seen[id] = true
		g.ops = append(g.ops, "new")
	}
	g.ops = append(g.ops, op)
}

// CDs returns the paths of every "cd" prompt received.
func (g *Gateway) CDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.cds...)
}

// KillSession makes the gateway reject further messages of a session
// with session_not_found (session loss).
func (g *Gateway) KillSession(id string) {
	g.mu.Lock()
	delete(g.sessions, id)
	g.killed[id] = true
	g.mu.Unlock()
}

// SetContext sets a session's reported context usage.
func (g *Gateway) SetContext(id string, tokens int64) {
	g.mu.Lock()
	g.sessionLocked(id).usage.ContextTokens = I64(tokens)
	g.mu.Unlock()
}

// sessionLocked returns (creating) the session state for id.
func (g *Gateway) sessionLocked(id string) *session {
	s := g.sessions[id]
	if s == nil {
		s = &session{id: id}
		g.sessions[id] = s
	}
	return s
}

// Messages returns the logged message sends.
func (g *Gateway) Messages() []Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Message(nil), g.messages...)
}

// MessagesFor returns sends for one session.
func (g *Gateway) MessagesFor(sessionID string) []Message {
	var out []Message
	for _, m := range g.Messages() {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	return out
}

// Sessions returns the ids of the live sessions that sent a message.
func (g *Gateway) Sessions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.sessions))
	for id := range g.sessions {
		out = append(out, id)
	}
	return out
}

// SessionCWD returns the directory a session's last successful "cd"
// prompt changed to.
func (g *Gateway) SessionCWD(id string) *string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sessions[id]; s != nil {
		return s.cwd
	}
	return nil
}

// HealthChecks counts handshake probes (model listings).
func (g *Gateway) HealthChecks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.healthHit
}

// Paths lists every request path received.
func (g *Gateway) Paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.paths...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, retryAfter int) {
	body := map[string]any{"code": code, "message": msg}
	if retryAfter > 0 {
		body["retry_after_seconds"] = retryAfter
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.Method+" "+r.URL.Path)
	token, ver := g.Token, g.ProviderVersion
	g.mu.Unlock()
	if ver != "" {
		w.Header().Set("Server", ver)
	}
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		writeErr(w, http.StatusUnauthorized, "auth_failed", "invalid gateway token", 0)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		g.models(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		g.completion(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no route", 0)
	}
}

func (g *Gateway) models(w http.ResponseWriter) {
	g.mu.Lock()
	g.healthHit++
	fail := g.healthErr
	g.mu.Unlock()
	if fail != 0 {
		writeErr(w, fail, "unavailable", "gateway unavailable", 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{"id": "hermes-agent", "object": "model"}}})
}

// cdPrefix starts the "change working directory to <path>" prompt.
const cdPrefix = "change working directory to "

// cdPath recognises the "change working directory to <path>" prompt (a
// message of that line alone).
func cdPath(m ChatMessage) (string, bool) {
	if m.Role != "user" || !strings.HasPrefix(m.Content, cdPrefix) || strings.Contains(m.Content, "\n") {
		return "", false
	}
	p := strings.TrimSpace(strings.TrimPrefix(m.Content, cdPrefix))
	if uq, err := strconv.Unquote(p); err == nil {
		p = uq
	}
	return p, p != ""
}

// changeDirectory answers a "cd <path>" prompt: the session's directory
// changes unless FailCD scripted the path. It is not logged as a message
// and consumes no scripted reply.
func (g *Gateway) changeDirectory(w http.ResponseWriter, id, rid, model, path string) {
	g.mu.Lock()
	if g.killed[id] {
		g.mu.Unlock()
		writeErr(w, http.StatusNotFound, "session_not_found", "session "+id+" does not exist", 0)
		return
	}
	s := g.sessionLocked(id)
	fail := g.cdFail[path]
	if !fail {
		p := path
		s.cwd = &p
	}
	g.cds = append(g.cds, path)
	g.opLocked(id, "cd")
	g.mu.Unlock()
	if fail {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_directory", "cd: "+path+": No such file or directory", 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl-" + rid, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "Working directory: " + path}, "finish_reason": "stop"}}})
}

func (g *Gateway) completion(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var in struct {
		Model    string        `json:"model"`
		Messages []ChatMessage `json:"messages"`
		Stream   bool          `json:"stream"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "messages required", 0)
		return
	}
	id := r.Header.Get("X-Overseer-Session-ID")
	rid := r.Header.Get("X-Request-ID")
	last := in.Messages[len(in.Messages)-1].Content
	// A prompt starting with the change-directory line: Content is the
	// prompt itself, Dir the directory it names.
	var dir string
	if line, rest, ok := strings.Cut(last, "\n\n"); ok && strings.HasPrefix(line, cdPrefix) {
		if p, ok := cdPath(ChatMessage{Role: "user", Content: line}); ok {
			dir, last = p, rest
		}
	}
	var md map[string]string
	if v := r.Header.Get("X-Overseer-Metadata"); v != "" {
		_ = json.Unmarshal([]byte(v), &md)
	}
	if path, ok := cdPath(in.Messages[len(in.Messages)-1]); ok {
		g.changeDirectory(w, id, rid, in.Model, path)
		return
	}
	g.mu.Lock()
	var s *session
	if !g.killed[id] {
		s = g.sessionLocked(id)
	}
	var b Behavior
	// A scripted reply is consumed only by a request the gateway accepts
	// (a rejected request — killed or busy session — produces no reply).
	scripted := false
	if len(g.script) > 0 && s != nil && !s.busy {
		b, g.script = g.script[0], g.script[1:]
		scripted = true
	}
	if s != nil {
		msg := Message{SessionID: id, RequestID: rid, DispatchKey: r.Header.Get("X-Overseer-Dispatch-Key"),
			Auth: r.Header.Get("Authorization"), Content: last, Dir: dir, Model: in.Model, Stream: in.Stream, Messages: in.Messages, Metadata: md, At: time.Now()}
		g.messages = append(g.messages, msg)
		g.opLocked(id, "msg")
		if !scripted && g.Responder != nil && !s.busy {
			if rb := g.Responder(msg); rb != nil {
				b = *rb
			}
		}
	}
	busy := s != nil && s.busy
	if s != nil && !busy {
		s.busy = true
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if s != nil && !busy {
			s.busy = false
		}
		g.mu.Unlock()
	}()
	switch {
	case s == nil:
		writeErr(w, http.StatusNotFound, "session_not_found", "session "+id+" does not exist", 0)
		return
	case busy:
		writeErr(w, http.StatusConflict, "session_busy", "session has a request in flight", 0)
		return
	}
	if b.Hold != nil {
		select {
		case <-b.Hold:
		case <-r.Context().Done():
			return
		}
	}
	if b.Delay > 0 {
		select {
		case <-time.After(b.Delay):
		case <-r.Context().Done():
			// Like a real gateway, execution does not stop because the
			// client went away.
			return
		}
	}
	if b.Drop {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
		return
	}
	if b.Status != 0 {
		writeErr(w, b.Status, b.ErrorCode, b.Message, b.RetryAfter)
		return
	}
	if b.Raw != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, b.Raw)
		return
	}
	content := "OK " + rid
	if b.Content != nil {
		content = *b.Content
	}
	if b.Oversize > 0 {
		content += strings.Repeat("x", b.Oversize)
	}
	g.mu.Lock()
	var usage map[string]any
	if !b.NoUsage {
		if b.Usage != nil {
			usage = map[string]any{}
			if b.Usage.InputTokens != nil {
				usage["prompt_tokens"] = *b.Usage.InputTokens
			}
			if b.Usage.OutputTokens != nil {
				usage["completion_tokens"] = *b.Usage.OutputTokens
			}
			if b.Usage.ContextTokens != nil {
				usage["total_tokens"] = *b.Usage.ContextTokens
				s.usage.ContextTokens = b.Usage.ContextTokens
			}
		} else {
			cur := int64(0)
			if s.usage.ContextTokens != nil {
				cur = *s.usage.ContextTokens
			}
			cur += g.ContextPerMessage
			s.usage.ContextTokens = I64(cur)
			usage = map[string]any{"prompt_tokens": int64(len(last) / 4), "completion_tokens": int64(len(content) / 4), "total_tokens": cur}
		}
	}
	g.mu.Unlock()
	choices := []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}
	if b.NoChoices {
		choices = []map[string]any{}
	}
	resp := map[string]any{"id": "chatcmpl-" + rid, "object": "chat.completion", "created": time.Now().Unix(), "model": in.Model, "choices": choices}
	if usage != nil {
		resp["usage"] = usage
	}
	writeJSON(w, http.StatusOK, resp)
}

// SetToken replaces the required bearer token (credential rotation tests).
func (g *Gateway) SetToken(tok string) {
	g.mu.Lock()
	g.Token = tok
	g.mu.Unlock()
}
