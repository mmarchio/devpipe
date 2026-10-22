package hermes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/gateway/hermes"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/worker"
	"overseer/internal/worker/workertest"
)

func cfgFor(endpoint string, extra string) worker.WorkerConfig {
	pc := fmt.Sprintf(`{"endpoint":%q,"connect_timeout":"500ms","reconnect":{"max_attempts":3,"initial_delay":"1ms","max_delay":"5ms"}%s}`, endpoint, extra)
	return worker.WorkerConfig{Provider: worker.ProviderHermesGateway, Model: "hermes-worker", RequestTimeout: 5 * time.Second,
		ContextWindow: 64000, ProviderConfig: json.RawMessage(pc)}
}

func newWorker(t *testing.T, g *hermestest.Gateway) *hermes.GatewayWorker {
	t.Helper()
	w, err := hermes.New(cfgFor(g.URL, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func connected(t *testing.T, g *hermestest.Gateway) *hermes.GatewayWorker {
	t.Helper()
	w := newWorker(t, g)
	t.Cleanup(func() { w.Close(context.Background()) })
	if err := w.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewSession(context.Background(), worker.NewSessionRequest{ProjectID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	return w
}

func werr(t *testing.T, err error) *worker.WorkerError {
	t.Helper()
	var we *worker.WorkerError
	if !errors.As(err, &we) {
		t.Fatalf("not a WorkerError: %v", err)
	}
	return we
}

// --- P12-26 configuration ---------------------------------------------------

func TestP1226Config(t *testing.T) {
	c, err := hermes.ParseConfig(json.RawMessage(`{"endpoint":"https://gw.local:8443","auth_secret_ref":"HERMES_TOKEN","tls":{"min_version":"1.3"}}`))
	if err != nil || c.ConnectTimeout != hermes.DefaultConnectTimeout || c.MaxResponseBytes != hermes.DefaultMaxResponseBytes {
		t.Fatalf("T12.3.1/T12.95.1 %+v %v", c, err)
	}
	if _, err := hermes.ParseConfig(json.RawMessage(`{"connect_timeout":"1s"}`)); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatal("T12.3.2 missing endpoint")
	}
	if _, err := hermes.ParseConfig(json.RawMessage(`{"endpoint":"http://gw","tls":{"min_version":"1.3"}}`)); err == nil {
		t.Fatal("T12.95.2 tls on http")
	}
	if _, err := hermes.ParseConfig(json.RawMessage(`{"endpoint":"https://gw","tls":{"ca_file":"/nonexistent.pem"}}`)); err == nil {
		t.Fatal("T12.95.2 unreadable CA")
	}
	if _, err := hermes.ParseConfig(json.RawMessage(`{"endpoint":"https://gw","tls":{"min_version":"1.0"}}`)); err == nil {
		t.Fatal("T12.95.2 weak TLS")
	}
	if _, err := hermes.ParseConfig(json.RawMessage(`{"endpoint":"http://gw","connect_timeout":"soon"}`)); err == nil {
		t.Fatal("invalid timeout")
	}
	os.Setenv("P12_SECRET_TOKEN", "tok-SENTINEL-123")
	defer os.Unsetenv("P12_SECRET_TOKEN")
	c, _ = hermes.ParseConfig(json.RawMessage(`{"endpoint":"http://gw","auth_secret_ref":"P12_SECRET_TOKEN"}`))
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "SENTINEL") || strings.Contains(c.String(), "SENTINEL") || !strings.Contains(c.String(), "env:P12_SECRET_TOKEN") {
		t.Fatalf("T12.3.4 secret in serialization: %s / %s", b, c)
	}
	if _, err := hermes.New(cfgFor("http://gw", `,"auth_secret_ref":"P12_UNSET_TOKEN"`), func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("unresolvable credentials accepted")
	}
}

// --- P12-27 translation --------------------------------------------------------

func TestP1227Translate(t *testing.T) {
	req := workertest.Request(uuid.New(), "line1\n  line2\té")
	wu := uuid.New()
	req.WorkUnitID = &wu
	cfg := cfgFor("http://gw", "")
	gr, err := hermes.Translate(req, cfg)
	if err != nil || gr.Content != req.Prompt.Content {
		t.Fatal("T12.11.1/T12.10.4 content altered")
	}
	if gr.Model != "hermes-worker" || gr.RequestID != req.RequestID.String() {
		t.Fatal("T12.11.2/T12.11.3")
	}
	if gr.Metadata["attempt"] != "1" || gr.Metadata["prompt_hash"] != req.Prompt.Hash || gr.Metadata["work_unit_id"] != wu.String() || gr.Metadata["state_key"] != "IMPLEMENT" {
		t.Fatalf("T12.10.1-T12.10.3 %v", gr.Metadata)
	}
	req.Metadata.PromptHash = "other"
	if _, err := hermes.Translate(req, cfg); werr(t, err).Code != worker.CodeInvalidRequest {
		t.Fatal("T12.11.4 inconsistent option must fail clearly")
	}
}

// --- P12-28 / P12-29 / P12-32 normalization + fixtures ------------------------

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var fixtureRID = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func TestP1232Fixtures(t *testing.T) {
	// Successful replies.
	for _, c := range []struct {
		file    string
		content string
		usage   bool
	}{{"normal_completion.json", "COMPLETE\n\nImplemented unit 12.1; go test ./... passed.", false}, {"context_usage.json", "INCOMPLETE", true}} {
		var gr hermes.GatewayResponse
		raw := fixture(t, c.file)
		if err := json.Unmarshal(raw, &gr); err != nil {
			t.Fatal(err)
		}
		r, err := hermes.Normalize(fixtureRID, "hs-0001", gr, raw)
		if err != nil || r.Content != c.content || r.RequestID != fixtureRID {
			t.Fatalf("T12.12.1/T12.12.2 %s: %+v %v", c.file, r, err)
		}
		if c.usage {
			u, _ := r.Usage.Utilization()
			if *r.Usage.InputTokens != 30100 || u.Percent != 51.31 || r.Usage.Estimated {
				t.Fatalf("T12.12.4/T12.13.1 %+v", r.Usage)
			}
		} else if r.Usage.HasAny() {
			t.Fatal("T12.13.2 absent telemetry invented")
		}
		if r.ProviderMetadata["gateway"] != "hermes" {
			t.Fatal("provider metadata")
		}
	}
	// Errors.
	for _, c := range []struct {
		file   string
		status int
		code   worker.WorkerErrorCode
	}{{"rate_limit.json", 429, worker.CodeRateLimited}, {"session_missing.json", 404, worker.CodeSessionLost},
		{"context_limit.json", 413, worker.CodeContextLimit}, {"auth_failed.json", 401, worker.CodeAuthFailed}} {
		e := hermes.NormalizeError(c.status, nil, fixture(t, c.file))
		if e.Code != c.code || e.Delivery != worker.DeliveryReceived || e.ProviderCode == nil || e.Diagnostic == "" {
			t.Fatalf("T12.38.x %s: %+v", c.file, e)
		}
		if c.code == worker.CodeRateLimited && (e.RetryAfter == nil || *e.RetryAfter != 30*time.Second) {
			t.Fatal("T12.59.2 retry-after lost")
		}
	}
	if e := hermes.NormalizeError(500, nil, []byte(`{"error":{"code":"weird_thing"}}`)); e.Code != worker.CodeInternal {
		t.Fatal("T12.38.4 unknown → internal")
	}
	// An intermediary's 502/504 (no gateway envelope) is ambiguous.
	if e := hermes.NormalizeError(502, nil, []byte("<html>Bad Gateway</html>")); e.Delivery != worker.DeliveryUnknown || e.RetryClass() != worker.RetryRecovery {
		t.Fatalf("proxy 502 must be delivery-unknown: %+v", e)
	}
	if e := hermes.NormalizeError(504, nil, nil); e.Code != worker.CodeTimeout || e.Delivery != worker.DeliveryUnknown {
		t.Fatalf("proxy 504: %+v", e)
	}
	if e := hermes.NormalizeError(503, nil, []byte(`{"error":{"code":"unavailable"}}`)); e.Delivery != worker.DeliveryReceived || e.RetryClass() != worker.RetrySafe {
		t.Fatalf("explicit gateway rejection: %+v", e)
	}
	h := http.Header{}
	h.Set("Retry-After", "7")
	if e := hermes.NormalizeError(429, h, []byte("{}")); *e.RetryAfter != 7*time.Second {
		t.Fatal("Retry-After header")
	}
	// Malformed + incomplete + wrong correlation → PROTOCOL_ERROR, no content.
	raw := fixture(t, "malformed.json")
	var gr hermes.GatewayResponse
	if json.Unmarshal(raw, &gr) == nil {
		t.Fatal("fixture should be malformed")
	}
	if _, err := hermes.Normalize(fixtureRID, "hs-0001", hermes.GatewayResponse{}, raw); werr(t, err).Code != worker.CodeProtocol || werr(t, err).Diagnostic == "" {
		t.Fatal("T12.92.1/T12.92.3")
	}
	json.Unmarshal(fixture(t, "normal_completion.json"), &gr)
	if r, err := hermes.Normalize(uuid.New(), "hs-0001", gr, nil); err == nil || r.Content != "" {
		t.Fatal("T12.32.3/T12.92.2 uncorrelated response attached")
	}
	// Secrets never survive into diagnostics.
	e := hermes.NormalizeError(401, nil, []byte(`{"error":{"code":"auth_failed","message":"token tok-XYZ rejected"}}`), "tok-XYZ")
	if strings.Contains(e.Message+e.Diagnostic, "tok-XYZ") {
		t.Fatal("T12.39.3")
	}
}

// Every message is an OpenAI chat completion: POST /v1/chat/completions
// with {"model","messages","stream":false}; the session starts with the
// prompt "change working directory to <path>", every later prompt starts
// with that line, and the history is resent.
func TestP1232ChatCompletionsWire(t *testing.T) {
	type call struct {
		path, auth, session string
		body                map[string]any
	}
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, call{r.Method + " " + r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(hermes.HeaderSessionID), body})
		n := len(calls)
		mu.Unlock()
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"object":"list","data":[{"id":"hermes-agent"}]}`)
			return
		}
		fmt.Fprintf(w, `{"id":"chatcmpl-%d","object":"chat.completion","model":"swift-qwen3.8","choices":[{"index":0,"message":{"role":"assistant","content":"reply %d"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, n, n)
	}))
	defer srv.Close()
	w, err := hermes.New(cfgFor(srv.URL, `,"auth_secret_ref":"HERMES_TOKEN"`), func(string) (string, bool) { return "tok-1", true })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ctx := context.Background()
	if err := w.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/a"}); err != nil {
		t.Fatal(err)
	}
	r1, err := w.Send(ctx, workertest.Request(uuid.New(), "first"))
	if err != nil || r1.Content != "reply 3" || *r1.Usage.InputTokens != 10 || *r1.Usage.ContextTokens != 15 || r1.ProviderMetadata["completion_id"] != "chatcmpl-3" {
		t.Fatalf("first: %+v %v", r1, err)
	}
	if _, err := w.Send(ctx, workertest.Request(uuid.New(), "second")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var paths []string
	for _, c := range calls {
		paths = append(paths, c.path)
		if c.auth != "Bearer tok-1" {
			t.Fatalf("auth header %q", c.auth)
		}
	}
	if strings.Join(paths, ",") != "GET /v1/models,POST /v1/chat/completions,POST /v1/chat/completions,POST /v1/chat/completions" {
		t.Fatalf("paths %v", paths)
	}
	if cd, _ := json.Marshal(calls[1].body["messages"]); string(cd) != `[{"content":"change working directory to /repos/a","role":"user"}]` {
		t.Fatalf("session start prompt %s", cd)
	}
	last := calls[3]
	keys := make([]string, 0, len(last.body))
	for k := range last.body {
		keys = append(keys, k)
	}
	if len(keys) != 3 || last.body["model"] != "hermes-worker" || last.body["stream"] != false || last.session == "" {
		t.Fatalf("body %v", last.body)
	}
	got, _ := json.Marshal(last.body["messages"])
	want := `[{"content":"change working directory to /repos/a","role":"user"},{"content":"reply 2","role":"assistant"},{"content":"change working directory to /repos/a\n\nfirst","role":"user"},{"content":"reply 3","role":"assistant"},{"content":"change working directory to /repos/a\n\nsecond","role":"user"}]`
	if string(got) != want {
		t.Fatalf("messages\n got %s\nwant %s", got, want)
	}
}

// OpenAI-style error envelopes (null / numeric code, type) normalize.
func TestP1232OpenAIErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		code   worker.WorkerErrorCode
	}{
		{404, `{"error":{"message":"Not Found","type":"invalid_request_error","code":null}}`, worker.CodeInternal},
		{400, `{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`, worker.CodeContextLimit},
		{401, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, worker.CodeAuthFailed},
		{429, `{"error":{"message":"slow down","type":"rate_limit_error","code":429}}`, worker.CodeRateLimited},
		{400, `{"error":{"message":"bad","type":"invalid_request_error"}}`, worker.CodeInvalidRequest},
	} {
		if e := hermes.NormalizeError(c.status, nil, []byte(c.body)); e.Code != c.code || e.Delivery != worker.DeliveryReceived {
			t.Fatalf("%s: %+v", c.body, e)
		}
	}
}

// --- P12-31 fake determinism ----------------------------------------------------

func TestP1231FakeDeterministic(t *testing.T) {
	run := func() []string {
		g := hermestest.New()
		defer g.Close()
		w := connected(t, g)
		g.Script(hermestest.Behavior{Status: 429, ErrorCode: "rate_limited", RetryAfter: 3}, hermestest.Behavior{}, hermestest.Behavior{Status: 413, ErrorCode: "context_limit_exceeded"})
		var out []string
		for i := 0; i < 3; i++ {
			r, err := w.Send(context.Background(), workertest.Request(uuid.New(), "p"))
			if err != nil {
				out = append(out, string(werr(t, err).Code))
			} else {
				out = append(out, fmt.Sprint(*r.Usage.ContextTokens))
			}
		}
		return out
	}
	a, b := run(), run()
	if fmt.Sprint(a) != fmt.Sprint(b) || a[0] != "RATE_LIMITED" || a[2] != "CONTEXT_LIMIT" {
		t.Fatalf("fake not deterministic: %v vs %v", a, b)
	}
}

// --- P12-33..P12-39 adapter behaviour ------------------------------------------

func TestP1233ConnectionLifecycle(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := newWorker(t, g)
	var mu sync.Mutex
	var trans []string
	w.OnConnectionChange(func(from, to worker.ConnectionState, err error) {
		mu.Lock()
		trans = append(trans, string(from)+">"+string(to))
		mu.Unlock()
	})
	if w.ConnectionState() != worker.ConnDisconnected {
		t.Fatal("T12.6.1")
	}
	if _, err := w.Capabilities(context.Background()); err == nil {
		t.Fatal("capabilities before connect")
	}
	if err := w.Connect(context.Background()); err != nil || w.ConnectionState() != worker.ConnConnected {
		t.Fatalf("T12.5.1 %v", err)
	}
	c1, _ := w.Capabilities(context.Background())
	c2, _ := w.Capabilities(context.Background())
	if c1 != c2 || c1.Provider != worker.ProviderHermesGateway || c1.ProviderVersion != "hermes-fake-1.0" || c1.ProtocolVersion != "1" || !c1.NewSession || !c1.ChangeDirectory {
		t.Fatalf("T12.7.1/T12.7.3/T12.8.1/T12.8.2 %+v", c1)
	}
	w.Close(context.Background())
	if w.ConnectionState() != worker.ConnClosed {
		t.Fatal("T12.6.3")
	}
	mu.Lock()
	if strings.Join(trans, ",") != "DISCONNECTED>CONNECTING,CONNECTING>CONNECTED,CONNECTED>CLOSING,CLOSING>CLOSED" {
		t.Fatalf("T12.6.2 %v", trans)
	}
	mu.Unlock()

	// Optional version missing.
	g.ProviderVersion = ""
	w2 := newWorker(t, g)
	w2.Connect(context.Background())
	if c, _ := w2.Capabilities(context.Background()); c.ProviderVersion != "" {
		t.Fatal("T12.8.3")
	}
	w2.Close(context.Background())

	// Refused connection.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	w3, _ := hermes.New(cfgFor(url, ""), nil)
	err := w3.Connect(context.Background())
	if we := werr(t, err); we.Code != worker.CodeUnavailable || we.Delivery != worker.DeliveryNotSent || w3.ConnectionState() != worker.ConnFailed || w3.ConnectionState().Healthy() {
		t.Fatalf("T12.5.2/T12.5.4 %v %s", err, w3.ConnectionState())
	}
	// Gateway unhealthy.
	g.FailHealth(503)
	w4 := newWorker(t, g)
	if err := w4.Connect(context.Background()); err == nil || w4.ConnectionState() != worker.ConnFailed {
		t.Fatal("T12.5.4 unhealthy gateway")
	}
}

func TestP1233ConnectTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	w, _ := hermes.New(cfgFor(slow.URL, ""), nil)
	start := time.Now()
	err := w.Connect(context.Background())
	if werr(t, err).Code != worker.CodeTimeout || time.Since(start) > 2*time.Second {
		t.Fatalf("T12.5.3 %v after %v", err, time.Since(start))
	}
}

func TestP1234NewSession(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	a, _ := w.Session(ctx)
	if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/a"}); err != nil {
		t.Fatal(err)
	}
	before := len(g.Paths())
	b, err := w.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()})
	if err != nil || b.ID == a.ID || b.ID == "" {
		t.Fatal("T12.19.1-T12.19.3/T12.20.1")
	}
	if b.WorkingDirectory != nil {
		t.Fatal("T12.26.1 new session inherited the CWD")
	}
	cur, _ := w.Session(ctx)
	if cur.ID != b.ID {
		t.Fatal("current session not switched")
	}
	// Sessions are client-held: creating one sends nothing.
	if after := g.Paths(); len(after) != before {
		t.Fatalf("new session reached the gateway: %v", after[before:])
	}
}

func TestP1235ChangeDirectory(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/a"}); err != nil {
		t.Fatal(err)
	}
	s, _ := w.Session(ctx)
	if *s.WorkingDirectory != "/repos/a" || *g.SessionCWD(s.ID) != "/repos/a" || strings.Join(g.CDs(), ",") != "/repos/a" || len(g.Messages()) != 0 {
		t.Fatal("T12.23.1/T12.23.2")
	}
	// The cd prompt is answered by the agent: a failure is reported and the
	// confirmed directory stays unchanged (WBS 12.62).
	g.FailCD("/repos/missing")
	err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/missing"})
	if we := werr(t, err); we.Code != worker.CodeInvalidRequest || !strings.Contains(we.Message, "No such file") {
		t.Fatalf("T12.23.3 %v", err)
	}
	if s, _ := w.Session(ctx); *s.WorkingDirectory != "/repos/a" {
		t.Fatal("T12.62.2 failed cd changed the confirmed CWD")
	}
	if err := w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "relative"}); werr(t, err).Delivery != worker.DeliveryNotSent {
		t.Fatal("local validation must reject before sending")
	}
	sandboxed, _ := hermes.New(cfgFor(g.URL, `,"allowed_roots":["/repos"]`), nil)
	sandboxed.Connect(ctx)
	sandboxed.NewSession(ctx, worker.NewSessionRequest{ProjectID: uuid.New()})
	defer sandboxed.Close(ctx)
	if err := sandboxed.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/etc"}); err == nil {
		t.Fatal("sandbox not enforced")
	}
}

func TestP1236Send(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	s, _ := w.Session(ctx)
	g.Script(hermestest.Behavior{Content: ptr(`{"status":"complete"}`)})
	req := workertest.Request(uuid.New(), "implement 12.28")
	r, err := w.Send(ctx, req)
	if err != nil || r.Content != `{"status":"complete"}` || r.RequestID != req.RequestID || r.Session.ID != s.ID {
		t.Fatalf("T12.28.1-T12.28.4 %+v %v", r, err)
	}
	msgs := g.MessagesFor(s.ID)
	if len(msgs) != 1 || msgs[0].RequestID != req.RequestID.String() || msgs[0].Content != req.Prompt.Content || msgs[0].DispatchKey == "" {
		t.Fatalf("T12.28.2/T12.32.1 %+v", msgs)
	}
	if !r.Usage.HasContext() || r.Usage.Estimated {
		t.Fatal("usage")
	}
	// Timeout after transmission → REQUEST_TIMEOUT + DELIVERY_UNKNOWN, cancel attempted.
	g.Script(hermestest.Behavior{Delay: 3 * time.Second})
	tctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = w.Send(tctx, workertest.Request(uuid.New(), "slow"))
	cancel()
	if we := werr(t, err); we.Code != worker.CodeTimeout || we.Delivery != worker.DeliveryUnknown || worker.OutcomeFor(we) != worker.DispatchUnknown {
		t.Fatalf("T12.36.1-T12.36.3 %+v", we)
	}
	// In-flight cancellation: the chat completions API cannot confirm a
	// cancel, so the delivery stays unknown.
	g.Script(hermestest.Behavior{Delay: 3 * time.Second})
	cctx, ccancel := context.WithCancel(ctx)
	go func() { time.Sleep(150 * time.Millisecond); ccancel() }()
	_, err = w.Send(cctx, workertest.Request(uuid.New(), "cancel me"))
	if we := werr(t, err); we.Code != worker.CodeCancelled || we.Delivery != worker.DeliveryUnknown {
		t.Fatalf("T12.37.2 %+v", we)
	}
	// Pre-send cancellation: nothing transmitted.
	before := len(g.Messages())
	dead, dc := context.WithCancel(ctx)
	dc()
	if _, err := w.Send(dead, workertest.Request(uuid.New(), "x")); werr(t, err).Delivery != worker.DeliveryNotSent || len(g.Messages()) != before {
		t.Fatal("T12.37.1")
	}
}

func TestP1236DeliveryClassification(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	// Connection dropped after the request was received: ambiguous, one send.
	g.Script(hermestest.Behavior{Drop: true})
	before := len(g.Messages())
	_, err := w.Send(ctx, workertest.Request(uuid.New(), "engineer prompt"))
	we := werr(t, err)
	if we.Delivery != worker.DeliveryUnknown || we.RetryClass() != worker.RetryRecovery || we.Retryable {
		t.Fatalf("T12.34.3/T12.35.2 %+v", we)
	}
	if n := len(g.Messages()) - before; n != 1 {
		t.Fatalf("T12.89.1 prompt sent %d times", n)
	}
	// Pre-send failure: gateway gone.
	w.Reconnect(ctx)
	g.Close()
	time.Sleep(50 * time.Millisecond)
	_, err = w.Send(ctx, workertest.Request(uuid.New(), "x"))
	if we := werr(t, err); we.Delivery != worker.DeliveryNotSent || we.RetryClass() != worker.RetrySafe {
		t.Fatalf("T12.34.1/T12.35.1 %+v", we)
	}
}

func TestP1236ErrorReplies(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	s, _ := w.Session(ctx)
	g.Script(hermestest.Behavior{Status: 413, ErrorCode: "context_limit_exceeded"}, hermestest.Behavior{Raw: `{"choices":`},
		hermestest.Behavior{NoChoices: true}, hermestest.Behavior{Oversize: 5 << 20})
	for _, want := range []worker.WorkerErrorCode{worker.CodeContextLimit, worker.CodeProtocol, worker.CodeProtocol, worker.CodeProtocol} {
		_, err := w.Send(ctx, workertest.Request(uuid.New(), "x"))
		if we := werr(t, err); we.Code != want || we.Delivery != worker.DeliveryReceived || worker.OutcomeFor(we) != worker.DispatchFailed {
			t.Fatalf("T12.60.1/T12.92.1/T12.93.2 want %s got %+v", want, we)
		}
	}
	if cur, _ := w.Session(ctx); cur.ID != s.ID {
		t.Fatal("T12.60.3 adapter altered the session")
	}
	// Session missing: SESSION_LOST, no replacement.
	g.KillSession(s.ID)
	sessions := len(g.Sessions())
	_, err := w.Send(ctx, workertest.Request(uuid.New(), "x"))
	if werr(t, err).Code != worker.CodeSessionLost || len(g.Sessions()) != sessions {
		t.Fatal("T12.61.1/T12.61.3")
	}
	if _, err := w.Session(ctx); err == nil {
		t.Fatal("lost session still current")
	}
}

func TestP1236OneRequestPerSession(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	hold := make(chan struct{})
	g.Script(hermestest.Behavior{Hold: hold})
	done := make(chan error, 1)
	go func() {
		_, err := w.Send(context.Background(), workertest.Request(uuid.New(), "first"))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_, err := w.Send(context.Background(), workertest.Request(uuid.New(), "second"))
	if we := werr(t, err); we.Code != worker.CodeBusy || we.Delivery != worker.DeliveryNotSent {
		t.Fatalf("T12.56.1 %+v", we)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(g.Messages()) != 1 {
		t.Fatal("second request reached the gateway")
	}
}

func TestP1237ReconnectAndReconcile(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	s, _ := w.Session(ctx)
	var mu sync.Mutex
	var trans []string
	w.OnConnectionChange(func(from, to worker.ConnectionState, err error) {
		mu.Lock()
		trans = append(trans, string(from)+">"+string(to))
		mu.Unlock()
	})
	// Bounded attempts while the gateway is unhealthy.
	g.FailHealth(503)
	checks := g.HealthChecks()
	if err := w.Reconnect(ctx); err == nil || w.ConnectionState() != worker.ConnFailed {
		t.Fatal("T12.40.1 reconnect succeeded against a failing gateway")
	}
	if n := g.HealthChecks() - checks; n != 3 {
		t.Fatalf("T12.40.1 attempts %d", n)
	}
	g.FailHealth(0)
	msgs := len(g.Messages())
	if err := w.Reconnect(ctx); err != nil || w.ConnectionState() != worker.ConnConnected {
		t.Fatal("T12.40.3")
	}
	if len(g.Messages()) != msgs {
		t.Fatal("T12.40.4 reconnect replayed a request")
	}
	mu.Lock()
	if trans[len(trans)-1] != "RECONNECTING>CONNECTED" {
		t.Fatalf("transitions %v", trans)
	}
	mu.Unlock()
	rec, _ := w.ReconcileSession(ctx, worker.SessionInfo{ID: s.ID})
	if rec.Result != worker.ReconcileConfirmed {
		t.Fatal("T12.41.1/T12.42.1")
	}
	// A session the gateway rejects is LOST (T12.41.2/T12.42.2): the next
	// send reports it and the adapter drops it.
	g.KillSession(s.ID)
	if _, err := w.Send(ctx, workertest.Request(uuid.New(), "x")); werr(t, err).Code != worker.CodeSessionLost {
		t.Fatal("T12.41.2/T12.41.3/T12.42.2")
	}
	if _, err := w.Session(ctx); err == nil {
		t.Fatal("lost session still current")
	}
	idle := newWorker(t, g)
	rec, _ = idle.ReconcileSession(ctx, worker.SessionInfo{ID: s.ID})
	if rec.Result != worker.ReconcileUnknown {
		t.Fatal("T12.42.3")
	}
}

// A fresh adapter after a restart (or another overseer instance) adopts a
// persisted session: same identity, empty client-held history and no
// confirmed working directory — the directory is restored before work
// continues (WBS 12.43/12.75).
func TestP1237AdoptAfterRestart(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/a"})
	s, _ := w.Session(ctx)
	w.Close(ctx)
	w2 := newWorker(t, g)
	defer w2.Close(ctx)
	w2.Connect(ctx)
	rec, _ := w2.ReconcileSession(ctx, worker.SessionInfo{ID: s.ID, CreatedAt: s.CreatedAt})
	cur, err := w2.Session(ctx)
	if rec.Result != worker.ReconcileConfirmed || err != nil || cur.ID != s.ID || cur.WorkingDirectory != nil {
		t.Fatalf("adopt: %+v %+v %v", rec, cur, err)
	}
	if err := w2.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{Path: "/repos/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Send(ctx, workertest.Request(uuid.New(), "continue")); err != nil {
		t.Fatal(err)
	}
	if cds := g.CDs(); len(cds) != 2 || cds[1] != "/repos/a" {
		t.Fatalf("directory not restored after adoption: %v", cds)
	}
}

// Streaming and provider cancellation are not advertised: every message
// is a non-streaming chat completion.
func TestP1238NoStreaming(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	if c, _ := w.Capabilities(ctx); c.Streaming || c.Cancellation {
		t.Fatalf("capabilities %+v", c)
	}
	if _, err := w.Stream(ctx, workertest.Request(uuid.New(), "s")); err == nil {
		t.Fatal("T12.45.2 stream without capability")
	}
	if _, err := w.Send(ctx, workertest.Request(uuid.New(), "s")); err != nil {
		t.Fatal(err)
	}
	if m := g.Messages(); len(m) != 1 || m[0].Stream {
		t.Fatalf("messages %+v", m)
	}
}

func TestP1239Close(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	w := connected(t, g)
	ctx := context.Background()
	s, _ := w.Session(ctx)
	g.Script(hermestest.Behavior{Delay: 5 * time.Second})
	res := make(chan error, 1)
	go func() {
		_, err := w.Send(ctx, workertest.Request(uuid.New(), "in flight"))
		res <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second || w.ConnectionState() != worker.ConnClosed {
		t.Fatal("T12.66.2/T12.66.3")
	}
	if err := <-res; err == nil {
		t.Fatal("in-flight request not cancelled")
	}
	if _, err := w.Send(ctx, workertest.Request(uuid.New(), "late")); werr(t, err).Code != worker.CodeClosed {
		t.Fatal("T12.66.1")
	}
	found := false
	for _, id := range g.Sessions() {
		found = found || id == s.ID
	}
	if !found {
		t.Fatal("close destroyed the external session")
	}
}

func TestP1294Auth(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	g.Token = "tok-AUTH-SENTINEL"
	resolve := func(ref string) (string, bool) {
		if ref == "HERMES_TOKEN" {
			return "tok-AUTH-SENTINEL", true
		}
		return "", false
	}
	w, err := hermes.New(cfgFor(g.URL, `,"auth_secret_ref":"HERMES_TOKEN"`), resolve)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	if err := w.Connect(context.Background()); err != nil {
		t.Fatal("T12.94.1", err)
	}
	if _, err := w.NewSession(context.Background(), worker.NewSessionRequest{ProjectID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	g.Token = "rotated"
	_, err = w.Send(context.Background(), workertest.Request(uuid.New(), "x"))
	we := werr(t, err)
	if we.Code != worker.CodeAuthFailed || strings.Contains(fmt.Sprintf("%v %+v %s", err, we, we.Diagnostic), "tok-AUTH-SENTINEL") {
		t.Fatalf("T12.94.2 %+v", we)
	}
}

// T12.69.1/T12.69.3: the Hermes adapter passes the generic contract.
func TestP1240HermesContract(t *testing.T) {
	g := hermestest.New()
	defer g.Close()
	workertest.RunContract(t, workertest.Target{
		New:       func(t *testing.T) worker.Worker { return newWorker(t, g) },
		DelayNext: func(d time.Duration) { g.Script(hermestest.Behavior{Delay: d}) },
		Dir:       "/repos/project-a",
	})
}

// T12.2.1: the registry resolves HERMES_GATEWAY to an interface value.
func TestP1240Registry(t *testing.T) {
	reg := worker.NewProviderRegistry()
	hermes.Register(reg, nil)
	w, err := reg.Create(context.Background(), cfgFor("http://127.0.0.1:1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.(worker.Connector); !ok {
		t.Fatal("not a connector")
	}
	bad := cfgFor("", "")
	if _, err := reg.Create(context.Background(), bad); err == nil {
		t.Fatal("T12.2.4")
	}
}

func ptr(s string) *string { return &s }
