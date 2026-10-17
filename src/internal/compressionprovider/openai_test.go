package compressionprovider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/prompt"
)

const sentinelKey = "sk-SENTINEL-9f8e7d6c5b4a"

func request(t *testing.T) compression.CompressionRequest {
	t.Helper()
	comps := []compression.CompressionComponent{
		{Ordinal: 0, Type: prompt.ComponentTaskContext, Policy: prompt.PolicyPreserveExact, Content: "go test ./..."},
		{Ordinal: 1, Type: prompt.ComponentProjectContext, Policy: prompt.PolicyCompressible, Content: "long history\nmore history"},
	}
	si := compression.RenderStructuredInput(comps)
	req, err := compression.NewCompressionRequest(compression.CompressionRequest{
		CanonicalPromptID: uuid.New(), Content: si.Text, Components: comps, TargetTokens: 100,
		Model: "qwen2.5-7b", Attempt: 2, InstructionContent: compression.DefaultInstructionContent,
		Feedback: "The previous compression attempt was rejected:\n- AC-3 missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestT9391To9393RequestAndTranslation(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	c, err := compressionprovider.New(compressionprovider.Options{Name: "remote", BaseURL: srv.URL + "/v1", APIKey: sentinelKey})
	if err != nil {
		t.Fatal(err)
	}
	req := request(t)
	resp, err := c.Compress(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := srv.Requests()[0]
	if got.Model != "qwen2.5-7b" || got.System != compression.DefaultInstructionContent ||
		!strings.HasPrefix(got.User, req.Content) || !strings.Contains(got.User, "AC-3 missing") {
		t.Fatalf("T9.39.1 request %+v", got)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(got.Body), &raw)
	if raw["temperature"] != float64(0) {
		t.Fatal("temperature must be 0")
	}
	if got.Authorization != "Bearer "+sentinelKey {
		t.Fatal("T9.39.2 auth not applied")
	}
	if resp.Content != "go test ./...\nlong history" || resp.Provider != "remote" || resp.Model != "qwen2.5-7b" ||
		resp.InputTokens == 0 || resp.OutputTokens == 0 || resp.FinishReason != "stop" || resp.ProviderRequestID == "" || resp.Duration <= 0 {
		t.Fatalf("T9.39.3 translation %+v", resp)
	}
}

func TestT9394ErrorTranslationAndRedaction(t *testing.T) {
	cases := []struct {
		reply providertest.Reply
		kind  compression.ErrorKind
	}{
		{providertest.Reply{Status: 429, Body: `{"error":{"message":"rate limited"}}`}, compression.ErrorRateLimit},
		{providertest.Reply{Status: 401, Body: `{"error":{"message":"invalid key ` + sentinelKey + `"}}`}, compression.ErrorAuth},
		{providertest.Reply{Status: 503, Body: `upstream down`}, compression.ErrorUnavailable},
		{providertest.Reply{Status: 400, Body: `{"error":{"code":"context_length_exceeded","message":"maximum context length is 8192"}}`}, compression.ErrorContextOverflow},
		{providertest.Reply{Raw: `{not json`}, compression.ErrorMalformed},
		{providertest.Reply{Raw: `{"choices":[{"message":{"content":"  "}}]}`}, compression.ErrorEmpty},
		{providertest.Reply{Raw: `{"choices":[]}`}, compression.ErrorEmpty},
	}
	for _, tc := range cases {
		srv := providertest.New(tc.reply)
		c, _ := compressionprovider.New(compressionprovider.Options{Name: "p", BaseURL: srv.URL + "/v1", APIKey: sentinelKey})
		_, err := c.Compress(context.Background(), request(t))
		srv.Close()
		if compression.KindOf(err) != tc.kind {
			t.Fatalf("T9.37/T9.39.4 %v: got %v (%v)", tc.reply, compression.KindOf(err), err)
		}
		if strings.Contains(err.Error(), sentinelKey) {
			t.Fatal("T9.41.4 credential leaked into error")
		}
	}
	if !compression.ErrorRateLimit.Retryable() || compression.ErrorAuth.Retryable() {
		t.Fatal("T9.37.2/T9.37.3")
	}
	// Network failure: nothing listening.
	c, _ := compressionprovider.New(compressionprovider.Options{Name: "p", BaseURL: "http://127.0.0.1:1/v1", APIKey: sentinelKey})
	_, err := c.Compress(context.Background(), request(t))
	if compression.KindOf(err) != compression.ErrorNetwork || strings.Contains(err.Error(), sentinelKey) {
		t.Fatalf("T9.37.1 %v", err)
	}
	if strings.Contains(c.String(), sentinelKey) {
		t.Fatal("T9.39.5 credential in String()")
	}
}

func TestT936ProviderTimeoutCancelsRequest(t *testing.T) {
	block := make(chan struct{})
	srv := providertest.New()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-block:
		}
	})
	defer srv.Close()
	defer close(block)
	c, _ := compressionprovider.New(compressionprovider.Options{Name: "p", BaseURL: srv.URL + "/v1"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Compress(ctx, request(t))
	if compression.KindOf(err) != compression.ErrorTimeout {
		t.Fatalf("timeout kind: %v", err)
	}
}

func TestT940LocalAndFactory(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	f := compressionprovider.NewFactory(map[string]config.CompressionProviderConfig{
		"local":  {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1"},
		"remote": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1", APIKeyEnv: "REMOTE_KEY"},
	}).WithEnv(func(k string) string {
		if k == "REMOTE_KEY" {
			return sentinelKey
		}
		return ""
	})
	var outs []string
	for _, name := range []string{"local", "remote"} {
		c, err := f.Compressor(name)
		if err != nil {
			t.Fatalf("T9.40.4 %s: %v", name, err)
		}
		var _ compression.Compressor = c // T9.40.1
		resp, err := c.Compress(context.Background(), request(t))
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, resp.Content)
	}
	if outs[0] != outs[1] {
		t.Fatal("T9.40.3 local and remote not interchangeable")
	}
	reqs := srv.Requests()
	if reqs[0].Authorization != "" || reqs[1].Authorization != "Bearer "+sentinelKey {
		t.Fatal("T9.40.2 local must send no key")
	}
	if _, err := f.Compressor("missing"); compression.KindOf(err) != compression.ErrorConfiguration {
		t.Fatal("unknown provider")
	}
	f2 := compressionprovider.NewFactory(map[string]config.CompressionProviderConfig{
		"remote": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL, APIKeyEnv: "UNSET_KEY_VAR"},
	}).WithEnv(func(string) string { return "" })
	if _, err := f2.Compressor("remote"); compression.KindOf(err) != compression.ErrorConfiguration {
		t.Fatal("missing key not a configuration error")
	}
}

func TestLLMSemanticValidator(t *testing.T) {
	comps := []compression.CompressionComponent{{Ordinal: 0, Type: prompt.ComponentSystem, Policy: prompt.PolicyPreserveSemantics, Content: "Do not add dependencies."}}
	for _, tc := range []struct {
		raw    string
		passed bool
		isErr  bool
	}{
		{`{"choices":[{"message":{"content":"{\"passed\":true,\"confidence\":0.9}"}}]}`, true, false},
		{`{"choices":[{"message":{"content":"{\"passed\":true,\"added_requirements\":[\"use MongoDB\"]}"}}]}`, false, false},
		{`{"choices":[{"message":{"content":"looks fine to me"}}]}`, false, true},
	} {
		srv := providertest.New(providertest.Reply{Raw: tc.raw})
		c, _ := compressionprovider.New(compressionprovider.Options{Name: "p", BaseURL: srv.URL})
		v := &compressionprovider.LLMSemanticValidator{Client: c, Model: "judge"}
		res, err := v.Validate(context.Background(), comps, "No new deps.")
		srv.Close()
		if tc.isErr {
			if err == nil || !strings.Contains(err.Error(), "semantic validation") {
				t.Fatalf("malformed verdict accepted: %+v", res)
			}
			continue
		}
		if err != nil || res.Passed != tc.passed {
			t.Fatalf("verdict %s: %+v %v", tc.raw, res, err)
		}
	}
}
