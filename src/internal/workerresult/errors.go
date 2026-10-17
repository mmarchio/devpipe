package workerresult

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// WorkerResultErrorCode classifies a protocol parse failure (WBS 13.30).
type WorkerResultErrorCode string

// Parse failure classes.
const (
	ResultMissing            WorkerResultErrorCode = "RESULT_MISSING"
	ResultMalformed          WorkerResultErrorCode = "RESULT_MALFORMED"
	ResultAmbiguous          WorkerResultErrorCode = "RESULT_AMBIGUOUS"
	ResultSchemaInvalid      WorkerResultErrorCode = "RESULT_SCHEMA_INVALID"
	ResultSemanticInvalid    WorkerResultErrorCode = "RESULT_SEMANTIC_INVALID"
	ResultUnsupportedVersion WorkerResultErrorCode = "RESULT_UNSUPPORTED_VERSION"
)

// ValidationError is one field-level problem (WBS 13.31).
type ValidationError struct {
	Field   string
	Code    string
	Message string
}

// ParseError is a protocol failure: the overseer could not obtain a valid
// result. It is never a worker-reported FAILED (WBS 13.32).
type ParseError struct {
	Code       WorkerResultErrorCode
	Errors     []ValidationError
	Provenance ResultParseProvenance
}

func (e *ParseError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, v := range e.Errors {
		parts = append(parts, v.Field+" "+v.Code)
	}
	if len(parts) == 0 {
		return "workerresult: " + string(e.Code)
	}
	return fmt.Sprintf("workerresult: %s: %s", e.Code, strings.Join(parts, ", "))
}

// Is matches a *ParseError target by code (empty code matches any).
func (e *ParseError) Is(target error) bool {
	var t *ParseError
	if errors.As(target, &t) {
		return t.Code == "" || t.Code == e.Code
	}
	return false
}

// AsParseError extracts a *ParseError.
func AsParseError(err error) (*ParseError, bool) {
	var pe *ParseError
	ok := errors.As(err, &pe)
	return pe, ok
}

// newParseError builds a ParseError with deterministically ordered
// details (by field, then code, then message).
func newParseError(code WorkerResultErrorCode, errs []ValidationError, prov ResultParseProvenance) *ParseError {
	out := append([]ValidationError(nil), errs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Message < out[j].Message
	})
	return &ParseError{Code: code, Errors: out, Provenance: prov}
}

// ErrBindingMismatch reports a response that does not belong to the
// dispatch being parsed (WBS 13.49) — an integrity error, not a protocol
// failure.
var ErrBindingMismatch = errors.New("workerresult: response does not belong to the dispatch")
