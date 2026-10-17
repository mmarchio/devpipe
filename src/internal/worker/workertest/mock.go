// Package workertest provides a scriptable in-memory worker and the
// reusable worker contract suite (WBS 12.69) every adapter runs.
package workertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"overseer/internal/worker"
)

// MockWorker is a deterministic in-memory Worker.
type MockWorker struct {
	mu      sync.Mutex
	caps    worker.WorkerCapabilities
	state   worker.ConnectionState
	session *worker.SessionInfo
	seq     int
	busy    bool

	// Delay is applied to the next Send only.
	delay time.Duration
	// NextErr is returned by the next Send (then cleared).
	nextErr error
	// Reply builds the content for a request (default: echo of the
	// request id).
	Reply func(req worker.WorkerRequest) (string, worker.WorkerUsage)

	Sends []worker.WorkerRequest
}

var (
	_ worker.Worker    = (*MockWorker)(nil)
	_ worker.Connector = (*MockWorker)(nil)
)

// NewMockWorker returns a mock advertising caps.
func NewMockWorker(caps worker.WorkerCapabilities) *MockWorker {
	if caps.Provider == "" {
		caps.Provider = worker.ProviderLocalAgent
	}
	return &MockWorker{caps: caps, state: worker.ConnDisconnected}
}

// FullCapabilities is a mock advertising every Phase 12 capability.
func FullCapabilities() worker.WorkerCapabilities {
	return worker.WorkerCapabilities{Provider: worker.ProviderLocalAgent, ProviderVersion: "mock-1", ProtocolVersion: "1", Sessions: true,
		NewSession: true, ChangeDirectory: true, ContextTelemetry: true, Cancellation: true}
}

// DelayNext delays the next Send.
func (m *MockWorker) DelayNext(d time.Duration) { m.mu.Lock(); m.delay = d; m.mu.Unlock() }

// FailNext makes the next Send return err.
func (m *MockWorker) FailNext(err error) { m.mu.Lock(); m.nextErr = err; m.mu.Unlock() }

// Connect implements worker.Connector.
func (m *MockWorker) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == worker.ConnClosed || m.state == worker.ConnClosing {
		return worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "closed", nil)
	}
	m.state = worker.ConnConnected
	return nil
}

// ConnectionState implements worker.Connector.
func (m *MockWorker) ConnectionState() worker.ConnectionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Capabilities implements worker.Worker.
func (m *MockWorker) Capabilities(context.Context) (worker.WorkerCapabilities, error) {
	return m.caps, nil
}

// NewSession implements worker.Worker.
func (m *MockWorker) NewSession(ctx context.Context, req worker.NewSessionRequest) (worker.SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.caps.NewSession {
		return worker.SessionInfo{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "sessions unsupported", nil)
	}
	if m.state == worker.ConnClosing || m.state == worker.ConnClosed {
		return worker.SessionInfo{}, worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "closed", nil)
	}
	m.seq++
	s := worker.SessionInfo{ID: fmt.Sprintf("mock-session-%d", m.seq), Provider: m.caps.Provider, CreatedAt: time.Now().UTC()}
	m.session = &s
	return s, nil
}

// ChangeDirectory implements worker.Worker.
func (m *MockWorker) ChangeDirectory(ctx context.Context, req worker.ChangeDirectoryRequest) error {
	if err := worker.ValidateDirectoryRequest(req.Path, nil, nil); err != nil {
		return worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, err.Error(), err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no session", nil)
	}
	p := req.Path
	m.session.WorkingDirectory = &p
	return nil
}

// Session implements worker.Worker.
func (m *MockWorker) Session(context.Context) (worker.SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return worker.SessionInfo{}, worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no session", nil)
	}
	return *m.session, nil
}

// Send implements worker.Worker.
func (m *MockWorker) Send(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	if err := req.Validate(); err != nil {
		return worker.WorkerResponse{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, err.Error(), err)
	}
	if err := ctx.Err(); err != nil {
		return worker.WorkerResponse{}, ctxError(err, worker.DeliveryNotSent)
	}
	m.mu.Lock()
	if m.state == worker.ConnClosing || m.state == worker.ConnClosed {
		m.mu.Unlock()
		return worker.WorkerResponse{}, worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "closed", nil)
	}
	if m.caps.Sessions && m.session == nil {
		m.mu.Unlock()
		return worker.WorkerResponse{}, worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no session", nil)
	}
	if m.busy {
		m.mu.Unlock()
		return worker.WorkerResponse{}, worker.NewError(worker.CodeBusy, worker.DeliveryNotSent, "session busy", nil)
	}
	m.busy = true
	delay, nextErr := m.delay, m.nextErr
	m.delay, m.nextErr = 0, nil
	m.Sends = append(m.Sends, req)
	var sess worker.SessionInfo
	if m.session != nil {
		sess = *m.session
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
	started := time.Now().UTC()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return worker.WorkerResponse{}, ctxError(ctx.Err(), worker.DeliveryUnknown)
		}
	}
	if nextErr != nil {
		return worker.WorkerResponse{}, nextErr
	}
	content, usage := "ack "+req.RequestID.String(), worker.WorkerUsage{InputTokens: worker.Int64(int64(len(req.Prompt.Content) / 4))}
	if m.caps.ContextTelemetry {
		usage.ContextTokens, usage.ContextWindow = worker.Int64(1000), worker.Int64(64000)
	}
	if m.Reply != nil {
		content, usage = m.Reply(req)
	}
	return worker.WorkerResponse{RequestID: req.RequestID, Content: content, Session: sess, Usage: usage, StartedAt: started,
		CompletedAt: time.Now().UTC(), ProviderMetadata: map[string]string{"provider": string(m.caps.Provider)}}, nil
}

// Close implements worker.Worker.
func (m *MockWorker) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = worker.ConnClosed
	return nil
}

func ctxError(err error, d worker.DeliveryState) *worker.WorkerError {
	if errors.Is(err, context.DeadlineExceeded) {
		return worker.NewError(worker.CodeTimeout, d, "request timed out", err)
	}
	return worker.NewError(worker.CodeCancelled, d, "request cancelled", err)
}
