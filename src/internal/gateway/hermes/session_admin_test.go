package hermes_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/gateway/hermes"
	"overseer/internal/worker"
	"overseer/internal/worker/workertest"
)

// Each overseer session is its own Hermes session: requests carry the
// session id in Hermes's continuity header, the session's counters are read
// from /api/sessions/{id}, and a completed session is deleted (an unknown
// one counts as already gone).
func TestHermesSessionAdmin(t *testing.T) {
	var mu sync.Mutex
	var hermesIDs, deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"hermes-agent"}]}`)
		case r.URL.Path == "/v1/chat/completions":
			hermesIDs = append(hermesIDs, r.Header.Get(hermes.HeaderHermesSessionID))
			io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		case strings.HasPrefix(r.URL.Path, hermes.PathSessions) && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, hermes.PathSessions)
			io.WriteString(w, `{"object":"hermes.session","session":{"id":"`+id+`","message_count":4,"tool_call_count":2,"api_call_count":3,"input_tokens":89,"output_tokens":79,"cache_read_tokens":29000}}`)
		case strings.HasPrefix(r.URL.Path, hermes.PathSessions) && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, hermes.PathSessions)
			if id == "gone" {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"message":"Session not found: gone","code":"session_not_found"}}`)
				return
			}
			deleted = append(deleted, id)
			io.WriteString(w, `{"object":"hermes.session.deleted","id":"`+id+`","deleted":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	w, err := hermes.New(cfgFor(srv.URL, ""), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ctx := context.Background()
	if err := w.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	sess, err := w.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Send(ctx, workertest.Request(uuid.New(), "hello")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ids := append([]string(nil), hermesIDs...)
	mu.Unlock()
	if len(ids) != 1 || ids[0] != sess.ID {
		t.Fatalf("Hermes session header %q, want %q", ids, sess.ID)
	}
	m, ok := any(w).(worker.SessionManager)
	if !ok {
		t.Fatal("the Hermes worker is not a SessionManager")
	}
	st, err := m.SessionStats(ctx, sess.ID)
	if err != nil || st.Messages != 4 || st.ToolCalls != 2 || st.ModelCalls != 3 || st.InputTokens != 89 || st.OutputTokens != 79 || st.CacheReadTokens != 29000 {
		t.Fatalf("stats %+v %v", st, err)
	}
	if err := m.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSession(ctx, "gone"); err != nil {
		t.Fatalf("deleting an unknown session: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != sess.ID {
		t.Fatalf("deleted %q", deleted)
	}
}
