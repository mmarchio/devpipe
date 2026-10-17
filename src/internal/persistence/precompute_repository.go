package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/precompute"
)

// PrecomputeSettingsRepository persists per-project precompute settings
// (WBS 11.3). Get returns the defaults with stored=false when absent.
type PrecomputeSettingsRepository interface {
	Get(ctx context.Context, projectID uuid.UUID) (precompute.Settings, bool, error)
	Upsert(ctx context.Context, projectID uuid.UUID, s precompute.Settings) error
}

// ProjectionRepository persists projected contexts (immutable).
type ProjectionRepository interface {
	Create(ctx context.Context, p *precompute.ProjectedContext) error
	Get(ctx context.Context, id uuid.UUID) (*precompute.ProjectedContext, error)
}

// JobFilter pages a project's jobs (newest first).
type JobFilter struct {
	ProjectID uuid.UUID
	Limit     int
	Offset    int
}

// PrecomputeJobRepository persists precompute jobs and candidate records
// (WBS 11.36/11.37).
type PrecomputeJobRepository interface {
	CreateJob(ctx context.Context, j *precompute.Job) error
	// StartJob moves PENDING → RUNNING (owner, started_at, heartbeat).
	StartJob(ctx context.Context, id uuid.UUID, owner string, at time.Time) (bool, error)
	// FinishJob sets a terminal status with counts (only from a
	// non-terminal status).
	FinishJob(ctx context.Context, j *precompute.Job) (bool, error)
	Heartbeat(ctx context.Context, id uuid.UUID, at time.Time) error
	GetJob(ctx context.Context, id uuid.UUID) (*precompute.Job, error)
	ListJobs(ctx context.Context, f JobFilter) ([]precompute.Job, int, error)
	// ListStale returns non-terminal jobs whose heartbeat is older than
	// before and whose owner differs from owner (restart recovery).
	ListStale(ctx context.Context, before time.Time, owner string) ([]precompute.Job, error)
	ListActive(ctx context.Context, projectID uuid.UUID) ([]precompute.Job, error)

	UpsertCandidate(ctx context.Context, c *precompute.CandidateRecord) error
	ListCandidates(ctx context.Context, jobID uuid.UUID) ([]precompute.CandidateRecord, error)
	ListProjectCandidates(ctx context.Context, projectID uuid.UUID) ([]precompute.CandidateRecord, error)
	// FailPendingCandidates marks a job's PENDING candidates FAILED.
	FailPendingCandidates(ctx context.Context, jobID uuid.UUID, reason string, at time.Time) (int, error)
}

// FragmentManifestRepository persists fragment manifests (immutable).
type FragmentManifestRepository interface {
	// CreateIfAbsent stores the manifest unless (fragment, projection
	// hash) exists.
	CreateIfAbsent(ctx context.Context, m *precompute.FragmentManifest) (*precompute.FragmentManifest, bool, error)
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]precompute.FragmentManifest, error)
}

// TransitionLatency is one measured transition (WBS 11.53).
type TransitionLatency struct {
	ID               uuid.UUID
	ProjectID        uuid.UUID
	StateKey         string
	WorkUnitID       *uuid.UUID
	WorkerPromptID   *uuid.UUID
	CacheEntryID     *uuid.UUID
	Source           string
	ResultReceivedAt time.Time
	PromptReadyAt    time.Time
	Duration         time.Duration
	CreatedAt        time.Time
}

// TransitionLatencyRepository persists measured transition latencies.
type TransitionLatencyRepository interface {
	Create(ctx context.Context, l *TransitionLatency) error
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]TransitionLatency, error)
}

// Template statuses (Decision 5).
const (
	TemplatePending  = "PENDING"
	TemplateResolved = "RESOLVED"
	TemplateStale    = "STALE"
	TemplateFailed   = "FAILED"
)

// PrecomputedTemplate is a pending placeholder template (PLAN Decision 5).
type PrecomputedTemplate struct {
	ID                 uuid.UUID
	ProjectID          uuid.UUID
	ProjectionID       uuid.UUID
	WorkUnitID         uuid.UUID
	StateKey           string
	NormalizedKey      string
	PlaceholderToken   string
	CanonicalPromptID  uuid.UUID
	CompressedPromptID *uuid.UUID
	ContentSource      string
	Status             string
	ResolvedEntryID    *uuid.UUID
	Reason             string
	CreatedAt          time.Time
	ResolvedAt         *time.Time
}

// PrecomputedTemplateRepository persists placeholder templates.
type PrecomputedTemplateRepository interface {
	// Create stores a PENDING template; a concurrent PENDING template for
	// the same normalized key wins (created=false, winner returned).
	Create(ctx context.Context, t *PrecomputedTemplate) (*PrecomputedTemplate, bool, error)
	GetPending(ctx context.Context, projectID uuid.UUID, normalizedKey string) (*PrecomputedTemplate, error)
	Get(ctx context.Context, id uuid.UUID) (*PrecomputedTemplate, error)
	// Finish moves a PENDING template to RESOLVED / STALE / FAILED.
	Finish(ctx context.Context, id uuid.UUID, status, reason string, entryID *uuid.UUID, at time.Time) (bool, error)
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]PrecomputedTemplate, error)
}
