package compression

import (
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// CompressionRequest is the explicit, provider-independent request handed to
// a Compressor (WBS 9.3). It carries only what the compression model needs:
// the canonical prompt identity, the structured (policy-framed) input, the
// compression-aware components, the token target, the model, the attempt
// number, the versioned compression instruction, and — on a retry — the
// concise validation feedback. No project object, setting, or credential is
// representable here (T9.3.5).
type CompressionRequest struct {
	CanonicalPromptID uuid.UUID

	// Content is the structured compression input (RenderStructuredInput).
	Content string

	Components []CompressionComponent

	TargetTokens int

	Model string

	Attempt int

	// InstructionContent is the versioned system instruction (WBS 9.10).
	InstructionContent string

	// Feedback is the concise validation feedback of the previous attempt
	// (WBS 9.27); empty on the first attempt.
	Feedback string
}

// Request validation errors.
var (
	ErrMissingCanonicalPrompt = errors.New("compression: request requires a canonical prompt")
	ErrInvalidTarget          = errors.New("compression: request target tokens must be positive")
	ErrInvalidAttempt         = errors.New("compression: request attempt must be >= 1")
)

// NewCompressionRequest validates and builds a request (T9.3.1-T9.3.4): the
// canonical prompt ID and content are required, the target is positive, the
// attempt is >= 1; components are copied (retained, independent of later
// input mutation).
func NewCompressionRequest(in CompressionRequest) (CompressionRequest, error) {
	if in.CanonicalPromptID == uuid.Nil || strings.TrimSpace(in.Content) == "" {
		return CompressionRequest{}, ErrMissingCanonicalPrompt
	}
	if in.TargetTokens <= 0 {
		return CompressionRequest{}, ErrInvalidTarget
	}
	if in.Attempt < 1 {
		return CompressionRequest{}, ErrInvalidAttempt
	}
	out := in
	out.Components = copyComponents(in.Components)
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
