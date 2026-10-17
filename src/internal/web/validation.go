package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// decodeStrictJSON is the single centralized request-body validator (WBS
// 3.23). It reads the body, enforces the strict API policy — the body must
// be a JSON object, every key must be in the resource's closed allow-list,
// every required key must be present, and every value must decode into the
// target struct — and returns one of the centralized structured validation
// errors:
//
//   - malformed / non-object / unreadable body → code "malformed_json"
//   - unknown or missing fields, malformed field values → the resource
//     validation code with a field-level message map
//
// required may be nil for partially-updatable (PATCH) payloads.
func decodeStrictJSON(r *http.Request, code string, allowed map[string]bool, required []string, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return NewValidationError("request body could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return NewValidationError("request body must be a JSON object")
	}
	// null / arrays / scalars are not objects (WBS 17.157: a malformed
	// body must never become an empty update).
	if bytes.TrimSpace(raw)[0] != '{' {
		return NewValidationError("request body must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return NewValidationError("request body must be a JSON object")
	}
	var problems map[string]string
	for k := range fields {
		if !allowed[k] {
			if problems == nil {
				problems = make(map[string]string)
			}
			problems[k] = "unknown field"
		}
	}
	for _, k := range required {
		if _, ok := fields[k]; !ok {
			if problems == nil {
				problems = make(map[string]string)
			}
			problems[k] = "missing required field"
		}
	}
	if problems != nil {
		return NewValidationFields(code, "request body contains invalid fields", problems)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return NewValidationCode(code, "request body contains a malformed field value")
	}
	return nil
}

// decodeStrictOptionalJSON is decodeStrictJSON for an OPTIONAL body (WBS
// 8.35 preview): an empty/absent body is accepted (out keeps its zero
// value); a present body must be a JSON object from the resource's closed
// allow-list (no required fields — all are optional).
func decodeStrictOptionalJSON(r *http.Request, code string, allowed map[string]bool, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return NewValidationError("request body could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return NewValidationError("request body must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return NewValidationError("request body must be a JSON object")
	}
	var problems map[string]string
	for k := range fields {
		if !allowed[k] {
			if problems == nil {
				problems = make(map[string]string)
			}
			problems[k] = "unknown field"
		}
	}
	if problems != nil {
		return NewValidationFields(code, "request body contains invalid fields", problems)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return NewValidationCode(code, "request body contains a malformed field value")
	}
	return nil
}
