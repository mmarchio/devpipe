package cache

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status is a cache entry's lifecycle status (WBS 10.1/10.31).
type Status string

const (
	StatusValid       Status = "VALID"
	StatusBuilding    Status = "BUILDING"
	StatusFailed      Status = "FAILED"
	StatusInvalidated Status = "INVALIDATED"
)

// Known reports whether s is a valid status.
func (s Status) Known() bool {
	switch s {
	case StatusValid, StatusBuilding, StatusFailed, StatusInvalidated:
		return true
	}
	return false
}

// EntrySource records how an entry was created (WBS 10.43). It is metadata
// only — never part of the semantic key.
type EntrySource string

const (
	SourceOnDemand    EntrySource = "ON_DEMAND"
	SourcePrecomputed EntrySource = "PRECOMPUTED"
)

// Known reports whether s is a valid source.
func (s EntrySource) Known() bool { return s == SourceOnDemand || s == SourcePrecomputed }

// Invalidation reasons.
const (
	ReasonCorrupt = "CORRUPT"
	ReasonManual  = "MANUAL"
)

// PromptCacheEntry is one persisted cache entry (WBS 10.1). It references
// the immutable Phase 8/9 prompt artifacts — it never stores prompt text.
type PromptCacheEntry struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	CacheKey string
	Layer    Layer

	CanonicalPromptID  *uuid.UUID
	CompressedPromptID *uuid.UUID
	WorkerPromptID     *uuid.UUID

	Status Status
	Source EntrySource

	// Slot identity (inspection filters, explain).
	StateKey   string
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	Metadata KeySourceMetadata

	// Build lease (WBS 10.31/10.32).
	BuildOwner     string
	LeaseExpiresAt *time.Time

	// Failure / negative cache (WBS 10.33/10.34).
	ErrorKind    string
	ErrorMessage string
	RetryAfter   *time.Time

	InvalidationReason string

	// Cost of the original preparation — what a hit avoids (estimated).
	CompressionCalls       int
	CompressionInputTokens int
	BuildDuration          time.Duration

	HitCount   uint64
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// Entry construction / transition errors.
var (
	ErrInvalidEntry      = errors.New("cache: invalid cache entry")
	ErrIllegalTransition = errors.New("cache: illegal cache entry status transition")
	ErrLeaseHeld         = errors.New("cache: build lease held by another owner")
	ErrNotLeaseOwner     = errors.New("cache: not the build lease owner")
)

// EntryInput carries the fields of a new VALID entry.
type EntryInput struct {
	ProjectID          uuid.UUID
	CacheKey           string
	Layer              Layer
	CanonicalPromptID  uuid.UUID
	CompressedPromptID *uuid.UUID
	WorkerPromptID     uuid.UUID
	Source             EntrySource
	StateKey           string
	PhaseID            *uuid.UUID
	WorkUnitID         *uuid.UUID
	Metadata           KeySourceMetadata
	Now                time.Time
}

// NewValidEntry builds a VALID entry (T10.1.1-T10.1.6): project, key and
// worker prompt are required; status defaults to VALID; source defaults to
// ON_DEMAND; creation time recorded.
func NewValidEntry(in EntryInput) (PromptCacheEntry, error) {
	if in.ProjectID == uuid.Nil || strings.TrimSpace(in.CacheKey) == "" || in.WorkerPromptID == uuid.Nil || in.CanonicalPromptID == uuid.Nil {
		return PromptCacheEntry{}, ErrInvalidEntry
	}
	if _, err := SchemaOf(in.CacheKey); err != nil {
		return PromptCacheEntry{}, err
	}
	src := in.Source
	if src == "" {
		src = SourceOnDemand
	}
	if !src.Known() {
		return PromptCacheEntry{}, ErrInvalidEntry
	}
	layer := in.Layer
	if layer == "" {
		layer = LayerWorker
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	canon, worker := in.CanonicalPromptID, in.WorkerPromptID
	var comp *uuid.UUID
	if in.CompressedPromptID != nil {
		c := *in.CompressedPromptID
		comp = &c
	}
	return PromptCacheEntry{
		ID: uuid.New(), ProjectID: in.ProjectID, CacheKey: in.CacheKey, Layer: layer,
		CanonicalPromptID: &canon, CompressedPromptID: comp, WorkerPromptID: &worker,
		Status: StatusValid, Source: src, StateKey: in.StateKey, PhaseID: in.PhaseID, WorkUnitID: in.WorkUnitID,
		Metadata: in.Metadata, CreatedAt: now, LastUsedAt: now,
	}, nil
}

// NewBuildingEntry builds a BUILDING entry owned by owner until now+lease
// (WBS 10.31/10.32).
func NewBuildingEntry(projectID uuid.UUID, key string, layer Layer, owner string, lease time.Duration, now time.Time) (PromptCacheEntry, error) {
	if projectID == uuid.Nil || strings.TrimSpace(key) == "" || strings.TrimSpace(owner) == "" || lease <= 0 {
		return PromptCacheEntry{}, ErrInvalidEntry
	}
	exp := now.Add(lease)
	return PromptCacheEntry{
		ID: uuid.New(), ProjectID: projectID, CacheKey: key, Layer: layer, Status: StatusBuilding,
		Source: SourceOnDemand, BuildOwner: owner, LeaseExpiresAt: &exp, CreatedAt: now, LastUsedAt: now,
	}, nil
}

var legalTransitions = map[Status]map[Status]bool{
	StatusBuilding: {StatusValid: true, StatusFailed: true},
	StatusValid:    {StatusInvalidated: true},
}

// CanTransition reports whether from → to is legal: BUILDING → VALID |
// FAILED, VALID → INVALIDATED. FAILED and INVALIDATED are terminal history.
func CanTransition(from, to Status) bool { return legalTransitions[from][to] }

// Transition applies a legal transition.
func (e *PromptCacheEntry) Transition(to Status) error {
	if !CanTransition(e.Status, to) {
		return ErrIllegalTransition
	}
	e.Status = to
	return nil
}

// Dispatchable reports whether the entry may be returned as a worker-prompt
// hit: VALID with a worker prompt (FAILED / INVALIDATED / BUILDING never).
func (e PromptCacheEntry) Dispatchable() bool {
	return e.Status == StatusValid && e.WorkerPromptID != nil
}

// LeaseActive reports whether a BUILDING entry's lease is still active.
func (e PromptCacheEntry) LeaseActive(now time.Time) bool {
	return e.Status == StatusBuilding && e.LeaseExpiresAt != nil && now.Before(*e.LeaseExpiresAt)
}

// Stale reports a BUILDING entry whose lease expired (T10.31.4).
func (e PromptCacheEntry) Stale(now time.Time) bool {
	return e.Status == StatusBuilding && !e.LeaseActive(now)
}

// Claim takes the build lease: allowed when no lease is active or the
// caller already owns it; an active foreign lease is ErrLeaseHeld; an
// expired lease is reclaimable (T10.32.1/T10.32.2).
func (e *PromptCacheEntry) Claim(owner string, lease time.Duration, now time.Time) error {
	if e.Status != StatusBuilding {
		return ErrIllegalTransition
	}
	if e.LeaseActive(now) && e.BuildOwner != owner {
		return ErrLeaseHeld
	}
	exp := now.Add(lease)
	e.BuildOwner, e.LeaseExpiresAt = owner, &exp
	return nil
}

// CheckOwner verifies the caller may finalize the build (T10.32.3): the
// current owner of an unexpired lease.
func (e PromptCacheEntry) CheckOwner(owner string, now time.Time) error {
	if e.Status != StatusBuilding {
		return ErrIllegalTransition
	}
	if e.BuildOwner != owner || !e.LeaseActive(now) {
		return ErrNotLeaseOwner
	}
	return nil
}

// Suppresses reports whether a FAILED entry's negative window suppresses a
// new provider attempt at now (WBS 10.34): only a transient failure within
// its retry window does. The key embeds the configuration, so a changed
// configuration is a different key and is never suppressed (T10.34.3).
func (e PromptCacheEntry) Suppresses(now time.Time) bool {
	return e.Status == StatusFailed && e.RetryAfter != nil && now.Before(*e.RetryAfter)
}

// TransientErrorKinds are the failure kinds that open a negative window.
var TransientErrorKinds = map[string]bool{
	"NETWORK": true, "UNAVAILABLE": true, "RATE_LIMIT": true, "TIMEOUT": true,
}
