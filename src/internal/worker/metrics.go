package worker

import "time"

// EventType is a worker audit event type (WBS 12.53).
type EventType string

// Worker audit events.
const (
	EventConnected        EventType = "WORKER_CONNECTED"
	EventDisconnected     EventType = "WORKER_DISCONNECTED"
	EventReconnected      EventType = "WORKER_RECONNECTED"
	EventSessionCreated   EventType = "SESSION_CREATED"
	EventSessionReset     EventType = "SESSION_RESET"
	EventSessionLost      EventType = "SESSION_LOST"
	EventSessionClosed    EventType = "SESSION_CLOSED"
	EventRequestStarted   EventType = "WORKER_REQUEST_STARTED"
	EventRequestCompleted EventType = "WORKER_REQUEST_COMPLETED"
	EventRequestFailed    EventType = "WORKER_REQUEST_FAILED"
	EventRequestCancelled EventType = "WORKER_REQUEST_CANCELLED"
	EventDeliveryUnknown  EventType = "WORKER_DELIVERY_UNKNOWN"
	EventCWDChanged       EventType = "WORKER_CWD_CHANGED"
	EventContextObserved  EventType = "WORKER_CONTEXT_OBSERVED"
	// EventConnectionFailed records a failed connect / reconnect attempt
	// (metric source for "connection failures", WBS 12.54).
	EventConnectionFailed EventType = "WORKER_CONNECTION_FAILED"

	// Phase 13 result events (WBS 13.64).
	EventResultParseStarted EventType = "WORKER_RESULT_PARSE_STARTED"
	EventResultParsed       EventType = "WORKER_RESULT_PARSED"
	EventResultRejected     EventType = "WORKER_RESULT_REJECTED"
	EventResultRepaired     EventType = "WORKER_RESULT_REPAIRED"
	EventResultPersisted    EventType = "WORKER_RESULT_PERSISTED"
)

// MetricsInput is what metrics aggregate over.
type MetricsInput struct {
	Dispatches []WorkerDispatch
	Events     []EventType
	// Utilization samples (percent) of known context observations.
	Utilization  []float64
	InputTokens  int64
	OutputTokens int64
}

// WorkerMetrics are worker counters (WBS 12.54).
type WorkerMetrics struct {
	Requests        int
	Successful      int
	Failed          int
	Cancelled       int
	DeliveryUnknown int
	InFlight        int

	SessionsCreated int
	SessionsLost    int
	SessionResets   int

	ConnectionFailures int
	Reconnections      int

	TotalDuration time.Duration
	AvgDuration   time.Duration

	InputTokens  int64
	OutputTokens int64

	AvgUtilization  float64
	PeakUtilization float64
}

// AggregateMetrics computes the counters.
func AggregateMetrics(in MetricsInput) WorkerMetrics {
	var m WorkerMetrics
	timed := 0
	for _, d := range in.Dispatches {
		m.Requests++
		switch d.Status {
		case DispatchCompleted:
			m.Successful++
		case DispatchFailed:
			m.Failed++
		case DispatchCancelled:
			m.Cancelled++
		case DispatchUnknown:
			m.DeliveryUnknown++
		default:
			m.InFlight++
		}
		if d.StartedAt != nil && d.CompletedAt != nil {
			m.TotalDuration += d.CompletedAt.Sub(*d.StartedAt)
			timed++
		}
	}
	if timed > 0 {
		m.AvgDuration = m.TotalDuration / time.Duration(timed)
	}
	for _, e := range in.Events {
		switch e {
		case EventSessionCreated:
			m.SessionsCreated++
		case EventSessionLost:
			m.SessionsLost++
		case EventSessionReset:
			m.SessionResets++
		case EventConnectionFailed:
			m.ConnectionFailures++
		case EventReconnected:
			m.Reconnections++
		}
	}
	m.InputTokens, m.OutputTokens = in.InputTokens, in.OutputTokens
	if n := len(in.Utilization); n > 0 {
		sum := 0.0
		for _, u := range in.Utilization {
			sum += u
			if u > m.PeakUtilization {
				m.PeakUtilization = u
			}
		}
		m.AvgUtilization = sum / float64(n)
	}
	return m
}
