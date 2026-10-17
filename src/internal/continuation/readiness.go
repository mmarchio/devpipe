package continuation

import (
	"errors"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/project"
	"overseer/internal/worker"
)

// ReadinessInput are the facts checked before a continuation dispatch
// (WBS 14.126).
type ReadinessInput struct {
	ProjectStatus project.ProjectStatus

	ContinuationPhaseID    uuid.UUID
	ContinuationWorkUnitID uuid.UUID
	CurrentPhaseID         *uuid.UUID
	CurrentWorkUnitID      *uuid.UUID

	ContinuationRuntimeID uuid.UUID
	CurrentRuntimeID      uuid.UUID

	RoleSnapshotValid bool
	StatusCaptured    bool

	SessionProjectID uuid.UUID
	ProjectID        uuid.UUID
	SessionStatus    worker.SessionStatus

	SessionCWD *string
	DesiredCWD string

	PromptReady   bool
	ProtocolValid bool

	LimitExceeded RecoveryReason
}

// ReadinessError lists every readiness problem.
type ReadinessError struct{ Problems []string }

func (e *ReadinessError) Error() string {
	return "continuation not ready: " + strings.Join(e.Problems, "; ")
}

// ErrNotReady matches any ReadinessError.
var ErrNotReady = errors.New("continuation not ready")

// Is lets errors.Is(err, ErrNotReady) match.
func (e *ReadinessError) Is(target error) bool { return target == ErrNotReady }

// Has reports whether a problem with the given prefix is present.
func (e *ReadinessError) Has(prefix string) bool {
	for _, p := range e.Problems {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// Readiness problem prefixes.
const (
	ProblemProject  = "project not RUNNING"
	ProblemPhase    = "phase not current"
	ProblemUnit     = "work unit not current"
	ProblemRuntime  = "runtime state changed"
	ProblemRole     = "role snapshot invalid"
	ProblemStatus   = "continuation status not captured"
	ProblemSession  = "session not ready"
	ProblemCWD      = "working directory not confirmed"
	ProblemPrompt   = "continuation prompt not prepared"
	ProblemProtocol = "response protocol invalid"
	ProblemLimits   = "attempt limits exceeded"
)

// ValidateReadiness returns every local readiness problem (nil = ready).
func ValidateReadiness(in ReadinessInput) error {
	var p []string
	if in.ProjectStatus != project.StatusRUNNING {
		p = append(p, ProblemProject)
	}
	if in.CurrentPhaseID == nil || *in.CurrentPhaseID != in.ContinuationPhaseID {
		p = append(p, ProblemPhase)
	}
	if in.CurrentWorkUnitID == nil || *in.CurrentWorkUnitID != in.ContinuationWorkUnitID {
		p = append(p, ProblemUnit)
	}
	if in.CurrentRuntimeID != in.ContinuationRuntimeID {
		p = append(p, ProblemRuntime)
	}
	if !in.RoleSnapshotValid {
		p = append(p, ProblemRole)
	}
	if !in.StatusCaptured {
		p = append(p, ProblemStatus)
	}
	if in.SessionStatus != worker.SessionActive || in.SessionProjectID != in.ProjectID {
		p = append(p, ProblemSession)
	}
	if in.SessionCWD == nil || *in.SessionCWD != in.DesiredCWD {
		p = append(p, ProblemCWD)
	}
	if !in.PromptReady {
		p = append(p, ProblemPrompt)
	}
	if !in.ProtocolValid {
		p = append(p, ProblemProtocol)
	}
	if in.LimitExceeded != "" {
		p = append(p, ProblemLimits+": "+string(in.LimitExceeded))
	}
	if len(p) == 0 {
		return nil
	}
	return &ReadinessError{Problems: p}
}
