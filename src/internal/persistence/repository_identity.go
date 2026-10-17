package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

// Repository identity sources.
const (
	IdentityFromBootstrap  = "BOOTSTRAP"
	IdentityFromFirstCheck = "FIRST_CHECK"
)

// RepositoryIdentityRecord is an execution's recorded repository identity
// (WBS 18.49; immutable).
type RepositoryIdentityRecord struct {
	ExecutionID uuid.UUID
	ProjectID   uuid.UUID
	Identity    execution.RepositoryIdentity
	Source      string
	RecordedAt  time.Time
}

// Expected HEAD sources.
const (
	HeadFromBootstrap      = "BOOTSTRAP"
	HeadFromFirstCheck     = "FIRST_CHECK"
	HeadFromCheckpoint     = "CHECKPOINT"
	HeadFromOperatorAccept = "OPERATOR_ACCEPT"
)

// ExpectedHeadRecord is one entry of an execution's append-only expected
// repository HEAD history (WBS 18.50; the latest entry is the expectation).
type ExpectedHeadRecord struct {
	ID           uuid.UUID
	ExecutionID  uuid.UUID
	ProjectID    uuid.UUID
	Seq          int
	Head         string
	Branch       string
	Source       string
	CheckpointID *uuid.UUID
	Actor        string
	Reason       string
	RecordedAt   time.Time
}

// RepositoryIdentityStore persists repository identities and expected
// HEADs.
type RepositoryIdentityStore interface {
	// LatestExpectedHead returns ErrNotFound when none is recorded.
	LatestExpectedHead(ctx context.Context, executionID uuid.UUID) (*ExpectedHeadRecord, error)
	// AppendExpectedHead appends rec (Seq and ID are assigned).
	AppendExpectedHead(ctx context.Context, rec *ExpectedHeadRecord) (*ExpectedHeadRecord, error)

	// Get returns ErrNotFound when none is recorded.
	Get(ctx context.Context, executionID uuid.UUID) (*RepositoryIdentityRecord, error)
	// Record stores rec unless one exists; it returns the stored record
	// and whether this call created it.
	Record(ctx context.Context, rec *RepositoryIdentityRecord) (*RepositoryIdentityRecord, bool, error)
}
