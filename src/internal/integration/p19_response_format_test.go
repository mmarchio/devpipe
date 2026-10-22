package integration

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"overseer/internal/gateway/hermes/hermestest"
)

// TestP19WorkerResponseFormat — the worker configuration page selects the
// worker's response formatting (Markdown by default, XML or key:value);
// the API validates it, keeps it when omitted, and the output-format skill
// command sent to Hermes carries the selected format.
func TestP19WorkerResponseFormat(t *testing.T) {
	r := p17NewRigWith(t, p16Options{statefile: true, noBootstrap: true})
	b := r.b
	pid := r.pid.String()
	page := "/projects/" + pid + "/config/worker"
	api := "/api/v1/projects/" + pid + "/settings/worker"
	format := func() string {
		t.Helper()
		var w struct {
			ResponseFormat string `json:"response_format"`
		}
		if code := b.getJSON(api, &w); code != 200 {
			t.Fatalf("worker settings: %d", code)
		}
		return w.ResponseFormat
	}

	// Markdown is the default, and the page offers all three formats.
	if got := format(); got != "MARKDOWN" {
		t.Fatalf("default response format = %q, want MARKDOWN", got)
	}
	body := b.mustPage(page)
	for _, want := range []string{`name="response_format"`, `<option value="MARKDOWN" selected>Markdown</option>`,
		`<option value="XML">XML</option>`, `<option value="KEY_VALUE">key:value</option>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("worker page lacks %q", want)
		}
	}

	// Saving the form stores the selection, and the page shows it.
	if code, body := b.save(page, url.Values{"response_format": {"KEY_VALUE"}}); code != 200 {
		t.Fatalf("save: %d %s", code, textOf(body))
	}
	if got := format(); got != "KEY_VALUE" {
		t.Fatalf("saved response format = %q, want KEY_VALUE", got)
	}
	if !strings.Contains(b.mustPage(page), `<option value="KEY_VALUE" selected>key:value</option>`) {
		t.Fatal("page does not show the saved format")
	}

	// The API rejects an unknown format and keeps the format when omitted.
	if code, raw := b.api(http.MethodPut, api, map[string]any{"session_policy": "SINGLE_ACTIVE_PER_PROJECT", "concurrent_send": "REJECT",
		"response_format": "YAML"}); code != http.StatusBadRequest {
		t.Fatalf("unknown format: %d %s", code, raw)
	}
	if code, raw := b.api(http.MethodPut, api, map[string]any{"session_policy": "SINGLE_ACTIVE_PER_PROJECT", "concurrent_send": "REJECT"}); code != 200 {
		t.Fatalf("update without format: %d %s", code, raw)
	} else {
		var w map[string]any
		if err := json.Unmarshal(raw, &w); err != nil || w["response_format"] != "KEY_VALUE" {
			t.Fatalf("omitted format not kept: %s %v", raw, err)
		}
	}

	// The output-format skill command carries the selected format.
	var mu sync.Mutex
	var skill string
	r.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		first, _, _ := strings.Cut(m.Content, "\n")
		if strings.HasSuffix(first, "-output-format skill") {
			mu.Lock()
			skill = m.Content
			mu.Unlock()
		}
		return nil
	}
	r.boot(t)
	r.run(t, 3)
	mu.Lock()
	defer mu.Unlock()
	if skill == "" {
		t.Fatal("no output-format skill command sent")
	}
	for _, want := range []string{"Format every reply as `key: value` lines", "formatted as key:value"} {
		if !strings.Contains(skill, want) {
			t.Fatalf("skill command lacks %q:\n%s", want, skill)
		}
	}
	if strings.Contains(skill, "Format every reply as Markdown") {
		t.Fatalf("skill command still asks for Markdown:\n%s", skill)
	}
}
