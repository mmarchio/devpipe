package hermes

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
)

// diagnosticLimit bounds a preserved raw diagnostic (WBS 12.92.3).
const diagnosticLimit = 2048

// Normalize maps a gateway reply to the generic response (WBS 12.12):
// content kept exact, session and usage captured, provider metadata kept.
// Missing or uncorrelated data is a PROTOCOL_ERROR — no content is ever
// invented (WBS 12.92). raw is the body for the diagnostic.
func Normalize(want uuid.UUID, sessionID string, gr GatewayResponse, raw []byte, secrets ...string) (worker.WorkerResponse, error) {
	switch {
	case gr.RequestID == nil || gr.Content == nil || gr.SessionID == nil:
		return worker.WorkerResponse{}, protocolError("response missing request_id, session_id or content", raw, secrets...)
	case *gr.RequestID != want.String():
		// WBS 12.32.3: a response for another request is quarantined,
		// never attached to this dispatch.
		return worker.WorkerResponse{}, protocolError("response correlation id does not match the request", raw, secrets...)
	case *gr.SessionID != sessionID:
		return worker.WorkerResponse{}, protocolError("response from an unexpected session", raw, secrets...)
	}
	out := worker.WorkerResponse{RequestID: want, Content: *gr.Content, Usage: usageFrom(gr.Usage), ProviderMetadata: map[string]string{}}
	for k, v := range gr.Metadata {
		out.ProviderMetadata[k] = worker.Redact(v, secrets...)
	}
	now := time.Now().UTC()
	out.StartedAt, out.CompletedAt = now, now
	if gr.StartedAt != nil {
		out.StartedAt = gr.StartedAt.UTC()
	}
	if gr.CompletedAt != nil {
		out.CompletedAt = gr.CompletedAt.UTC()
	}
	return out, nil
}

func protocolError(msg string, raw []byte, secrets ...string) *worker.WorkerError {
	e := worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, msg, nil)
	e.Diagnostic = worker.Truncate(worker.Redact(string(raw), secrets...), diagnosticLimit)
	return e
}

// gatewayCodes maps gateway error codes (WBS 12.38).
var gatewayCodes = map[string]worker.WorkerErrorCode{
	"auth_failed":            worker.CodeAuthFailed,
	"unauthorized":           worker.CodeAuthFailed,
	"forbidden":              worker.CodeAuthFailed,
	"session_not_found":      worker.CodeSessionLost,
	"session_expired":        worker.CodeSessionLost,
	"rate_limited":           worker.CodeRateLimited,
	"context_limit_exceeded": worker.CodeContextLimit,
	"context_length":         worker.CodeContextLimit,
	"invalid_request":        worker.CodeInvalidRequest,
	"invalid_directory":      worker.CodeInvalidRequest,
	"timeout":                worker.CodeTimeout,
	"unavailable":            worker.CodeUnavailable,
	"overloaded":             worker.CodeUnavailable,
	"session_busy":           worker.CodeBusy,
	"cancelled":              worker.CodeCancelled,
	// OpenAI-compatible codes and types.
	"context_length_exceeded": worker.CodeContextLimit,
	"invalid_api_key":         worker.CodeAuthFailed,
	"authentication_error":    worker.CodeAuthFailed,
	"permission_error":        worker.CodeAuthFailed,
	"rate_limit_exceeded":     worker.CodeRateLimited,
	"rate_limit_error":        worker.CodeRateLimited,
}

// errorEnvelope decodes both envelope shapes: the code may be a string,
// a number or null (OpenAI), and "type" classifies when it is absent.
type errorEnvelope struct {
	Error struct {
		Code              json.RawMessage `json:"code"`
		Type              string          `json:"type"`
		Message           string          `json:"message"`
		RetryAfterSeconds *int            `json:"retry_after_seconds"`
	} `json:"error"`
}

func (e errorEnvelope) gateway() GatewayError {
	var ge GatewayError
	var code string
	if err := json.Unmarshal(e.Error.Code, &code); err != nil && len(e.Error.Code) > 0 && string(e.Error.Code) != "null" {
		code = string(e.Error.Code)
	}
	ge.Error.Code, ge.Error.Message, ge.Error.RetryAfterSeconds = code, e.Error.Message, e.Error.RetryAfterSeconds
	return ge
}

// NormalizeError maps a non-2xx gateway reply to a WorkerError (WBS
// 12.38/12.59-12.61): gateway error code first, then HTTP status; the
// provider code, Retry-After and a sanitized diagnostic are preserved.
// The gateway answered, so delivery is SENT_RESPONSE_RECEIVED.
func NormalizeError(status int, header http.Header, body []byte, secrets ...string) *worker.WorkerError {
	var env errorEnvelope
	_ = json.Unmarshal(body, &env)
	ge := env.gateway()
	code, ok := gatewayCodes[strings.ToLower(ge.Error.Code)]
	if !ok {
		code, ok = gatewayCodes[strings.ToLower(env.Error.Type)]
		if !ok || code == worker.CodeInvalidRequest {
			code = statusCode(status)
		}
	}
	msg := ge.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	e := worker.NewError(code, worker.DeliveryReceived, worker.Redact(msg, secrets...), nil)
	if ge.Error.Code != "" {
		pc := worker.Redact(ge.Error.Code, secrets...)
		e.ProviderCode = &pc
	} else {
		pc := "http_" + strconv.Itoa(status)
		e.ProviderCode = &pc
	}
	e.Diagnostic = worker.Truncate(worker.Redact(string(body), secrets...), diagnosticLimit)
	if ra := retryAfter(header, ge); ra != nil {
		e.RetryAfter = ra
	}
	if ge.Error.Code == "" && (status == http.StatusBadGateway || status == http.StatusGatewayTimeout) {
		// An intermediary (proxy / load balancer) answered, not the
		// gateway: the upstream may have received and executed the
		// request, so delivery is unknown (WBS 12.34/12.89).
		e.Delivery, e.Retryable = worker.DeliveryUnknown, false
		if status == http.StatusBadGateway {
			e.Code = worker.CodeDeliveryUnknown
		}
	}
	return e
}

func statusCode(status int) worker.WorkerErrorCode {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return worker.CodeAuthFailed
	case status == http.StatusTooManyRequests:
		return worker.CodeRateLimited
	case status == http.StatusRequestEntityTooLarge:
		return worker.CodeContextLimit
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return worker.CodeTimeout
	case status == http.StatusServiceUnavailable || status == http.StatusBadGateway:
		return worker.CodeUnavailable
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return worker.CodeInvalidRequest
	}
	return worker.CodeInternal
}

func retryAfter(h http.Header, ge GatewayError) *time.Duration {
	if ge.Error.RetryAfterSeconds != nil && *ge.Error.RetryAfterSeconds >= 0 {
		d := time.Duration(*ge.Error.RetryAfterSeconds) * time.Second
		return &d
	}
	if h == nil {
		return nil
	}
	v := h.Get("Retry-After")
	if v == "" {
		return nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		return &d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}
