package project

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Settings holds the per-project runtime configuration, kept separate from
// the Project entity (WBS 2.2). Provider-specific configuration does not
// belong on the Project; it lives here as plain fields so later phases can
// add model parameters without redesigning the project model.
type Settings struct {
	ProjectID uuid.UUID

	WorkingDirectory string

	WorkerProvider string
	WorkerModel    string

	CompressionProvider string
	CompressionModel    string

	ContextWindow    int
	ContextThreshold float64

	AutoContinue  bool
	MaxRetries    int
	WorkerTimeout time.Duration

	// PlanningPolicy is the project's configured planning policy (WBS 8.16);
	// an empty value marks the non-atomic-project case. PlanningGranularity
	// is the project's planning-granularity setting (planning granularity
	// remains project configuration, not an implementation assumption).
	PlanningPolicy      string
	PlanningGranularity string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewSettings constructs a valid Settings record associated with exactly one
// project (T2.2.1), validating the numeric constraints (T2.2.3–T2.2.6).
// WorkingDirectory is retained exactly as given, without trimming or path
// normalization (T2.2.7). The project ID must be non-zero.
func NewSettings(
	projectID uuid.UUID,
	workingDirectory, workerProvider, workerModel string,
	compressionProvider, compressionModel string,
	contextWindow int, contextThreshold float64,
	autoContinue bool, maxRetries int, workerTimeout time.Duration,
) (*Settings, error) {
	if projectID == uuid.Nil {
		return nil, fmt.Errorf("project: settings require a project ID")
	}
	if contextWindow <= 0 {
		return nil, fmt.Errorf("project: context window must be positive, got %d", contextWindow)
	}
	if contextThreshold <= 0 || contextThreshold > 1 {
		return nil, fmt.Errorf("project: context threshold must be in (0, 1], got %v", contextThreshold)
	}
	if maxRetries < 0 {
		return nil, fmt.Errorf("project: max retries must not be negative, got %d", maxRetries)
	}
	if workerTimeout <= 0 {
		return nil, fmt.Errorf("project: worker timeout must be positive, got %v", workerTimeout)
	}
	now := time.Now().UTC()
	return &Settings{
		ProjectID:           projectID,
		WorkingDirectory:    workingDirectory,
		WorkerProvider:      workerProvider,
		WorkerModel:         workerModel,
		CompressionProvider: compressionProvider,
		CompressionModel:    compressionModel,
		ContextWindow:       contextWindow,
		ContextThreshold:    contextThreshold,
		AutoContinue:        autoContinue,
		MaxRetries:          maxRetries,
		WorkerTimeout:       workerTimeout,
		CreatedAt:           now,
		UpdatedAt:           now,
	}, nil
}
