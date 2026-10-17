package worker

import (
	"time"

	"github.com/google/uuid"
)

// Liveness keeps transport, session and request liveness separate (WBS
// 12.48): a silent long request is not a dead one.
type Liveness struct {
	TransportAlive   bool
	SessionAlive     bool
	RequestExecuting bool
}

// WorkerHealth is the worker health read model (WBS 12.49).
type WorkerHealth struct {
	Provider WorkerProvider

	ConnectionState ConnectionState

	SessionState *SessionStatus

	LastSuccessfulRequest *time.Time
	LastError             *WorkerErrorSummary

	Liveness Liveness
}

// BuildHealth assembles the health read model.
func BuildHealth(p WorkerProvider, conn ConnectionState, session *SessionStatus, lastOK *time.Time, lastErr *WorkerErrorSummary, executing bool) WorkerHealth {
	return WorkerHealth{Provider: p, ConnectionState: conn, SessionState: session, LastSuccessfulRequest: lastOK, LastError: lastErr,
		Liveness: Liveness{TransportAlive: conn.Healthy(), SessionAlive: session != nil && *session == SessionActive, RequestExecuting: executing}}
}

// ContextProgress is the context read model (WBS 12.51). Unknown usage is
// Known=false, never 0%.
type ContextProgress struct {
	Known     bool
	Tokens    int64
	Window    int64
	Percent   float64
	Estimated bool
	Source    UsageSource
	// Label is "Hermes" for provider-reported values, "estimated" or
	// "unknown".
	Label string
}

// BuildContextProgress derives the progress from usage and its source.
func BuildContextProgress(u WorkerUsage, src UsageSource, providerLabel string) ContextProgress {
	ut, err := u.Utilization()
	if err != nil || !ut.Known {
		return ContextProgress{Source: SourceUnknown, Label: "unknown"}
	}
	cp := ContextProgress{Known: true, Tokens: ut.Tokens, Window: ut.Window, Percent: ut.Percent, Estimated: u.Estimated || src == SourceEstimated, Source: src}
	if cp.Estimated {
		cp.Label = "estimated"
	} else {
		cp.Label = providerLabel
	}
	return cp
}

// CurrentRequest is the in-flight request of the worker view.
type CurrentRequest struct {
	DispatchID uuid.UUID
	WorkUnitID *uuid.UUID
	RoleKey    string
	StateKey   string
	Status     DispatchStatus
	StartedAt  time.Time
	Duration   time.Duration
}

// WorkerView is the Phase 17 worker panel read model (WBS 12.86).
type WorkerView struct {
	Provider         WorkerProvider
	Connection       ConnectionState
	SessionID        *uuid.UUID
	SessionStatus    *SessionStatus
	WorkingDirectory *string
	Context          ContextProgress
	Current          *CurrentRequest
}
