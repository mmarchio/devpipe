package continuation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"overseer/internal/prompt"
	"overseer/internal/workerresult"
)

// CapturedStatus is the exact normalized Phase 13 status a continuation
// uses (WBS 14.16). It is a deep copy: later edits to the source cannot
// mutate it.
type CapturedStatus struct {
	Summary    string
	Completed  []string
	Remaining  []string
	Blocking   []string
	NextAction string
}

// Capture deep-copies a normalized Phase 13 continuation status.
func Capture(s workerresult.ContinuationStatus) CapturedStatus {
	return CapturedStatus{Summary: s.Summary, Completed: cloneList(s.Completed), Remaining: cloneList(s.Remaining),
		Blocking: cloneList(s.Blocking), NextAction: s.NextAction}
}

// Clone returns an independent copy.
func (c CapturedStatus) Clone() CapturedStatus {
	return CapturedStatus{Summary: c.Summary, Completed: cloneList(c.Completed), Remaining: cloneList(c.Remaining),
		Blocking: cloneList(c.Blocking), NextAction: c.NextAction}
}

// Empty reports a status without any content.
func (c CapturedStatus) Empty() bool {
	return c.Summary == "" && len(c.Completed) == 0 && len(c.Remaining) == 0 && len(c.Blocking) == 0 && c.NextAction == ""
}

func cloneList(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

type canonicalStatus struct {
	Summary    string   `json:"summary"`
	Completed  []string `json:"completed"`
	Remaining  []string `json:"remaining"`
	Blocking   []string `json:"blocking"`
	NextAction string   `json:"next_action"`
}

// Canonical is the byte-stable serialization (fixed field order, lists in
// captured order, nil lists as []).
func (c CapturedStatus) Canonical() []byte {
	b, _ := json.Marshal(canonicalStatus{Summary: c.Summary, Completed: nonNil(c.Completed), Remaining: nonNil(c.Remaining),
		Blocking: nonNil(c.Blocking), NextAction: c.NextAction})
	return b
}

// Hash is SHA-256 of the canonical status (WBS 14.17).
func (c CapturedStatus) Hash() string {
	h := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(h[:])
}

// ProgressKey hashes the structural status — completed, remaining,
// blocking and next action, never the summary wording (WBS 14.40/14.42).
func (c CapturedStatus) ProgressKey() string {
	b, _ := json.Marshal(struct {
		Completed  []string `json:"completed"`
		Remaining  []string `json:"remaining"`
		Blocking   []string `json:"blocking"`
		NextAction string   `json:"next_action"`
	}{nonNil(c.Completed), nonNil(c.Remaining), nonNil(c.Blocking), c.NextAction})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// DecodeCaptured parses a canonical status.
func DecodeCaptured(b []byte) (CapturedStatus, error) {
	var cs canonicalStatus
	if err := json.Unmarshal(b, &cs); err != nil {
		return CapturedStatus{}, err
	}
	return CapturedStatus{Summary: cs.Summary, Completed: nonNil(cs.Completed), Remaining: nonNil(cs.Remaining),
		Blocking: nonNil(cs.Blocking), NextAction: cs.NextAction}, nil
}

// PromptInput is the Phase 8 captured-status input for the continued unit.
func (c CapturedStatus) PromptInput(workUnitID uuid.UUID, capturedAt time.Time) prompt.ContinuationStatusInput {
	return prompt.ContinuationStatusInput{WorkUnitID: workUnitID, Summary: c.Summary, Completed: cloneList(c.Completed),
		Remaining: cloneList(c.Remaining), Blocking: cloneList(c.Blocking), NextAction: c.NextAction, CapturedAt: capturedAt}
}
