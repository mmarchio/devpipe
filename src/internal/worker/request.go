package worker

import (
	"errors"

	"github.com/google/uuid"
)

// Request validation errors (WBS 12.9).
var (
	ErrRequestIDRequired = errors.New("worker: request id required")
	ErrProjectRequired   = errors.New("worker: project id required")
	ErrPromptRequired    = errors.New("worker: worker prompt required")
	ErrAttemptInvalid    = errors.New("worker: attempt must be positive")
)

// Validate checks a request before it reaches any adapter. Phase and work
// unit are optional.
func (r WorkerRequest) Validate() error {
	switch {
	case r.RequestID == uuid.Nil:
		return ErrRequestIDRequired
	case r.ProjectID == uuid.Nil:
		return ErrProjectRequired
	case r.Prompt.ID == uuid.Nil || r.Prompt.Content == "" || r.Prompt.Hash == "":
		return ErrPromptRequired
	case r.Metadata.Attempt < 0:
		return ErrAttemptInvalid
	}
	return nil
}
