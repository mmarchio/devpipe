package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/database"
)

// Phase 17 rig (P17-125): the p16 rig (real services, test PostgreSQL,
// hermestest fake worker, temp git repository) plus a browser-like HTTP
// client — cookie jar, CSRF token from the page's meta tag, form posts and
// redirects — and an SSE reader with Last-Event-ID. There is no browser /
// JS runtime on the box: this client exercises the same server contract a
// browser uses.

type p17Rig struct {
	*p16Rig
	b *browser
	// ctx, when set, drives engine runs of chaos operators (fault
	// registries).
	ctx context.Context
}

func p17Config(extra func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		c.Web.CookieInsecure = true // the test server is plain http
		c.Web.FeedInterval = 50 * time.Millisecond
		if extra != nil {
			extra(c)
		}
	}
}

func p17NewRig(t *testing.T) *p17Rig { return p17NewRigWith(t, p16Options{}) }

func p17NewRigWith(t *testing.T, o p16Options) *p17Rig {
	t.Helper()
	o.config = p17Config(o.config)
	r := &p17Rig{p16Rig: p16NewRigWith(t, o)}
	r.b = newBrowser(t, r.h.BaseURL)
	return r
}

// browser is a cookie-carrying HTTP client that behaves like a browser
// toward the console: page GETs with Accept text/html, form POSTs carrying
// the CSRF token, redirects followed.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
	// headers are added to every API request (e.g. X-Request-ID).
	headers map[string]string
}

func newBrowser(t *testing.T, base string) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 60 * time.Second}}
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]*)">`)

func (b *browser) remember(body string) {
	if m := csrfMeta.FindStringSubmatch(body); m != nil && m[1] != "" {
		b.csrf = m[1]
	}
}

// page GETs a console page.
func (b *browser) page(path string) (int, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.base+path, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	b.remember(body)
	return resp.StatusCode, body
}

// mustPage GETs a page and requires 200.
func (b *browser) mustPage(path string) string {
	b.t.Helper()
	code, body := b.page(path)
	if code != http.StatusOK {
		b.t.Fatalf("GET %s: %d\n%s", path, code, trimBody(body))
	}
	return body
}

func trimBody(s string) string {
	if len(s) > 3000 {
		return s[:3000] + "…"
	}
	return s
}

// form POSTs a form with the CSRF token (following the redirect); it
// returns the final status, body and URL path.
func (b *browser) form(path string, vals url.Values) (int, string, string) {
	b.t.Helper()
	if vals == nil {
		vals = url.Values{}
	}
	if b.csrf == "" {
		b.page("/")
	}
	vals.Set("csrf_token", b.csrf)
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	b.remember(body)
	return resp.StatusCode, body, resp.Request.URL.Path
}

// rawForm POSTs without adding a CSRF token.
func (b *browser) rawForm(path string, vals url.Values) int {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// api calls the JSON API with the session cookie + CSRF header.
func (b *browser) api(method, path string, body any) (int, []byte) {
	b.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.csrf == "" {
		b.page("/")
	}
	req.Header.Set("X-CSRF-Token", b.csrf)
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// getJSON GETs an API path into v (status returned).
func (b *browser) getJSON(path string, v any) int {
	b.t.Helper()
	code, raw := b.api(http.MethodGet, path, nil)
	if v != nil && code == http.StatusOK {
		if err := json.Unmarshal(raw, v); err != nil {
			b.t.Fatalf("decode %s: %v\n%s", path, err, raw)
		}
	}
	return code
}

// sseEvent is one received server-sent event.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// stream opens an SSE stream; events arrive on the channel until cancel.
func (b *browser) stream(path, lastEventID string) (<-chan sseEvent, func(), int) {
	b.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.base+path, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Jar: b.c.Jar}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		b.t.Fatalf("stream %s: %v", path, err)
	}
	ch := make(chan sseEvent, 1024)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		close(ch)
		return ch, cancel, resp.StatusCode
	}
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					ch <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch, cancel, resp.StatusCode
}

// collect reads events until n arrived or the timeout passed.
func collect(ch <-chan sseEvent, n int, timeout time.Duration) []sseEvent {
	var out []sseEvent
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			if e.Event == "execution" {
				out = append(out, e)
			}
		case <-deadline:
			return out
		}
	}
	return out
}

// queries counts the statements a function issues through ctx.
func queries(fn func(ctx context.Context)) int64 {
	var c atomic.Int64
	fn(database.WithQueryCounter(context.Background(), &c))
	return c.Load()
}

// textOf strips tags (assertions on visible text).
var tagRE = regexp.MustCompile(`<[^>]+>`)

func textOf(html string) string {
	return strings.Join(strings.Fields(tagRE.ReplaceAllString(html, " ")), " ")
}
