package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"overseer/internal/service"
)

// truncatedMarker ends a string field shortened to fit the response cap.
const truncatedMarker = "\n…[truncated]"

// render marshals a tool result: only the requested top-level fields
// (unknown names are an error), then capped at max bytes — the longest
// string field is shortened first (marked, with "truncated": true); a
// result still too large is an explicit error.
func render(v any, fields []string, max int) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return capped(raw, max)
	}
	if len(fields) > 0 {
		keep := map[string]any{}
		var unknown []string
		for _, f := range fields {
			val, ok := m[f]
			if !ok {
				unknown = append(unknown, f)
				continue
			}
			keep[f] = val
		}
		if len(unknown) > 0 {
			names := make([]string, 0, len(m))
			for k := range m {
				names = append(names, k)
			}
			sort.Strings(names)
			return nil, &service.MCPRejection{Code: "UNKNOWN_FIELD",
				Message: fmt.Sprintf("unknown fields %s; available: %s", strings.Join(unknown, ", "), strings.Join(names, ", "))}
		}
		m = keep
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	for len(out) > max {
		key, longest := "", 0
		for k, val := range m {
			if s, ok := val.(string); ok && len(s) > longest && !strings.HasSuffix(s, truncatedMarker) {
				key, longest = k, len(s)
			}
		}
		if key == "" {
			break
		}
		s := m[key].(string)
		keep := longest - (len(out) - max) - len(truncatedMarker) - 32
		if keep < 0 {
			keep = 0
		}
		m[key] = strings.ToValidUTF8(s[:keep], "") + truncatedMarker
		m["truncated"] = true
		if out, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	return capped(out, max)
}

func capped(b []byte, max int) ([]byte, error) {
	if len(b) > max {
		return nil, &service.MCPRejection{Code: "RESPONSE_TOO_LARGE",
			Message: fmt.Sprintf("the result is %d bytes (limit %d); request fewer fields", len(b), max)}
	}
	return b, nil
}

// code classifies a domain error for the client.
func code(err error) string {
	var rej *service.MCPRejection
	switch {
	case errors.As(err, &rej):
		return rej.Code
	case errors.Is(err, service.ErrMCPUnauthorized):
		return "UNAUTHORIZED"
	case errors.Is(err, service.ErrNotFound):
		return "NOT_FOUND"
	case errors.Is(err, service.ErrConflict):
		return "CONFLICT"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	}
	return "INTERNAL"
}

// toolError is the tool-error result of a domain error: "<CODE>: <what to
// do>"; internal failures are not detailed.
func toolError(err error) *sdk.CallToolResult {
	c := code(err)
	msg := err.Error()
	var rej *service.MCPRejection
	switch {
	case errors.As(err, &rej):
		msg = rej.Message
	case c == "INTERNAL":
		msg = "the overseer could not complete the request; retry with the same idempotency_key"
	case c == "TIMEOUT":
		msg = "the request timed out; retry with the same idempotency_key"
	}
	for _, p := range []string{"service: MCP call not authorized: ", "service: conflict: ", "service: project not found: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: c + ": " + msg}}}
}
