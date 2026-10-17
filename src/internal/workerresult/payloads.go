package workerresult

import (
	"bytes"
	"errors"
	"time"

	"overseer/internal/prompt"
)

// ErrWrongStatus: a payload was requested for a result of another status.
var ErrWrongStatus = errors.New("workerresult: result status does not match the payload")

// ContinuationStatus is the INCOMPLETE payload for continuation prompts
// (WBS 13.56).
type ContinuationStatus struct {
	Summary    string
	Completed  []string
	Remaining  []string
	Blocking   []string
	NextAction string
}

// BlockedStatus is the BLOCKED payload (WBS 13.58).
type BlockedStatus struct {
	Summary    string
	Blocking   []string
	Remaining  []string
	NextAction string
}

// FailureStatus is the worker-reported FAILED payload (WBS 13.59) — not a
// phase failure transition.
type FailureStatus struct {
	Summary    string
	Blocking   []string
	NextAction string
}

// CompletionReport is the COMPLETE payload for later validation (WBS
// 13.60): worker claims only.
type CompletionReport struct {
	Summary          string
	Completed        []string
	TestClaim        *WorkerTestClaim
	Artifacts        []ArtifactClaim
	AcceptanceClaims []AcceptanceClaim
}

func clone(xs []string) []string { return append([]string{}, xs...) }

// Continuation converts an INCOMPLETE result.
func (r WorkerResult) Continuation() (ContinuationStatus, error) {
	if !r.IsIncomplete() {
		return ContinuationStatus{}, ErrWrongStatus
	}
	return ContinuationStatus{Summary: r.Summary, Completed: clone(r.Completed), Remaining: clone(r.Remaining), Blocking: clone(r.Blocking), NextAction: r.NextAction}, nil
}

// Blocked converts a BLOCKED result.
func (r WorkerResult) Blocked() (BlockedStatus, error) {
	if !r.IsBlocked() {
		return BlockedStatus{}, ErrWrongStatus
	}
	return BlockedStatus{Summary: r.Summary, Blocking: clone(r.Blocking), Remaining: clone(r.Remaining), NextAction: r.NextAction}, nil
}

// Failure converts a FAILED result.
func (r WorkerResult) Failure() (FailureStatus, error) {
	if !r.IsFailed() {
		return FailureStatus{}, ErrWrongStatus
	}
	return FailureStatus{Summary: r.Summary, Blocking: clone(r.Blocking), NextAction: r.NextAction}, nil
}

// Completion converts a COMPLETE result.
func (r WorkerResult) Completion() (CompletionReport, error) {
	if !r.IsComplete() {
		return CompletionReport{}, ErrWrongStatus
	}
	return CompletionReport{Summary: r.Summary, Completed: clone(r.Completed), TestClaim: r.Tests,
		Artifacts: append([]ArtifactClaim{}, r.Artifacts...), AcceptanceClaims: append([]AcceptanceClaim{}, r.AcceptanceClaims...)}, nil
}

// Serialize is the deterministic continuation serialization Phase 14
// injects into "continue + captured status" (WBS 13.57).
func (c ContinuationStatus) Serialize() []byte {
	var b bytes.Buffer
	b.WriteString(`{"summary":`)
	jstr(&b, c.Summary)
	b.WriteString(`,"completed":`)
	jlist(&b, c.Completed)
	b.WriteString(`,"remaining":`)
	jlist(&b, c.Remaining)
	b.WriteString(`,"blocking":`)
	jlist(&b, c.Blocking)
	b.WriteString(`,"next_action":`)
	jstr(&b, c.NextAction)
	b.WriteByte('}')
	return b.Bytes()
}

// PromptContinuation bridges an INCOMPLETE result to the Phase 8
// continuation-prompt input, bound to the dispatched work unit.
func (r WorkerResult) PromptContinuation(capturedAt time.Time) (*prompt.ContinuationStatus, error) {
	c, err := r.Continuation()
	if err != nil {
		return nil, err
	}
	if r.Metadata.WorkUnitID == nil {
		return nil, errors.New("workerresult: continuation requires a dispatched work unit")
	}
	return prompt.NewContinuationStatus(prompt.ContinuationStatusInput{WorkUnitID: *r.Metadata.WorkUnitID, Summary: c.Summary,
		Completed: c.Completed, Remaining: c.Remaining, Blocking: c.Blocking, NextAction: c.NextAction, CapturedAt: capturedAt})
}
