package prompt

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ResponseProtocolKind is the protocol kind discriminator (WBS 8.23): the
// named kinds a state may be required to answer with.
type ResponseProtocolKind string

const (
	KindPlanResponse           ResponseProtocolKind = "PLAN_RESPONSE"
	KindImplementationResponse ResponseProtocolKind = "IMPLEMENTATION_RESPONSE"
	KindValidationResponse     ResponseProtocolKind = "VALIDATION_RESPONSE"
	KindReviewResponse         ResponseProtocolKind = "REVIEW_RESPONSE"
)

// Known reports whether k is one of the four WBS protocol kinds.
func (k ResponseProtocolKind) Known() bool {
	switch k {
	case KindPlanResponse,
		KindImplementationResponse,
		KindValidationResponse,
		KindReviewResponse:
		return true
	}
	return false
}

// ResponseProtocolVersion is one immutable, append-only revision of a
// response protocol (WBS 8.23: the schema a worker must answer with).
// Historical rows are never overwritten (T8.23.4).
type ResponseProtocolVersion struct {
	ProtocolID  uuid.UUID
	Version     int
	Content     string
	ContentHash string
	CreatedAt   time.Time
}

// ResponseProtocolInput carries the caller-supplied fields for
// NewResponseProtocolSeries. Timestamps and Version are assigned by the
// domain.
type ResponseProtocolInput struct {
	Name    string
	Kind    ResponseProtocolKind
	Content string
	Ts      time.Time
}

// ResponseProtocolSeries is the in-domain (pure, no DB) model of a
// response protocol's version history (WBS 8.23, pure half): it stores the
// protocol schema exactly — the full protocol content, byte-for-byte
// (T8.23.1) — and versions it by appending instead of overwriting
// (T8.23.2/T8.23.4). The DB persistence half (P8-13) mirrors these
// semantics.
type ResponseProtocolSeries struct {
	ID   uuid.UUID
	Name string
	Kind ResponseProtocolKind
	Rows []ResponseProtocolVersion
}

// NewResponseProtocolSeries validates the kind and initial content and
// creates the v1 row (T8.23.1/T8.23.2).
func NewResponseProtocolSeries(in ResponseProtocolInput) (*ResponseProtocolSeries, error) {
	if !in.Kind.Known() {
		return nil, fmt.Errorf("prompt: unknown response protocol kind %q", in.Kind)
	}
	if in.Content == "" {
		return nil, fmt.Errorf("prompt: response protocol content must not be empty")
	}
	s := &ResponseProtocolSeries{
		ID:   uuid.New(),
		Name: in.Name,
		Kind: in.Kind,
	}
	s.Rows = []ResponseProtocolVersion{newProtocolVersion(s.ID, 1, in.Content, in.Ts)}
	return s, nil
}

// Append records a new schema revision (T8.23.2) and advances the current
// version. Empty content is rejected.
func (s *ResponseProtocolSeries) Append(content string, ts time.Time) (*ResponseProtocolVersion, error) {
	if content == "" {
		return nil, fmt.Errorf("prompt: response protocol content must not be empty")
	}
	next := s.Rows[len(s.Rows)-1].Version + 1
	s.Rows = append(s.Rows, newProtocolVersion(s.ID, next, content, ts))
	return &s.Rows[len(s.Rows)-1], nil
}

// GetVersion retrieves any historical revision (T8.23.4) by exact version.
func (s *ResponseProtocolSeries) GetVersion(version int) (*ResponseProtocolVersion, error) {
	for i := range s.Rows {
		if s.Rows[i].Version == version {
			return &s.Rows[i], nil
		}
	}
	return nil, fmt.Errorf("prompt: protocol version %d not found", version)
}

// Current returns the highest version row (T8.23.2).
func (s *ResponseProtocolSeries) Current() (*ResponseProtocolVersion, error) {
	if len(s.Rows) == 0 {
		return nil, fmt.Errorf("prompt: series has no versions")
	}
	return &s.Rows[len(s.Rows)-1], nil
}

func newProtocolVersion(protocolID uuid.UUID, version int, content string, ts time.Time) ResponseProtocolVersion {
	return ResponseProtocolVersion{
		ProtocolID:  protocolID,
		Version:     version,
		Content:     content,
		ContentHash: ContentHash(content),
		CreatedAt:   ts,
	}
}

// StateProtocolReference is a state's pin of a specific protocol version
// (WBS 8.23: "different states may use different schemas" — a reference is
// by protocol identity + version, never "latest").
type StateProtocolReference struct {
	StateID         uuid.UUID
	ProtocolID      uuid.UUID
	ProtocolVersion int
}

// NewStateProtocolReference validates a state -> (protocol, version)
// reference (T8.23.3).
func NewStateProtocolReference(stateID, protocolID uuid.UUID, version int) (*StateProtocolReference, error) {
	if protocolID == uuid.Nil {
		return nil, fmt.Errorf("prompt: state protocol reference requires a protocol id")
	}
	if version < 1 {
		return nil, fmt.Errorf("prompt: state protocol reference requires a version >= 1")
	}
	return &StateProtocolReference{StateID: stateID, ProtocolID: protocolID, ProtocolVersion: version}, nil
}

// Validate checks the reference against a protocol: it must name THIS
// protocol and a version the protocol carries (T8.23.3).
func (r *StateProtocolReference) Validate(p *ResponseProtocolSeries) error {
	if p == nil {
		return fmt.Errorf("prompt: state protocol reference has no protocol")
	}
	if r.ProtocolID != p.ID {
		return fmt.Errorf("prompt: state references protocol %s, got %s", r.ProtocolID, p.ID)
	}
	if _, err := p.GetVersion(r.ProtocolVersion); err != nil {
		return fmt.Errorf("prompt: state protocol reference unsatisfied: %w", err)
	}
	return nil
}

// Resolved returns the pinned historical version (T8.23.4): it stays
// readable after the protocol advances, and never silently tracks the
// current version.
func (r *StateProtocolReference) Resolved(p *ResponseProtocolSeries) (*ResponseProtocolVersion, error) {
	if err := r.Validate(p); err != nil {
		return nil, err
	}
	return p.GetVersion(r.ProtocolVersion)
}
