package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"overseer/internal/config"
	"overseer/internal/service"
)

type view struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	WBS  string `json:"wbs"`
}

// T6.8 — only the requested top-level fields are returned; an unknown
// field is refused with the available ones.
func TestRenderFields(t *testing.T) {
	b, err := render(view{Key: "A", Name: "Task A", WBS: "doc"}, []string{"key"}, 1024)
	if err != nil || string(b) != `{"key":"A"}` {
		t.Fatalf("%s %v", b, err)
	}
	_, err = render(view{}, []string{"nope"}, 1024)
	var rej *service.MCPRejection
	if !errors.As(err, &rej) || rej.Code != "UNKNOWN_FIELD" || !strings.Contains(rej.Message, "available: key, name, wbs") {
		t.Fatalf("%v", err)
	}
}

// T6.9 — an oversized result is bounded: the longest string is truncated
// and marked; one that cannot fit is an explicit error.
func TestRenderSizeLimit(t *testing.T) {
	b, err := render(view{Key: "A", WBS: strings.Repeat("x", 5000)}, nil, 400)
	if err != nil || len(b) > 400 {
		t.Fatalf("%d bytes %v", len(b), err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["truncated"] != true || !strings.HasSuffix(m["wbs"].(string), truncatedMarker) || m["key"] != "A" {
		t.Fatalf("%s %v", b, err)
	}
	_, err = render(map[string]any{"list": make([]int, 500)}, nil, 100)
	var rej *service.MCPRejection
	if !errors.As(err, &rej) || rej.Code != "RESPONSE_TOO_LARGE" {
		t.Fatalf("%v", err)
	}
}

// T5.6 — domain errors become tool errors with a code and actionable
// text; internal failures are not detailed.
func TestToolErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: unknown token", service.ErrMCPUnauthorized), "UNAUTHORIZED: unknown token"},
		{&service.MCPRejection{Code: "CRITERIA_COUNT", Message: "give one per check"}, "CRITERIA_COUNT: give one per check"},
		{fmt.Errorf("%w: task X is not in the current plan", service.ErrNotFound), "NOT_FOUND: task X is not in the current plan"},
		{fmt.Errorf("%w: key reused", service.ErrConflict), "CONFLICT: key reused"},
		{context.DeadlineExceeded, "TIMEOUT: the request timed out; retry with the same idempotency_key"},
		{errors.New("pq: connection refused"), "INTERNAL: the overseer could not complete the request; retry with the same idempotency_key"},
	} {
		res := toolError(c.err)
		if !res.IsError || res.Content[0].(*sdk.TextContent).Text != c.want {
			t.Fatalf("%v → %+v, want %q", c.err, res.Content[0], c.want)
		}
	}
}

// T5.10 — the listener refuses requests without the bearer token (and
// everything when no token is configured); health needs none.
func TestBearerAuthentication(t *testing.T) {
	secret := "s3cret"
	srv := httptest.NewServer(New(config.MCPConfig{Enabled: true}, nil, nil, func() string { return secret }).Handler())
	defer srv.Close()
	post := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, auth := range []string{"", "Bearer wrong", "Basic " + secret, secret} {
		if code := post(auth); code != http.StatusUnauthorized {
			t.Fatalf("auth %q: %d", auth, code)
		}
	}
	if code := post("Bearer " + secret); code == http.StatusUnauthorized {
		t.Fatal("the bearer token was refused")
	}
	secret = ""
	if code := post("Bearer "); code != http.StatusUnauthorized {
		t.Fatalf("no configured secret: %d", code)
	}
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("healthz %v %v", res, err)
	}
	res.Body.Close()
}
