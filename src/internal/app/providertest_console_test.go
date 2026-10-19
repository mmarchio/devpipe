package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"overseer/internal/config"
)

// The console connection test sends "list your capabilities" to the
// configured model as one chat completion, for a worker gateway (fresh
// client-held session) and a compressor alike.
func TestProviderTestSendsCapabilitiesPrompt(t *testing.T) {
	var got []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			w.Write([]byte(`{"object":"list","data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var m struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Stream *bool `json:"stream"`
			}
			json.NewDecoder(r.Body).Decode(&m)
			if len(m.Messages) != 1 || m.Messages[0].Role != "user" || m.Stream == nil || *m.Stream {
				http.Error(w, `{"error":{"message":"bad body"}}`, http.StatusBadRequest)
				return
			}
			got = append(got, m.Model+"|"+m.Messages[0].Content)
			w.Write([]byte(`{"id":"c0","model":"` + m.Model + `","choices":[{"index":0,"message":{"role":"assistant","content":"I can edit files."},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gw.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m.Model+"|"+m.Messages[len(m.Messages)-1].Content)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","model":"` + m.Model + `","choices":[{"message":{"content":"I compress prompts."},"finish_reason":"stop"}]}`))
	}))
	defer llm.Close()

	a := &App{secrets: newSecretSource(), cfg: config.Config{
		WorkerProviders:      map[string]config.WorkerProviderConfig{"hermes": {Kind: config.WorkerKindHermesGateway, Endpoint: gw.URL}},
		CompressionProviders: map[string]config.CompressionProviderConfig{"local": {Kind: "openai_compatible", BaseURL: llm.URL + "/v1"}},
	}}
	a.providers = config.NewProviderStore(a.cfg)
	ctx := t.Context()
	res, found := a.providerTest(ctx, "worker", "hermes", "swift-qwen-3.8")
	if !found || !res.OK || res.Response != "I can edit files." {
		t.Fatalf("worker: %+v %v", res, found)
	}
	res, found = a.providerTest(ctx, "compression", "local", "qwen-small")
	if !found || !res.OK || res.Response != "I compress prompts." {
		t.Fatalf("compression: %+v %v", res, found)
	}
	if strings.Join(got, ",") != "swift-qwen-3.8|list your capabilities,qwen-small|list your capabilities" {
		t.Fatalf("sent: %v", got)
	}
	if _, found := a.providerTest(ctx, "worker", "nope", "m"); found {
		t.Fatal("unknown provider found")
	}
	// A passed test marks the readiness session check; a later failure
	// clears it.
	if !a.providerTestPassed("worker", "hermes", "swift-qwen-3.8") || !a.providerTestPassed("compression", "local", "qwen-small") ||
		a.providerTestPassed("worker", "hermes", "other-model") {
		t.Fatal("passed connection tests not recorded")
	}
	gw.Close()
	if res, _ := a.providerTest(ctx, "worker", "hermes", "swift-qwen-3.8"); res.OK || res.Error == "" {
		t.Fatalf("down gateway: %+v", res)
	}
	if a.providerTestPassed("worker", "hermes", "swift-qwen-3.8") {
		t.Fatal("failed connection test left the check passing")
	}
}
