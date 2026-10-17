package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
)

// DispatchStatus is the persisted dispatch lifecycle (WBS 12.30).
type DispatchStatus string

// Dispatch statuses.
const (
	DispatchPending   DispatchStatus = "PENDING"
	DispatchSending   DispatchStatus = "SENDING"
	DispatchWaiting   DispatchStatus = "WAITING"
	DispatchCompleted DispatchStatus = "COMPLETED"
	DispatchFailed    DispatchStatus = "FAILED"
	DispatchCancelled DispatchStatus = "CANCELLED"
	DispatchUnknown   DispatchStatus = "UNKNOWN"
)

// Terminal reports whether the status is final.
func (s DispatchStatus) Terminal() bool {
	switch s {
	case DispatchCompleted, DispatchFailed, DispatchCancelled, DispatchUnknown:
		return true
	}
	return false
}

// Valid reports whether s is known.
func (s DispatchStatus) Valid() bool {
	return s == DispatchPending || s == DispatchSending || s == DispatchWaiting || s.Terminal()
}

var dispatchTransitions = map[DispatchStatus][]DispatchStatus{
	DispatchPending: {DispatchSending, DispatchFailed, DispatchCancelled},
	DispatchSending: {DispatchWaiting, DispatchFailed, DispatchCancelled, DispatchUnknown},
	DispatchWaiting: {DispatchCompleted, DispatchFailed, DispatchCancelled, DispatchUnknown},
}

// ErrIllegalDispatchTransition is returned for a forbidden change.
var ErrIllegalDispatchTransition = errors.New("worker: illegal dispatch status transition")

// CheckDispatchTransition validates from → to; terminal statuses are never
// reopened.
func CheckDispatchTransition(from, to DispatchStatus) error {
	for _, n := range dispatchTransitions[from] {
		if n == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", ErrIllegalDispatchTransition, from, to)
}

// WorkerDispatch is one persisted send attempt (WBS 12.30/12.58).
type WorkerDispatch struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	SessionID uuid.UUID

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	WorkerPromptID uuid.UUID
	PromptHash     string

	RuntimeStateID uuid.UUID

	StateKey string
	RoleKey  string
	// WorkerModel is the model configured when the dispatch was sent
	// (authoritative grouping key, WBS 13.63).
	WorkerModel string

	// ContinuationID links a Phase 14 continuation dispatch (WBS 14.35).
	ContinuationID *uuid.UUID

	// RequestID is the end-to-end correlation id (WBS 12.32).
	RequestID uuid.UUID
	// OperationID groups attempts of one logical operation (WBS 12.58).
	OperationID string
	Attempt     int

	Status     DispatchStatus
	Delivery   *DeliveryState
	RetryClass *RetryClass

	ErrorCode    string
	ErrorMessage string
	ProviderCode string
	RetryAfter   *time.Duration
	Diagnostic   string

	Owner string

	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// NextAttempt returns the next attempt number given existing attempts
// (monotonic, 1-based; gaps never reused).
func NextAttempt(existing []int) int {
	max := 0
	for _, a := range existing {
		if a > max {
			max = a
		}
	}
	return max + 1
}

// LogicalOperationID identifies one logical worker operation (WBS 12.58):
// the same runtime state, work unit and prompt.
func LogicalOperationID(runtimeState uuid.UUID, workUnit *uuid.UUID, workerPrompt uuid.UUID) string {
	u := "-"
	if workUnit != nil {
		u = workUnit.String()
	}
	s := sha256.Sum256([]byte("worker-op:v1|" + runtimeState.String() + "|" + u + "|" + workerPrompt.String()))
	return hex.EncodeToString(s[:16])
}

// DispatchKey is the idempotency identity of one attempt (WBS 12.33).
func DispatchKey(runtimeState uuid.UUID, workUnit *uuid.UUID, workerPrompt uuid.UUID, attempt int) string {
	return fmt.Sprintf("%s#%d", LogicalOperationID(runtimeState, workUnit, workerPrompt), attempt)
}

// OutcomeFor maps a send error to the terminal dispatch status (PLAN
// Decision 5): not sent → FAILED, ambiguous → UNKNOWN, cancelled → CANCELLED
// (only when nothing could have executed, or the provider confirmed),
// provider error replies → FAILED.
func OutcomeFor(we *WorkerError) DispatchStatus {
	switch {
	case we.Delivery == DeliveryUnknown:
		return DispatchUnknown
	case we.Code == CodeCancelled:
		return DispatchCancelled
	}
	return DispatchFailed
}

// ErrPromptIntegrity is returned when a worker prompt's content no longer
// matches its hash (WBS 12.29).
var ErrPromptIntegrity = errors.New("worker: prompt content does not match its hash")

// VerifyPromptIntegrity recomputes the Phase 9 worker-prompt hash over the
// content and compares it with the stored hash; it never mutates the
// prompt.
func VerifyPromptIntegrity(p compression.WorkerPrompt) error {
	if p.Hash == "" || compression.WorkerPromptHash(p.Content, p.Source) != p.Hash {
		return ErrPromptIntegrity
	}
	return nil
}

// WorkerResponseRecord is the persisted raw response (WBS 12.31) — stored
// byte-exact before any Phase 13 parsing.
type WorkerResponseRecord struct {
	ID               uuid.UUID
	DispatchID       uuid.UUID
	ProjectID        uuid.UUID
	Content          string
	ContentHash      string
	ProviderMetadata map[string]string
	Usage            WorkerUsage
	StartedAt        time.Time
	CompletedAt      time.Time
	CreatedAt        time.Time
}

// ContentHash is the sha256 of the raw content bytes.
func ContentHash(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

// NewResponseRecord builds the persisted record of a response.
func NewResponseRecord(dispatch WorkerDispatch, resp WorkerResponse, now time.Time) WorkerResponseRecord {
	return WorkerResponseRecord{ID: uuid.New(), DispatchID: dispatch.ID, ProjectID: dispatch.ProjectID, Content: resp.Content,
		ContentHash: ContentHash(resp.Content), ProviderMetadata: resp.ProviderMetadata, Usage: resp.Usage,
		StartedAt: resp.StartedAt, CompletedAt: resp.CompletedAt, CreatedAt: now}
}
