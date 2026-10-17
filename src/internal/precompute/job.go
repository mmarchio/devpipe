package precompute

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// FragmentManifest describes the dynamic pieces a precomputed static
// fragment still needs (WBS 11.13). Immutable once created.
type FragmentManifest struct {
	ID                    uuid.UUID
	ProjectID             uuid.UUID
	FragmentID            uuid.UUID
	FragmentKey           string
	RequiredDynamicInputs []DynamicInputType
	SourceSnapshotHash    string
	ProjectionHash        string
	CreatedAt             time.Time
}

// ErrInvalidManifest rejects an invalid manifest.
var ErrInvalidManifest = errors.New("precompute: invalid fragment manifest")

// NewFragmentManifest validates a manifest (T11.13.1/T11.13.2/T11.13.4).
func NewFragmentManifest(m FragmentManifest) (FragmentManifest, error) {
	if m.ProjectID == uuid.Nil || m.FragmentID == uuid.Nil || m.SourceSnapshotHash == "" || len(m.RequiredDynamicInputs) == 0 {
		return FragmentManifest{}, ErrInvalidManifest
	}
	for _, in := range m.RequiredDynamicInputs {
		if !in.Known() {
			return FragmentManifest{}, ErrInvalidManifest
		}
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	m.RequiredDynamicInputs = append([]DynamicInputType(nil), m.RequiredDynamicInputs...)
	return m, nil
}

// JobStatus is the lifecycle status of a precompute job (WBS 11.36).
type JobStatus string

const (
	JobPending   JobStatus = "PENDING"
	JobRunning   JobStatus = "RUNNING"
	JobCompleted JobStatus = "COMPLETED"
	JobPartial   JobStatus = "PARTIAL"
	JobFailed    JobStatus = "FAILED"
	JobCancelled JobStatus = "CANCELLED"
)

// Terminal reports whether a status is final.
func (s JobStatus) Terminal() bool {
	return s == JobCompleted || s == JobPartial || s == JobFailed || s == JobCancelled
}

var jobTransitions = map[JobStatus]map[JobStatus]bool{
	JobPending: {JobRunning: true, JobCancelled: true, JobFailed: true},
	JobRunning: {JobCompleted: true, JobPartial: true, JobFailed: true, JobCancelled: true},
}

// CanTransition reports whether a job may move from → to.
func CanTransition(from, to JobStatus) bool { return jobTransitions[from][to] }

// CandidateResult is the outcome of one candidate.
type CandidateResult string

const (
	ResultHit      CandidateResult = "CACHE_HIT"
	ResultBuilt    CandidateResult = "PROMPT_BUILT"
	ResultFragment CandidateResult = "FRAGMENT"
	// ResultTemplate: a placeholder template was built (Decision 5).
	ResultTemplate CandidateResult = "PLACEHOLDER_TEMPLATE"
	// ResultCanonical: compression disabled — the canonical prompt was
	// warmed (Layer 1) without a worker prompt.
	ResultCanonical CandidateResult = "CANONICAL_ONLY"
	ResultSkipped   CandidateResult = "SKIPPED"
	ResultFailed    CandidateResult = "FAILED"
	ResultPending   CandidateResult = "PENDING"
)

// Job is a persisted precompute job (WBS 11.36).
type Job struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Trigger   Trigger
	Status    JobStatus
	Owner     string

	BaseSnapshotID *uuid.UUID
	StateKey       string
	WorkUnitID     *uuid.UUID

	Depth         int
	MaxCandidates int

	CandidateCount int
	HitCount       int
	BuiltCount     int
	FragmentCount  int
	TemplateCount  int
	SkippedCount   int
	FailedCount    int

	Reason string

	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	HeartbeatAt time.Time
}

// CandidateRecord is the persisted result of one candidate (WBS 11.37).
type CandidateRecord struct {
	ID        uuid.UUID
	JobID     uuid.UUID
	ProjectID uuid.UUID

	StateKey   string
	RoleKey    string
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	Paths      [][]string
	Depth      int
	NextUnit   bool

	Readiness Readiness
	Priority  int
	Missing   []DynamicInputType
	Reason    string

	Result       CandidateResult
	CacheKey     string
	CacheEntryID *uuid.UUID
	FragmentIDs  []uuid.UUID
	TemplateID   *uuid.UUID
	ProjectionID *uuid.UUID

	CreatedAt time.Time
}

// Result is the summary of a job (WBS 11.46).
type Result struct {
	JobID uuid.UUID

	Candidates   int
	ExistingHits int
	FullPrompts  int
	Fragments    int
	Templates    int
	Skipped      int
	Failed       int

	Status JobStatus
}

// Summarize aggregates candidate records into a result and the job's final
// status: COMPLETED when nothing failed, PARTIAL when some candidates
// failed but others succeeded, FAILED when every attempted candidate failed
// (T11.36.4, T11.46.1-T11.46.3, T11.47).
func Summarize(jobID uuid.UUID, recs []CandidateRecord) Result {
	r := Result{JobID: jobID, Candidates: len(recs)}
	for _, c := range recs {
		switch c.Result {
		case ResultHit:
			r.ExistingHits++
		case ResultBuilt, ResultCanonical:
			r.FullPrompts++
		case ResultFragment:
			r.Fragments++
		case ResultTemplate:
			r.Templates++
		case ResultSkipped:
			r.Skipped++
		case ResultFailed, ResultPending:
			r.Failed++
		}
	}
	succeeded := r.ExistingHits + r.FullPrompts + r.Fragments + r.Templates
	switch {
	case r.Failed == 0:
		r.Status = JobCompleted
	case succeeded > 0 || r.Skipped > 0:
		r.Status = JobPartial
	default:
		r.Status = JobFailed
	}
	return r
}
