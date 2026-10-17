package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
)

// P9-06 / P9-55..P9-59 / P9-54 REST surface over the full application: the
// app's compression service resolves the project's provider name through
// the configured provider registry, here an OpenAI-compatible fake whose
// API key is read from the environment (a sentinel that must never leak).

const p9SentinelKey = "sk-p9-SENTINEL-4c1d2e3f"

func p9HTTP(t *testing.T, method, url, body string) (int, string, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, string(raw), out
}

// p9APIHarness starts the app with the "fake" provider pointed at srv.
func p9APIHarness(t *testing.T, srv *providertest.Server) *Harness {
	t.Helper()
	t.Setenv("P9_FAKE_KEY", p9SentinelKey)
	return NewHarnessWith(t, func(c *config.Config) {
		c.CompressionProviders = map[string]config.CompressionProviderConfig{
			"fake": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1", APIKeyEnv: "P9_FAKE_KEY"},
		}
	})
}

func p9DropAC3(s string) string { return strings.Replace(s, p9AC3, "", -1) }

func TestP955CompressionAPI(t *testing.T) {
	srv := providertest.New(
		providertest.Reply{Edit: p9DropAC3}, providertest.Reply{}, // preview: fail, pass
		providertest.Reply{Edit: p9DropAC3}, providertest.Reply{}, // prepare: fail, pass
	)
	defer srv.Close()
	h := p9APIHarness(t, srv)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'compressor-7b', context_window = "+itoa(n*4)+" WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	promptURL := base + "/prompts/" + rec.ID.String()

	// --- settings (WBS 9.2) ---
	code, _, body := p9HTTP(t, http.MethodGet, base+"/settings/compression", "")
	if code != 200 || body["mode"] != "AUTO" || body["stored"] != false || body["provider"] != "fake" || body["max_attempts"] != float64(2) {
		t.Fatalf("GET settings defaults: %d %v", code, body)
	}
	code, _, body = p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_ratio":2,"max_attempts":0}`)
	fields, _ := body["error"].(map[string]any)["fields"].(map[string]any)
	if code != 400 || fields["target_ratio"] == nil || fields["max_attempts"] == nil {
		t.Fatalf("PUT invalid settings: %d %v", code, body)
	}
	code, _, body = p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_tokens":`+itoa(n/2)+`,"max_attempts":2,"minimum_savings":0.15}`)
	if code != 200 || body["stored"] != true || body["effective_target_tokens"] != float64(n/2) {
		t.Fatalf("PUT settings: %d %v", code, body)
	}

	// --- preview (WBS 9.45): invokes the compressor, persists nothing ---
	runtimeBefore := p9RuntimeRow(t, h, pos.ProjectID)
	code, raw, body := p9HTTP(t, http.MethodPost, promptURL+"/compression-preview", "")
	if code != 200 || body["decision"] != "COMPRESS" || body["validation_status"] != "PASSED" || body["source"] != "COMPRESSED" {
		t.Fatalf("T9.45.1 preview: %d %s", code, raw)
	}
	if srv.Calls() != 2 || body["attempts"] != float64(2) {
		t.Fatalf("preview attempts: calls=%d", srv.Calls())
	}
	if !strings.Contains(body["compressed_preview"].(string), p9AC3) || body["compressed_tokens"].(float64) >= body["canonical_tokens"].(float64) {
		t.Fatal("T9.45.3 preview content/metrics")
	}
	items, _ := body["preserved_items"].([]any)
	if len(items) == 0 {
		t.Fatal("T9.45.3 preserved-item results missing")
	}
	for _, it := range items {
		if it.(map[string]any)["preserved"] != true {
			t.Fatalf("accepted preview reports an unpreserved item: %v", it)
		}
	}
	if strings.Contains(raw, p9SentinelKey) {
		t.Fatal("T9.45.4 secret in preview")
	}
	if srv.Requests()[0].Authorization != "Bearer "+p9SentinelKey {
		t.Fatal("adapter did not authenticate with the configured key")
	}
	for _, table := range []string{"compression_runs", "compression_attempts", "compressed_prompts", "worker_prompts"} {
		if p9Count(t, h, table, "canonical_prompt_id = '"+rec.ID.String()+"'") != 0 {
			t.Fatalf("T9.45.2 preview persisted into %s", table)
		}
	}
	if p9RuntimeRow(t, h, pos.ProjectID) != runtimeBefore {
		t.Fatal("T9.45.2 runtime state changed")
	}

	// --- prepare (WBS 9.35) ---
	code, raw, body = p9HTTP(t, http.MethodPost, promptURL+"/worker-prompt", "")
	wp, _ := body["worker_prompt"].(map[string]any)
	if code != 200 || wp == nil || wp["source"] != "COMPRESSED" || body["run"].(map[string]any)["outcome"] != "COMPRESSED" {
		t.Fatalf("prepare: %d %s", code, raw)
	}

	// --- attempts (WBS 9.46) ---
	code, raw, body = p9HTTP(t, http.MethodGet, promptURL+"/compression-attempts", "")
	atts, _ := body["attempts"].([]any)
	if code != 200 || len(atts) != 2 {
		t.Fatalf("T9.46.1 attempts: %d %s", code, raw)
	}
	a1, a2 := atts[0].(map[string]any), atts[1].(map[string]any)
	if a1["attempt"] != float64(1) || a2["attempt"] != float64(2) {
		t.Fatal("T9.46.1 order")
	}
	if a1["status"] != "VALIDATION_FAILED" || a1["accepted"] != false || !strings.Contains(raw, `"item_id":"AC-3"`) {
		t.Fatal("T9.46.2 failed attempt")
	}
	if a2["accepted"] != true || a2["model"] != "compressor-7b" || a2["compressed_tokens"].(float64) <= 0 {
		t.Fatal("T9.46.3 accepted attempt")
	}
	if strings.Contains(raw, p9SentinelKey) {
		t.Fatal("secret in attempts")
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, promptURL+"/compression-attempts", "{}"); code != http.StatusMethodNotAllowed {
		t.Fatalf("attempts write method: %d", code)
	}
	other, _, _ := p705Rig(t, h)
	if code, _, _ := p9HTTP(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+other.String()+"/prompts/"+rec.ID.String()+"/compression-attempts", ""); code != 404 {
		t.Fatalf("T9.46.4 project isolation: %d", code)
	}

	// --- diff (WBS 9.48) ---
	code, raw, body = p9HTTP(t, http.MethodGet, promptURL+"/compression-attempts/"+a2["id"].(string)+"/diff", "")
	if code != 200 || body["removed"].(float64) == 0 || body["equal"].(float64) == 0 {
		t.Fatalf("diff: %d %s", code, raw)
	}
	exactSeen := false
	for _, l := range body["lines"].([]any) {
		lm := l.(map[string]any)
		if strings.Contains(lm["text"].(string), p9AC1) {
			exactSeen = true
			if lm["op"] != " " {
				t.Fatal("T9.48.3 exact line shows a difference")
			}
		}
	}
	if !exactSeen {
		t.Fatal("diff lacks the exact line")
	}
	if code, _, _ := p9HTTP(t, http.MethodGet, promptURL+"/compression-attempts/"+uuid.NewString()+"/diff", ""); code != 404 {
		t.Fatal("unknown attempt diff")
	}

	// --- metrics (WBS 9.47) ---
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/compression-metrics", "")
	if code != 200 || body["prompts_compressed"] != float64(1) || body["prompts_bypassed"] != float64(0) ||
		body["validation_failure_rate"] != 0.5 || body["retry_rate"] != float64(1) || body["average_reduction_percent"].(float64) <= 0 {
		t.Fatalf("metrics: %d %s", code, raw)
	}

	// --- lineage (WBS 9.34) ---
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/worker-prompts/"+wp["id"].(string)+"/lineage", "")
	if code != 200 || body["canonical_prompt_id"] != rec.ID.String() || body["context_snapshot_id"] != pos.SnapID.String() ||
		body["accepted_attempt_id"] != a2["id"] || body["compressed_prompt_id"] != wp["compressed_prompt_id"] {
		t.Fatalf("lineage: %d %s", code, raw)
	}

	// Reuse: a repeated preparation is answered from the stored result.
	before := srv.Calls()
	_, _, body = p9HTTP(t, http.MethodPost, promptURL+"/worker-prompt", "")
	if body["reused"] != true || srv.Calls() != before {
		t.Fatal("repeated preparation called the provider")
	}
}

// WBS 9.41 — credentials never enter prompts, previews, provenance, logs or
// persisted errors, even when the provider echoes the key back.
func TestP941CredentialsNeverPersisted(t *testing.T) {
	srv := providertest.New(providertest.Reply{Status: http.StatusUnauthorized, Body: `{"error":{"message":"invalid api key ` + p9SentinelKey + `"}}`})
	defer srv.Close()
	h := p9APIHarness(t, srv)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', context_window = "+itoa(n*4)+" WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"ALWAYS","target_tokens":`+itoa(n/2)+`}`)
	promptURL := base + "/prompts/" + rec.ID.String()
	_, rawPrev, _ := p9HTTP(t, http.MethodPost, promptURL+"/compression-preview", "")
	_, rawPrep, body := p9HTTP(t, http.MethodPost, promptURL+"/worker-prompt", "")
	_, rawAtt, _ := p9HTTP(t, http.MethodGet, promptURL+"/compression-attempts", "")
	_, rawProv, _ := p9HTTP(t, http.MethodGet, promptURL+"/provenance", "")
	for name, raw := range map[string]string{"preview": rawPrev, "prepare": rawPrep, "attempts": rawAtt, "provenance": rawProv} {
		if strings.Contains(raw, p9SentinelKey) {
			t.Fatalf("T9.41 secret in %s response", name)
		}
	}
	if !strings.Contains(rawAtt, "REDACTED") || body["run"].(map[string]any)["outcome"] != "FALLBACK_CANONICAL" {
		t.Fatalf("auth failure not recorded with redaction: %s", rawAtt)
	}
	var leaks int
	if err := h.DBScan(ctx, `SELECT
		(SELECT count(*) FROM canonical_prompts WHERE content LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM canonical_prompt_components WHERE content LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM compression_attempts WHERE error_message LIKE '%`+p9SentinelKey+`%' OR coalesce(validation::text, '') LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM compressed_prompts WHERE content LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM worker_prompts WHERE content LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM compression_runs WHERE reason LIKE '%`+p9SentinelKey+`%')`, &leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatalf("T9.41.1-T9.41.4 credential persisted in %d rows", leaks)
	}
}
