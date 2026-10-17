// Package audit is the pure model of human / API-originated control-plane
// actions (WBS 17.103–17.106). Autonomous engine history stays in the
// execution timeline (execution_events).
package audit

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
)

// ErrInvalid is returned for a malformed event or filter.
var ErrInvalid = errors.New("audit: invalid")

// Result is the outcome of the audited request.
type Result string

const (
	ResultSucceeded Result = "SUCCEEDED"
	ResultRejected  Result = "REJECTED"
	ResultFailed    Result = "FAILED"
)

// ParseResult accepts one result.
func ParseResult(s string) (Result, error) {
	switch r := Result(s); r {
	case ResultSucceeded, ResultRejected, ResultFailed:
		return r, nil
	}
	return "", fmt.Errorf("%w: result %q", ErrInvalid, s)
}

// ResultForStatus maps an HTTP status class to a result: 2xx / 3xx
// SUCCEEDED, 4xx REJECTED, 5xx FAILED.
func ResultForStatus(status int) Result {
	switch {
	case status >= 500:
		return ResultFailed
	case status >= 400:
		return ResultRejected
	}
	return ResultSucceeded
}

// Actor is the principal snapshot recorded with an event.
type Actor struct {
	ID   string
	Name string
	Type access.PrincipalType
}

// Event is one audited action (WBS 17.103, 17.106).
type Event struct {
	ID           uuid.UUID
	Seq          int64
	At           time.Time
	ProjectID    *uuid.UUID
	ProjectName  string
	ExecutionID  *uuid.UUID
	PhaseID      *uuid.UUID
	WorkUnitID   *uuid.UUID
	Actor        Actor
	Action       access.Action
	ResourceKind string
	ResourceID   string
	Result       Result
	// HTTPStatus is the response status of the audited request.
	HTTPStatus int
	Reason     string
	RequestID  string
	Method     string
	Path       string
	// Metadata is redacted structured detail.
	Metadata map[string]any
}

// MaxReason bounds the stored reason.
const MaxReason = 2000

// Validate checks an event before it is persisted.
func (e Event) Validate() error {
	var probs []string
	if e.Actor.ID == "" || e.Actor.Name == "" {
		probs = append(probs, "actor required")
	}
	if _, err := access.ParsePrincipalType(string(e.Actor.Type)); err != nil {
		probs = append(probs, "actor type invalid")
	}
	if !e.Action.IsMutation() {
		probs = append(probs, "action must be a mutation")
	}
	if _, err := ParseResult(string(e.Result)); err != nil {
		probs = append(probs, "result invalid")
	}
	if e.RequestID == "" {
		probs = append(probs, "request id required")
	}
	if e.ResourceKind == "" {
		probs = append(probs, "resource kind required")
	}
	if len(e.Reason) > MaxReason {
		probs = append(probs, "reason too long")
	}
	if e.At.IsZero() {
		probs = append(probs, "timestamp required")
	}
	if len(probs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(probs, "; "))
	}
	return nil
}

// Filter selects audit events (WBS 17.104). Zero fields do not filter.
type Filter struct {
	ProjectID   *uuid.UUID
	ExecutionID *uuid.UUID
	PhaseID     *uuid.UUID
	WorkUnitID  *uuid.UUID
	Action      access.Action
	Result      Result
	From        *time.Time
	To          *time.Time
	// Limit is the page size (default 50, max 200).
	Limit int
	// BeforeSeq is the keyset cursor: events with Seq < BeforeSeq
	// (0 = newest first page).
	BeforeSeq int64
}

// MaxLimit bounds a page.
const MaxLimit = 200

// Validate normalizes and checks the filter.
func (f *Filter) Validate() error {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 0 || f.Limit > MaxLimit {
		return fmt.Errorf("%w: limit must be 1..%d", ErrInvalid, MaxLimit)
	}
	if f.Action != "" && !f.Action.IsMutation() {
		return fmt.Errorf("%w: unknown action %q", ErrInvalid, f.Action)
	}
	if f.Result != "" {
		if _, err := ParseResult(string(f.Result)); err != nil {
			return err
		}
	}
	if f.From != nil && f.To != nil && f.To.Before(*f.From) {
		return fmt.Errorf("%w: to before from", ErrInvalid)
	}
	if f.BeforeSeq < 0 {
		return fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return nil
}

// EncodeCursor returns the opaque cursor for the page after seq.
func EncodeCursor(seq int64) string {
	if seq <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte("a:" + strconv.FormatInt(seq, 10)))
}

// DecodeCursor parses an opaque cursor ("" = first page).
func DecodeCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(b), "a:") {
		return 0, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	n, err := strconv.ParseInt(string(b[2:]), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return n, nil
}
