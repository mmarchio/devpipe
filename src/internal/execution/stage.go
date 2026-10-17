package execution

import (
	"errors"
	"fmt"
)

// BootstrapStage is one persisted initialization stage (WBS 15.9).
type BootstrapStage string

// Bootstrap stages in progression order.
const (
	StageValidating          BootstrapStage = "VALIDATING"
	StageSnapshotting        BootstrapStage = "SNAPSHOTTING"
	StageInitializingFiles   BootstrapStage = "INITIALIZING_FILES"
	StageInitializingRuntime BootstrapStage = "INITIALIZING_RUNTIME"
	StagePreparingContext    BootstrapStage = "PREPARING_CONTEXT"
	StageFinalizing          BootstrapStage = "FINALIZING"
	StageComplete            BootstrapStage = "COMPLETE"
	StageFailed              BootstrapStage = "FAILED"
)

// ErrInvalidStage reports an empty or unknown stage.
var ErrInvalidStage = errors.New("execution: invalid bootstrap stage")

// ErrIllegalStage reports a disallowed stage advance.
var ErrIllegalStage = errors.New("execution: illegal bootstrap stage advance")

// stageOrder is the forward progression (FAILED is off-path).
var stageOrder = []BootstrapStage{
	StageValidating, StageSnapshotting, StageInitializingFiles, StageInitializingRuntime,
	StagePreparingContext, StageFinalizing, StageComplete,
}

// AllStages returns every stage (progression order, then FAILED).
func AllStages() []BootstrapStage {
	return append(append([]BootstrapStage(nil), stageOrder...), StageFailed)
}

// ParseStage accepts exactly one canonical stage token.
func ParseStage(s string) (BootstrapStage, error) {
	st := BootstrapStage(s)
	if !st.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidStage, s)
	}
	return st, nil
}

// Valid reports whether s is a known stage.
func (s BootstrapStage) Valid() bool {
	if s == StageFailed {
		return true
	}
	return s.index() >= 0
}

func (s BootstrapStage) index() int {
	for i, v := range stageOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// String returns the canonical token.
func (s BootstrapStage) String() string { return string(s) }

// Next returns the following stage on the forward path ("" at COMPLETE /
// FAILED).
func (s BootstrapStage) Next() BootstrapStage {
	i := s.index()
	if i < 0 || i == len(stageOrder)-1 {
		return ""
	}
	return stageOrder[i+1]
}

// Before reports whether s precedes o on the forward path.
func (s BootstrapStage) Before(o BootstrapStage) bool {
	a, b := s.index(), o.index()
	return a >= 0 && b >= 0 && a < b
}

// CanAdvanceStage reports whether from → to is allowed (WBS 15.9/15.87):
// the forward path one step at a time, any nonterminal stage → FAILED, and
// FAILED → failedStage (resume of the recorded failed stage). COMPLETE is
// terminal.
func CanAdvanceStage(from, to, failedStage BootstrapStage) bool {
	switch {
	case from == StageComplete:
		return false
	case from == StageFailed:
		return failedStage != "" && failedStage != StageFailed && failedStage != StageComplete && to == failedStage
	case to == StageFailed:
		return from.Valid()
	}
	return from.Next() == to && to != ""
}

// ValidateStageAdvance returns ErrIllegalStage for a disallowed advance.
func ValidateStageAdvance(from, to, failedStage BootstrapStage) error {
	if !CanAdvanceStage(from, to, failedStage) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalStage, from, to)
	}
	return nil
}
