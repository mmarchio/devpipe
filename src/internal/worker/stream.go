package worker

import (
	"context"
	"errors"
	"strings"
	"time"
)

// StreamingWorker is the optional streaming extension (WBS 12.45). The
// plain Worker interface stays fully usable without it.
type StreamingWorker interface {
	Stream(ctx context.Context, req WorkerRequest) (WorkerStream, error)
}

// WorkerStream yields generic stream events; Next returns io.EOF-like
// ErrStreamDone after the terminal event.
type WorkerStream interface {
	Next(ctx context.Context) (StreamEvent, error)
	Close() error
}

// ErrStreamDone ends a stream.
var ErrStreamDone = errors.New("worker: stream done")

// StreamEventKind is a provider-independent event kind (WBS 12.46).
type StreamEventKind string

// Stream event kinds.
const (
	StreamStarted      StreamEventKind = "STARTED"
	StreamContentDelta StreamEventKind = "CONTENT_DELTA"
	StreamToolActivity StreamEventKind = "TOOL_ACTIVITY"
	StreamUsageUpdate  StreamEventKind = "USAGE_UPDATE"
	StreamCompleted    StreamEventKind = "COMPLETED"
	StreamError        StreamEventKind = "ERROR"
)

// StreamEvent is one generic stream event.
type StreamEvent struct {
	Kind     StreamEventKind
	Sequence int64
	Delta    string
	Tool     *ToolActivity
	Usage    *WorkerUsage
	Response *WorkerResponse
	Err      *WorkerError
	At       time.Time
}

// ToolState is the state of a tool invocation (WBS 12.47).
type ToolState string

// Tool states.
const (
	ToolStarted   ToolState = "STARTED"
	ToolCompleted ToolState = "COMPLETED"
	ToolFailed    ToolState = "FAILED"
)

// ToolActivity is non-authoritative tool telemetry (WBS 12.47): it never
// implies work-unit completion.
type ToolActivity struct {
	Name       string
	State      ToolState
	StartedAt  time.Time
	FinishedAt *time.Time
	Duration   *time.Duration
}

// Stream assembly errors.
var (
	ErrStreamOutOfOrder     = errors.New("worker: stream event out of order")
	ErrStreamDoubleComplete = errors.New("worker: stream completed twice")
	ErrStreamAfterComplete  = errors.New("worker: stream event after completion")
	ErrStreamIncomplete     = errors.New("worker: stream ended without completion")
)

// StreamAssembler builds the final response from ordered events (WBS
// 12.45.3/12.46): deltas must arrive in sequence and exactly one
// completion is accepted.
type StreamAssembler struct {
	buf       strings.Builder
	last      int64
	completed bool
	resp      WorkerResponse
	tools     []ToolActivity
	usage     *WorkerUsage
	failure   *WorkerError
}

// Add consumes one event.
func (a *StreamAssembler) Add(ev StreamEvent) error {
	if a.completed {
		if ev.Kind == StreamCompleted {
			return ErrStreamDoubleComplete
		}
		return ErrStreamAfterComplete
	}
	if ev.Sequence <= a.last {
		return ErrStreamOutOfOrder
	}
	a.last = ev.Sequence
	switch ev.Kind {
	case StreamContentDelta:
		a.buf.WriteString(ev.Delta)
	case StreamToolActivity:
		if ev.Tool != nil {
			a.tools = append(a.tools, *ev.Tool)
		}
	case StreamUsageUpdate:
		a.usage = ev.Usage
	case StreamError:
		a.completed, a.failure = true, ev.Err
	case StreamCompleted:
		a.completed = true
		if ev.Response != nil {
			a.resp = *ev.Response
		}
	}
	return nil
}

// Result returns the assembled response; content is the concatenated
// deltas unless the completion carried the authoritative full content.
func (a *StreamAssembler) Result() (WorkerResponse, []ToolActivity, error) {
	if a.failure != nil {
		return WorkerResponse{}, a.tools, a.failure
	}
	if !a.completed {
		return WorkerResponse{}, a.tools, ErrStreamIncomplete
	}
	r := a.resp
	if r.Content == "" {
		r.Content = a.buf.String()
	}
	if a.usage != nil && !r.Usage.HasAny() {
		r.Usage = *a.usage
	}
	return r, a.tools, nil
}
