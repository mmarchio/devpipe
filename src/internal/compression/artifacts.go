package compression

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ValidationStatus is the validation outcome of one compressed output.
type ValidationStatus string

const (
	ValidationPending ValidationStatus = "PENDING"
	ValidationPassed  ValidationStatus = "PASSED"
	ValidationFailed  ValidationStatus = "FAILED"
	// ValidationIneffective: the output preserved everything but did not
	// save enough (WBS 9.16/9.55) — never accepted as compression.
	ValidationIneffective ValidationStatus = "INEFFECTIVE"
)

// Known reports whether v is a valid status.
func (v ValidationStatus) Known() bool {
	switch v {
	case ValidationPending, ValidationPassed, ValidationFailed, ValidationIneffective:
		return true
	}
	return false
}

// CompressedPrompt is a compressed output persisted separately from the
// canonical prompt (WBS 9.13). The canonical prompt is never overwritten.
type CompressedPrompt struct {
	ID uuid.UUID

	CanonicalPromptID uuid.UUID
	AttemptID         uuid.UUID

	CompressionInstructionVersion int

	Provider string
	Model    string

	Attempt int

	Content string
	Hash    string

	CanonicalTokens  int
	CompressedTokens int

	ValidationStatus ValidationStatus

	CreatedAt time.Time
}

// Artifact construction errors.
var (
	ErrInvalidArtifact = errors.New("compression: invalid artifact")
	// ErrUnvalidatedContent: only validated content can become a worker
	// prompt (WBS 9.32).
	ErrUnvalidatedContent = errors.New("compression: compressed prompt has not passed validation")
)

// NewCompressedPrompt validates and builds a compressed prompt; the hash is
// computed from the content (WBS 9.14).
func NewCompressedPrompt(in CompressedPrompt) (CompressedPrompt, error) {
	if in.CanonicalPromptID == uuid.Nil || in.Attempt < 1 || in.Content == "" || !in.ValidationStatus.Known() {
		return CompressedPrompt{}, ErrInvalidArtifact
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	in.Hash = CompressedPromptHash(in.Content)
	in.CompressedTokens = EstimateTokens(in.Content)
	return in, nil
}

// AttemptStatus is the outcome of one compression attempt (WBS 9.26).
type AttemptStatus string

const (
	// AttemptAccepted: the output passed validation and was accepted.
	AttemptAccepted AttemptStatus = "ACCEPTED"
	// AttemptValidationFailed: the output failed validation.
	AttemptValidationFailed AttemptStatus = "VALIDATION_FAILED"
	// AttemptIneffective: the output passed preservation checks but did not
	// meet the savings/size policy.
	AttemptIneffective AttemptStatus = "INEFFECTIVE"
	// AttemptProviderError: the provider call failed (no output).
	AttemptProviderError AttemptStatus = "PROVIDER_ERROR"
)

// Attempt is one compression attempt (WBS 9.26). Number starts at 1.
type Attempt struct {
	ID    uuid.UUID
	RunID uuid.UUID

	Number int
	Status AttemptStatus

	Provider string
	Model    string

	// ErrorKind / ErrorMessage are set for provider errors (redacted).
	ErrorKind    ErrorKind
	ErrorMessage string

	// Output is the compressed output (empty for provider errors); the
	// persisted CompressedPrompt carries it.
	Output             string
	CompressedPromptID *uuid.UUID

	Validation *ValidationResult

	InputTokens       int
	OutputTokens      int
	FinishReason      string
	ProviderRequestID string

	Duration time.Duration

	CreatedAt time.Time
}

// Accepted reports whether this is the accepted attempt.
func (a Attempt) Accepted() bool { return a.Status == AttemptAccepted }

// WorkerPromptSource names where the worker-ready content came from (WBS
// 9.32).
type WorkerPromptSource string

const (
	SourceCanonical  WorkerPromptSource = "CANONICAL"
	SourceCompressed WorkerPromptSource = "COMPRESSED"
	// SourceCommand: a workflow command sent verbatim (no canonical
	// prompt, no compression run).
	SourceCommand WorkerPromptSource = "COMMAND"
)

// WorkerPrompt is the final Phase 9 artifact: the exact content to be sent
// to the worker (WBS 9.32).
type WorkerPrompt struct {
	ID uuid.UUID

	CanonicalPromptID  uuid.UUID
	CompressedPromptID *uuid.UUID
	RunID              uuid.UUID

	Content string
	Hash    string

	Source WorkerPromptSource

	EstimatedTokens int

	CreatedAt time.Time
}

// WorkerPromptFromCanonical builds a worker prompt whose content is the
// canonical prompt itself (bypass or safe fallback, WBS 9.29/9.30).
func WorkerPromptFromCanonical(canonicalID, runID uuid.UUID, canonicalContent string, now time.Time) (WorkerPrompt, error) {
	if canonicalID == uuid.Nil || canonicalContent == "" || ContainsPlaceholder(canonicalContent) {
		return WorkerPrompt{}, ErrInvalidArtifact
	}
	return WorkerPrompt{
		ID: uuid.New(), CanonicalPromptID: canonicalID, RunID: runID,
		Content: canonicalContent, Hash: WorkerPromptHash(canonicalContent, SourceCanonical),
		Source: SourceCanonical, EstimatedTokens: EstimateTokens(canonicalContent), CreatedAt: now,
	}, nil
}

// WorkerPromptFromCommand builds the worker prompt of a workflow command
// sent verbatim.
func WorkerPromptFromCommand(command string, now time.Time) (WorkerPrompt, error) {
	if strings.TrimSpace(command) == "" || ContainsPlaceholder(command) {
		return WorkerPrompt{}, ErrInvalidArtifact
	}
	return WorkerPrompt{ID: uuid.New(), Content: command, Hash: WorkerPromptHash(command, SourceCommand), Source: SourceCommand,
		EstimatedTokens: EstimateTokens(command), CreatedAt: now}, nil
}

// WorkerPromptFromCompressed builds a worker prompt from a compressed
// prompt; only a PASSED compressed prompt is accepted (T9.32.3).
func WorkerPromptFromCompressed(cp CompressedPrompt, runID uuid.UUID, now time.Time) (WorkerPrompt, error) {
	if cp.ValidationStatus != ValidationPassed {
		return WorkerPrompt{}, ErrUnvalidatedContent
	}
	if cp.ID == uuid.Nil || cp.CanonicalPromptID == uuid.Nil || cp.Content == "" || ContainsPlaceholder(cp.Content) {
		return WorkerPrompt{}, ErrInvalidArtifact
	}
	id := cp.ID
	return WorkerPrompt{
		ID: uuid.New(), CanonicalPromptID: cp.CanonicalPromptID, CompressedPromptID: &id, RunID: runID,
		Content: cp.Content, Hash: WorkerPromptHash(cp.Content, SourceCompressed),
		Source: SourceCompressed, EstimatedTokens: EstimateTokens(cp.Content), CreatedAt: now,
	}, nil
}

// RunOutcome is the outcome of one PrepareWorkerPrompt run.
type RunOutcome string

const (
	RunBypassed          RunOutcome = "BYPASSED"
	RunCompressed        RunOutcome = "COMPRESSED"
	RunFallbackCanonical RunOutcome = "FALLBACK_CANONICAL"
	RunFailed            RunOutcome = "FAILED"
)

// Run is one prompt-preparation run over a canonical prompt: the decision,
// the attempts it made, and the outcome + reason (bypass / fallback
// decisions are recorded here, T9.29.4/T9.30.4).
type Run struct {
	ID uuid.UUID

	ProjectID         uuid.UUID
	CanonicalPromptID uuid.UUID

	RequestIdentity string
	ConfigHash      string

	InstructionVersion int

	Provider string
	Model    string
	Mode     Mode

	TargetTokens    int
	CanonicalTokens int

	Outcome RunOutcome
	Reason  string

	Dropped []DroppedComponent

	Attempts int

	WorkerPromptID *uuid.UUID

	CreatedAt time.Time
}
