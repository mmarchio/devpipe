package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
)

// CanonicalPromptComponentRepository persists the composed components (with
// compression policies) and the preservation manifest of a canonical prompt
// (PLAN P9-12/P9-13). Both are written once, in the canonical prompt's
// creation transaction, and never rewritten.
type CanonicalPromptComponentRepository interface {
	// CreateComponents stores the components in ordinal order.
	CreateComponents(ctx context.Context, canonicalPromptID uuid.UUID, comps []compression.CompressionComponent) error
	// ListComponents returns the components in ordinal order (empty when the
	// prompt predates component persistence).
	ListComponents(ctx context.Context, canonicalPromptID uuid.UUID) ([]compression.CompressionComponent, error)
	// CreateManifest stores the preservation manifest.
	CreateManifest(ctx context.Context, canonicalPromptID uuid.UUID, m compression.Manifest) error
	// ListManifest returns the manifest in ordinal order.
	ListManifest(ctx context.Context, canonicalPromptID uuid.UUID) (compression.Manifest, error)
	WithTx(tx Transaction) CanonicalPromptComponentRepository
}

// CompressionSettingsRecord is the stored compression-specific settings of a
// project (only the compression fields of Settings are persisted; provider,
// model, context window and threshold come from project_settings).
type CompressionSettingsRecord struct {
	ProjectID uuid.UUID
	Settings  compression.Settings
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CompressionSettingsRepository persists project compression settings (WBS
// 9.2). Get returns ErrNotFound when the project has no stored row (the
// caller applies the defaults).
type CompressionSettingsRepository interface {
	Get(ctx context.Context, projectID uuid.UUID) (*CompressionSettingsRecord, error)
	Upsert(ctx context.Context, rec *CompressionSettingsRecord) error
	WithTx(tx Transaction) CompressionSettingsRepository
}

// CompressionInstructionRepository persists versioned compression
// instructions (WBS 9.10). Versions are append-only.
type CompressionInstructionRepository interface {
	// Append stores content as the next version.
	Append(ctx context.Context, content string) (compression.Instruction, error)
	// Get returns one version (ErrNotFound when absent).
	Get(ctx context.Context, version int) (compression.Instruction, error)
	// GetCurrent returns the highest version (ErrNotFound when none).
	GetCurrent(ctx context.Context) (compression.Instruction, error)
	// List returns every version ascending.
	List(ctx context.Context) ([]compression.Instruction, error)
	// EnsureDefault seeds the built-in v1 instruction when the table is
	// empty (idempotent) and returns the current instruction.
	EnsureDefault(ctx context.Context) (compression.Instruction, error)
}

// CompressionRepository persists compression runs, attempts, compressed
// prompts and worker prompts (WBS 9.13/9.26/9.32). Attempts, compressed
// prompts and worker prompts are immutable once written; a run is created
// PENDING and finalized exactly once.
type CompressionRepository interface {
	CreateRun(ctx context.Context, run *compression.Run) error
	// FinishRun sets the outcome of a PENDING run; finishing a run twice is
	// ErrConflict.
	FinishRun(ctx context.Context, runID uuid.UUID, outcome compression.RunOutcome, reason string, attempts int, finishedAt time.Time) error
	GetRun(ctx context.Context, runID uuid.UUID) (*compression.Run, error)
	ListRunsByCanonical(ctx context.Context, canonicalPromptID uuid.UUID) ([]*compression.Run, error)
	ListRunsByProject(ctx context.Context, projectID uuid.UUID) ([]*compression.Run, error)
	// ListRunsByIdentity returns runs with the request identity, newest first.
	ListRunsByIdentity(ctx context.Context, identity string) ([]*compression.Run, error)

	CreateAttempt(ctx context.Context, canonicalPromptID uuid.UUID, a *compression.Attempt) error
	ListAttemptsByRun(ctx context.Context, runID uuid.UUID) ([]compression.Attempt, error)
	// ListAttemptsByCanonical returns every attempt of the canonical prompt
	// ordered by run creation then attempt number.
	ListAttemptsByCanonical(ctx context.Context, canonicalPromptID uuid.UUID) ([]compression.Attempt, error)
	ListAttemptsByProject(ctx context.Context, projectID uuid.UUID) ([]compression.Attempt, error)

	CreateCompressedPrompt(ctx context.Context, cp *compression.CompressedPrompt) error
	GetCompressedPrompt(ctx context.Context, id uuid.UUID) (*compression.CompressedPrompt, error)
	ListCompressedByCanonical(ctx context.Context, canonicalPromptID uuid.UUID) ([]*compression.CompressedPrompt, error)

	CreateWorkerPrompt(ctx context.Context, projectID uuid.UUID, wp *compression.WorkerPrompt) error
	GetWorkerPrompt(ctx context.Context, id uuid.UUID) (*compression.WorkerPrompt, error)
	GetWorkerPromptByRun(ctx context.Context, runID uuid.UUID) (*compression.WorkerPrompt, error)
	ListWorkerPromptsByCanonical(ctx context.Context, canonicalPromptID uuid.UUID) ([]*compression.WorkerPrompt, error)

	WithTx(tx Transaction) CompressionRepository
}
