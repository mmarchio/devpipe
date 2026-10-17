package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
)

// PromptCacheFilter selects cache entries for inspection (WBS 10.39).
type PromptCacheFilter struct {
	ProjectID  uuid.UUID
	StateKey   *string
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	Status     *cache.Status
	Source     *cache.EntrySource
	Layer      *cache.Layer
}

// BuildResult carries the artifact references and cost of a completed
// build.
type BuildResult struct {
	CanonicalPromptID      uuid.UUID
	CompressedPromptID     *uuid.UUID
	WorkerPromptID         uuid.UUID
	Metadata               cache.KeySourceMetadata
	Source                 cache.EntrySource
	CompressionCalls       int
	CompressionInputTokens int
	BuildDuration          time.Duration
}

// PromptCacheRepository persists prompt cache entries (WBS 10.13). At most
// one authoritative (VALID / BUILDING) entry exists per (project, key,
// layer); history (FAILED / INVALIDATED) is retained.
type PromptCacheRepository interface {
	// GetValid returns the VALID entry for the key (ErrNotFound otherwise).
	GetValid(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error)
	// GetAuthoritative returns the VALID or BUILDING entry for the key.
	GetAuthoritative(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error)
	// LatestFailed returns the newest FAILED entry for the key.
	LatestFailed(ctx context.Context, projectID uuid.UUID, key string, layer cache.Layer) (*cache.PromptCacheEntry, error)
	Get(ctx context.Context, id uuid.UUID) (*cache.PromptCacheEntry, error)
	// Insert stores a new VALID or BUILDING entry; when an authoritative
	// entry already exists for the key the insert loses and the winner is
	// returned with created=false (WBS 10.30).
	Insert(ctx context.Context, e *cache.PromptCacheEntry) (winner *cache.PromptCacheEntry, created bool, err error)
	// ReclaimLease takes over an expired BUILDING lease (WBS 10.32).
	ReclaimLease(ctx context.Context, id uuid.UUID, owner string, expires, now time.Time) (bool, error)
	// FinalizeValid completes a BUILDING entry owned by owner with an
	// unexpired lease.
	FinalizeValid(ctx context.Context, id uuid.UUID, owner string, now time.Time, r BuildResult) (bool, error)
	// FinalizeFailed marks a BUILDING entry FAILED (owner-checked).
	FinalizeFailed(ctx context.Context, id uuid.UUID, owner string, now time.Time, kind, message string, retryAfter *time.Time, md cache.KeySourceMetadata) (bool, error)
	// MarkInvalid invalidates a VALID entry with a reason.
	MarkInvalid(ctx context.Context, id uuid.UUID, reason string) (bool, error)
	// InvalidateProject invalidates every VALID entry of the project.
	InvalidateProject(ctx context.Context, projectID uuid.UUID, reason string) (int, error)
	// Touch records a hit: hit_count+1, last_used_at=now.
	Touch(ctx context.Context, id uuid.UUID, now time.Time) error
	List(ctx context.Context, f PromptCacheFilter) ([]cache.PromptCacheEntry, error)
	// LatestInSlot returns the newest non-BUILDING entry for (project,
	// state, work unit, layer) — the explain reference.
	LatestInSlot(ctx context.Context, projectID uuid.UUID, stateKey string, workUnitID *uuid.UUID, layer cache.Layer) (*cache.PromptCacheEntry, error)
	// WorkerPromptRevoked reports whether an INVALIDATED entry of the project
	// references the worker prompt (WBS 10.41: a revoked result is never
	// re-served through compression reuse).
	WorkerPromptRevoked(ctx context.Context, projectID, workerPromptID uuid.UUID) (bool, error)
	// DeleteEntries removes cache-entry rows (never prompt artifacts).
	DeleteEntries(ctx context.Context, projectID uuid.UUID, ids []uuid.UUID) (int, error)
}

// PromptCacheEventRepository persists cache audit events (WBS 10.66).
type PromptCacheEventRepository interface {
	Append(ctx context.Context, e cache.Event) error
	// ListByProject returns events newest first (limit <= 0 = all).
	ListByProject(ctx context.Context, projectID uuid.UUID, limit int) ([]cache.Event, error)
}

// PromptFragmentRepository persists static prompt fragments (WBS 10.46).
type PromptFragmentRepository interface {
	// StoreIfAbsent stores the fragment unless its key exists; returns the
	// stored fragment and whether it was created.
	StoreIfAbsent(ctx context.Context, f *cache.PromptFragment) (*cache.PromptFragment, bool, error)
	GetByKey(ctx context.Context, projectID uuid.UUID, key string) (*cache.PromptFragment, error)
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]cache.PromptFragment, error)
	IncrementHit(ctx context.Context, id uuid.UUID) error
}

// PromptCacheSettingsRepository persists the per-project cache policy.
type PromptCacheSettingsRepository interface {
	// Get returns the stored settings, or the defaults with stored=false.
	Get(ctx context.Context, projectID uuid.UUID) (cache.Settings, bool, error)
	Upsert(ctx context.Context, projectID uuid.UUID, s cache.Settings) error
}
