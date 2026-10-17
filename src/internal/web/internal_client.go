package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"overseer/internal/web/middleware"
)

// InternalClient performs in-process calls of the registered /api/v1
// routes for the server-rendered pages (WBS 17.1 / 17.200: pages use the
// normal backend API). Calls reuse the caller's context (principal,
// request id), so the API route's authorization and audit apply exactly
// once; the outer page request has already passed CSRF and size limits.
type InternalClient struct {
	r *Router
}

// Internal returns the router's in-process API client.
func (r *Router) Internal() *InternalClient { return &InternalClient{r: r} }

// memWriter is a minimal in-memory ResponseWriter.
type memWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (m *memWriter) Header() http.Header { return m.h }
func (m *memWriter) WriteHeader(code int) {
	if m.status == 0 {
		m.status = code
	}
}
func (m *memWriter) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	return m.buf.Write(b)
}

// APIResponse is an in-process API response.
type APIResponse struct {
	Status int
	Body   []byte
	Header http.Header
}

// OK reports a 2xx status.
func (a APIResponse) OK() bool { return a.Status >= 200 && a.Status < 300 }

// Decode unmarshals the body into v.
func (a APIResponse) Decode(v any) error { return json.Unmarshal(a.Body, v) }

// Error returns the error envelope (code, message, field errors).
func (a APIResponse) Error() (code, message string, fields map[string]string) {
	var env struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.Body, &env)
	return env.Error.Code, env.Error.Message, env.Error.Fields
}

// Do calls method path (path may carry a query) with an optional JSON body
// and human reason (X-Overseer-Reason).
func (c *InternalClient) Do(parent *http.Request, method, path string, body any, reason string) APIResponse {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return APIResponse{Status: http.StatusInternalServerError}
		}
		rd = bytes.NewReader(b)
	}
	ctx := parent.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		return APIResponse{Status: http.StatusBadRequest}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(reason) != "" {
		req.Header.Set(ReasonHeader, reason)
	}
	if v := parent.Header.Get(ConfigVersionHeader); v != "" && method != http.MethodGet {
		req.Header.Set(ConfigVersionHeader, v)
	}
	req.Host = parent.Host
	req.RemoteAddr = parent.RemoteAddr
	// The principal travels in the context; ensure it is present.
	if middleware.PrincipalFromContext(ctx).IsZero() {
		return APIResponse{Status: http.StatusUnauthorized}
	}
	w := &memWriter{h: http.Header{}}
	c.r.apiMux.ServeHTTP(w, req)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return APIResponse{Status: w.status, Body: w.buf.Bytes(), Header: w.h}
}

// Get is Do(GET) decoding into v; it returns the response.
func (c *InternalClient) Get(parent *http.Request, path string, v any) APIResponse {
	resp := c.Do(parent, http.MethodGet, path, nil, "")
	if resp.OK() && v != nil {
		if err := resp.Decode(v); err != nil {
			resp.Status = http.StatusInternalServerError
		}
	}
	return resp
}
