// Package providertest is a fake OpenAI-compatible chat-completions server
// for tests: it runs the deterministic heuristic compressor over the
// structured input it receives, or replays scripted failures. No model is
// ever called.
package providertest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"overseer/internal/compression/compressiontest"
)

// Reply is one scripted server behaviour.
type Reply struct {
	// Status != 0 returns that status with Body.
	Status int
	Body   string
	// Edit, when set, post-processes the heuristic output.
	Edit func(string) string
	// Raw, when set, is returned verbatim as the 200 body.
	Raw string
	// Delay, when set, is slept before answering.
	Delay time.Duration
}

// Request is one recorded request.
type Request struct {
	Authorization string
	Model         string
	System        string
	User          string
	Body          string
}

// Server is the fake provider.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	replies  []Reply
	base     int // request index the current script starts at
	requests []Request
}

// New starts a fake server replaying replies in order (the last repeats;
// none = always the heuristic).
func New(replies ...Reply) *Server {
	s := &Server{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Script replaces the scripted replies from the next request on (the last
// repeats; none = always the heuristic).
func (s *Server) Script(replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies, s.base = replies, len(s.requests)
}

// Requests returns the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls returns the request count.
func (s *Server) Calls() int { return len(s.Requests()) }

// FeedbackMarker starts the retry-feedback suffix of a user message.
const FeedbackMarker = "\n\nThe previous compression attempt was rejected:"

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// The model listing (readiness probe) is answered without recording a
	// request: an empty listing accepts every model.
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	rec := Request{Authorization: r.Header.Get("Authorization"), Model: body.Model, Body: string(raw)}
	for _, m := range body.Messages {
		switch m.Role {
		case "system":
			rec.System = m.Content
		case "user":
			rec.User = m.Content
		}
	}
	s.mu.Lock()
	i := len(s.requests) - s.base
	s.requests = append(s.requests, rec)
	var rep Reply
	if len(s.replies) > 0 {
		if i >= len(s.replies) {
			i = len(s.replies) - 1
		}
		rep = s.replies[i]
	}
	s.mu.Unlock()

	if rep.Delay > 0 {
		time.Sleep(rep.Delay)
	}
	if r.URL.Path != "/v1/chat/completions" && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	if rep.Status != 0 {
		w.WriteHeader(rep.Status)
		_, _ = w.Write([]byte(rep.Body))
		return
	}
	if rep.Raw != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rep.Raw))
		return
	}
	user := rec.User
	if k := strings.Index(user, FeedbackMarker); k >= 0 {
		user = user[:k]
	}
	out, err := compressiontest.HeuristicText(user)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unparseable input"}}`))
		return
	}
	if rep.Edit != nil {
		out = rep.Edit(out)
	}
	resp := map[string]any{
		"id":    "chatcmpl-fake-" + itoa(len(s.Requests())),
		"model": body.Model,
		"choices": []map[string]any{{
			"message":       map[string]string{"role": "assistant", "content": out},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": (len(user) + 3) / 4, "completion_tokens": (len(out) + 3) / 4},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
