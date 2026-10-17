package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ResponseProtocolRecord is the domain-facing view of a stored response
// protocol (WBS 8.23): the identity row of a worker response schema. A nil
// ProjectID is the GLOBAL scope; a set ProjectID is a PROJECT scope. Kind
// uniqueness is scoped: the same kind may exist in different projects and
// globally, but never twice within one scope.
type ResponseProtocolRecord struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Name string

	// Kind is the protocol kind discriminator (PLAN_RESPONSE /
	// IMPLEMENTATION_RESPONSE / VALIDATION_RESPONSE / REVIEW_RESPONSE).
	Kind string

	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ResponseProtocolVersionRecord is the domain-facing view of a stored
// response-protocol content version (WBS 8.23): immutable historical content
// plus its deterministic content hash (prompt.ContentHash). Historical rows
// are never rewritten (T8.23.4).
type ResponseProtocolVersionRecord struct {
	ID         uuid.UUID
	ProtocolID uuid.UUID

	Version int

	Content string

	ContentHash string

	CreatedAt time.Time
}

// StateProtocolReferenceRecord is the domain-facing view of a state's pin of
// a specific protocol version (T8.23.3): a reference is by protocol identity
// + version, never "latest".
type StateProtocolReferenceRecord struct {
	StateID         uuid.UUID
	ProtocolID      uuid.UUID
	ProtocolVersion int
	CreatedAt       time.Time
}

// ResponseProtocolRepository is the domain-facing persistence contract for
// response protocols (WBS 8.23 persistence half, P8-13). Create inserts the
// protocol's parent row AND its initial v1 version row atomically (the v1
// content is supplied alongside the parent record); a scoped kind duplicate
// yields ErrConflict. AppendVersion is the only later writer of a version row
// (append-only; a version number is never reused).
type ResponseProtocolRepository interface {
	// Create inserts a new protocol with its v1 version row; a scoped kind
	// duplicate yields ErrConflict, a missing project reference yields
	// ErrInvalidReference.
	Create(ctx context.Context, rec *ResponseProtocolRecord, initialContent string) error
	// Get returns the protocol by id; an unknown id yields ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (*ResponseProtocolRecord, error)
	// AppendVersion stores content as the next version, computed as
	// MAX(version)+1 inside a transaction serialized by a per-protocol
	// advisory lock and wrapped in a bounded retry on the (protocol_id,
	// version) unique constraint (T8.23.2). An unknown protocol yields
	// ErrNotFound before any write.
	AppendVersion(ctx context.Context, protocolID uuid.UUID, content string) (*ResponseProtocolVersionRecord, error)
	// GetCurrentVersion returns the protocol's current (highest) version,
	// resolved from the stored history. An unknown protocol yields
	// ErrNotFound; a protocol with no versions returns (nil, nil).
	GetCurrentVersion(ctx context.Context, protocolID uuid.UUID) (*ResponseProtocolVersionRecord, error)
	// GetVersion returns the stored version; a missing version yields
	// ErrNotFound (T8.23.4: historical versions stay retrievable).
	GetVersion(ctx context.Context, protocolID uuid.UUID, version int) (*ResponseProtocolVersionRecord, error)
	// ListVersions returns all stored versions in ascending order.
	ListVersions(ctx context.Context, protocolID uuid.UUID) ([]*ResponseProtocolVersionRecord, error)
	// CreateStateReference pins a state to (protocol, version) (T8.23.3).
	// Both the state and the protocol row must exist (and, for the protocol,
	// the pinned version must be a stored version); unknown state/protocol
	// or absent version yield ErrNotFound, a duplicate (state, protocol)
	// reference yields ErrConflict.
	CreateStateReference(ctx context.Context, stateID, protocolID uuid.UUID, version int) (*StateProtocolReferenceRecord, error)
	// GetStateReference returns the state's reference to the protocol; an
	// absent reference yields ErrNotFound.
	GetStateReference(ctx context.Context, stateID, protocolID uuid.UUID) (*StateProtocolReferenceRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) ResponseProtocolRepository
}

// ResponseProtocolLookup resolves the protocol of a kind visible to a
// project (project scope first, then global).
type ResponseProtocolLookup interface {
	FindByKind(ctx context.Context, projectID *uuid.UUID, kind string) (*ResponseProtocolRecord, error)
}
