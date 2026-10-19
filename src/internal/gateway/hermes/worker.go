package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
)

// SecretResolver resolves an auth secret reference (an environment
// variable name) to its value.
type SecretResolver func(ref string) (string, bool)

// GatewayWorker is HermesGatewayWorker: the generic worker.Worker over the
// Hermes Gateway (WBS 12.1). It holds one current session; it never
// creates a session implicitly, never resends a request and never acts on
// response content or context telemetry.
type GatewayWorker struct {
	cfg    worker.WorkerConfig
	hc     HermesGatewayConfig
	client HermesGatewayClient
	secret string
	// secretFn returns the current (possibly rotated) credential.
	secretFn func() string

	mu        sync.Mutex
	state     worker.ConnectionState
	caps      *worker.WorkerCapabilities
	session   *worker.SessionInfo
	busy      bool
	inflight  map[string]context.CancelFunc
	wg        sync.WaitGroup
	observers []worker.ConnectionObserver

	// Sleep waits between reconnect attempts (injectable for tests).
	Sleep func(ctx context.Context, d time.Duration) error
	// CancelTimeout bounds the best-effort provider cancellation.
	CancelTimeout time.Duration
}

var (
	_ worker.Worker          = (*GatewayWorker)(nil)
	_ worker.Connector       = (*GatewayWorker)(nil)
	_ worker.Reconciler      = (*GatewayWorker)(nil)
	_ worker.Observable      = (*GatewayWorker)(nil)
	_ worker.StreamingWorker = (*GatewayWorker)(nil)
)

// New builds the adapter from generic configuration: the provider
// configuration is parsed and validated and the auth secret resolved
// before any connection (T12.2.4).
func New(cfg worker.WorkerConfig, resolve SecretResolver, opts ...Option) (*GatewayWorker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	hc, err := ParseConfig(cfg.ProviderConfig)
	if err != nil {
		return nil, err
	}
	var o options
	for _, f := range opts {
		f(&o)
	}
	hc.DialControl = o.dialControl
	secret := ""
	if hc.AuthSecretRef != nil {
		v, ok := "", false
		if resolve != nil {
			v, ok = resolve(*hc.AuthSecretRef)
		}
		if !ok || v == "" {
			return nil, &worker.ConfigError{Problems: []string{"credentials not resolvable (env " + *hc.AuthSecretRef + ")"}}
		}
		secret = v
	}
	// The secret is re-resolved per request so a rotated credential
	// (e.g. a replaced secret file) applies without a restart; a
	// temporarily unresolvable value keeps the last good one.
	ref := hc.AuthSecretRef
	var mu sync.Mutex
	current := secret
	tokenFn := func() string {
		mu.Lock()
		defer mu.Unlock()
		if ref != nil && resolve != nil {
			if v, ok := resolve(*ref); ok && v != "" {
				current = v
			}
		}
		return current
	}
	client, err := NewHTTPClientWithToken(hc, tokenFn)
	if err != nil {
		return nil, err
	}
	w := NewWithClient(cfg, hc, client, secret)
	w.secretFn = tokenFn
	return w, nil
}

// Option tunes the adapter.
type Option func(*options)

type options struct {
	dialControl func(network, address string, c syscall.RawConn) error
}

// WithDialControl vets every resolved address before connecting
// (endpoint policy, WBS 18.88).
func WithDialControl(f func(network, address string, c syscall.RawConn) error) Option {
	return func(o *options) { o.dialControl = f }
}

// secrets returns the values to redact: the initial and the current
// credential.
func (w *GatewayWorker) secrets() []string {
	if w.secretFn == nil {
		return []string{w.secret}
	}
	cur := w.secretFn()
	if cur == w.secret {
		return []string{w.secret}
	}
	return []string{w.secret, cur}
}

// NewWithClient builds the adapter over any gateway client (tests).
func NewWithClient(cfg worker.WorkerConfig, hc HermesGatewayConfig, client HermesGatewayClient, secret string) *GatewayWorker {
	return &GatewayWorker{cfg: cfg, hc: hc, client: client, secret: secret, state: worker.ConnDisconnected,
		inflight: map[string]context.CancelFunc{}, CancelTimeout: 2 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
}

// Register binds HERMES_GATEWAY in a provider registry (WBS 12.2).
func Register(reg *worker.ProviderRegistry, resolve SecretResolver, opts ...Option) {
	reg.Register(worker.ProviderHermesGateway, func(_ context.Context, cfg worker.WorkerConfig) (worker.Worker, error) {
		return New(cfg, resolve, opts...)
	})
}

// OnConnectionChange implements worker.Observable.
func (w *GatewayWorker) OnConnectionChange(o worker.ConnectionObserver) {
	w.mu.Lock()
	w.observers = append(w.observers, o)
	w.mu.Unlock()
}

// setLocked moves the transport state (caller holds mu) and returns the
// notification to deliver after unlocking.
func (w *GatewayWorker) setLocked(to worker.ConnectionState, cause error) func() {
	from := w.state
	if from == to {
		return func() {}
	}
	if _, err := from.Transition(to); err != nil {
		return func() {}
	}
	w.state = to
	obs := append([]worker.ConnectionObserver(nil), w.observers...)
	return func() {
		for _, o := range obs {
			o(from, to, cause)
		}
	}
}

func (w *GatewayWorker) set(to worker.ConnectionState, cause error) {
	w.mu.Lock()
	n := w.setLocked(to, cause)
	w.mu.Unlock()
	n()
}

// ConnectionState implements worker.Connector.
func (w *GatewayWorker) ConnectionState() worker.ConnectionState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// Connect establishes and validates the gateway connection (WBS 12.5). A
// failed connection is FAILED, never healthy.
func (w *GatewayWorker) Connect(ctx context.Context) error {
	w.mu.Lock()
	switch w.state {
	case worker.ConnConnected:
		w.mu.Unlock()
		return nil
	case worker.ConnClosing, worker.ConnClosed:
		w.mu.Unlock()
		return worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker closed", nil)
	}
	n := w.setLocked(worker.ConnConnecting, nil)
	w.mu.Unlock()
	n()
	return w.handshake(ctx, worker.ConnFailed)
}

func (w *GatewayWorker) handshake(ctx context.Context, failState worker.ConnectionState) error {
	h, err := w.client.Connect(ctx)
	if err == nil && h.ProtocolVersion != "" && h.ProtocolVersion != ProtocolVersion {
		err = worker.NewError(worker.CodeProtocol, worker.DeliveryNotSent, "unsupported gateway protocol version "+h.ProtocolVersion, nil)
	}
	if err != nil {
		we := worker.AsWorkerError(err).Sanitize(w.secrets()...)
		w.set(failState, we)
		return we
	}
	caps := capabilitiesFrom(h)
	w.mu.Lock()
	w.caps = &caps
	n := w.setLocked(worker.ConnConnected, nil)
	w.mu.Unlock()
	n()
	return nil
}

// Capabilities reports the gateway's advertised capabilities (WBS 12.7);
// deterministic for one handshake.
func (w *GatewayWorker) Capabilities(context.Context) (worker.WorkerCapabilities, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.caps == nil {
		return worker.WorkerCapabilities{}, worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "worker not connected", nil)
	}
	return *w.caps, nil
}

// usable checks the adapter can issue an operation (caller holds mu).
func (w *GatewayWorker) usableLocked() *worker.WorkerError {
	switch w.state {
	case worker.ConnClosing, worker.ConnClosed:
		return worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker closed", nil)
	case worker.ConnConnected:
		return nil
	}
	return worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "worker not connected ("+string(w.state)+")", nil)
}

// noteTransport marks the transport DISCONNECTED when a call failed at the
// transport level (not a gateway reply). Reconnection is explicit.
func (w *GatewayWorker) noteTransport(we *worker.WorkerError) {
	if we == nil || we.Delivery == worker.DeliveryReceived {
		return
	}
	if we.Code == worker.CodeUnavailable || we.Code == worker.CodeDeliveryUnknown {
		w.set(worker.ConnDisconnected, we)
	}
}

// NewSession creates a fresh conversational context ("new", WBS 12.19/
// 12.20). The adapter's current session switches only on success; the new
// session's working directory is UNKNOWN unless the gateway reports one.
func (w *GatewayWorker) NewSession(ctx context.Context, req worker.NewSessionRequest) (worker.SessionInfo, error) {
	w.mu.Lock()
	if e := w.usableLocked(); e != nil {
		w.mu.Unlock()
		return worker.SessionInfo{}, e
	}
	if !w.caps.NewSession {
		w.mu.Unlock()
		return worker.SessionInfo{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "gateway cannot create sessions", nil)
	}
	prev := ""
	if w.session != nil {
		prev = w.session.ID
	}
	w.mu.Unlock()
	gs, err := w.client.New(ctx, req.ProjectID.String())
	if err != nil {
		we := worker.AsWorkerError(err).Sanitize(w.secrets()...)
		w.noteTransport(we)
		return worker.SessionInfo{}, we
	}
	s := sessionFrom(gs)
	if err := s.Validate(true); err != nil {
		return worker.SessionInfo{}, worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, err.Error(), nil)
	}
	if s.ID == prev {
		return worker.SessionInfo{}, worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, "gateway returned the previous session for new", nil)
	}
	w.mu.Lock()
	w.session = &s
	w.mu.Unlock()
	return s, nil
}

// Session returns the current session.
func (w *GatewayWorker) Session(context.Context) (worker.SessionInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.session == nil {
		return worker.SessionInfo{}, worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no current session", nil)
	}
	s := *w.session
	return s, nil
}

// ChangeDirectory performs "cd <path>" (WBS 12.23). The confirmed
// directory changes only after the gateway confirms; failure leaves it
// unchanged (WBS 12.62).
func (w *GatewayWorker) ChangeDirectory(ctx context.Context, req worker.ChangeDirectoryRequest) error {
	if err := worker.ValidateDirectoryRequest(req.Path, w.hc.AllowedRoots, nil); err != nil {
		return worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, err.Error(), err)
	}
	w.mu.Lock()
	if e := w.usableLocked(); e != nil {
		w.mu.Unlock()
		return e
	}
	if w.session == nil {
		w.mu.Unlock()
		return worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no current session", nil)
	}
	id := w.session.ID
	w.mu.Unlock()
	gs, err := w.client.ChangeDirectory(ctx, id, req.Path, w.cfg.Model)
	if err != nil {
		we := worker.AsWorkerError(err).Sanitize(w.secrets()...)
		w.noteTransport(we)
		if we.Code == worker.CodeSessionLost {
			w.dropSession(id)
		}
		return we
	}
	w.mu.Lock()
	if w.session != nil && w.session.ID == id {
		d := *gs.WorkingDirectory
		w.session.WorkingDirectory = &d
	}
	w.mu.Unlock()
	return nil
}

func (w *GatewayWorker) dropSession(id string) {
	w.mu.Lock()
	if w.session != nil && w.session.ID == id {
		w.session = nil
	}
	w.mu.Unlock()
}

// begin admits one request on the current session (one in flight per
// session, WBS 12.56).
func (w *GatewayWorker) begin(ctx context.Context, req worker.WorkerRequest) (worker.SessionInfo, context.Context, func(), *worker.WorkerError) {
	if err := ctx.Err(); err != nil {
		code := worker.CodeCancelled
		if errors.Is(err, context.DeadlineExceeded) {
			code = worker.CodeTimeout
		}
		return worker.SessionInfo{}, nil, nil, worker.NewError(code, worker.DeliveryNotSent, "cancelled before send", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.usableLocked(); e != nil {
		return worker.SessionInfo{}, nil, nil, e
	}
	if w.session == nil {
		return worker.SessionInfo{}, nil, nil, worker.NewError(worker.CodeNoSession, worker.DeliveryNotSent, "no current session", nil)
	}
	if w.busy {
		return worker.SessionInfo{}, nil, nil, worker.NewError(worker.CodeBusy, worker.DeliveryNotSent, "session already has a request in flight", nil)
	}
	w.busy = true
	sctx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	rid := req.RequestID.String()
	w.inflight[rid] = cancel
	w.wg.Add(1)
	done := func() {
		cancel()
		w.mu.Lock()
		delete(w.inflight, rid)
		w.busy = false
		w.mu.Unlock()
		w.wg.Done()
	}
	return *w.session, sctx, done, nil
}

// Send transmits the prompt on the current session and returns the raw
// response (WBS 12.28). It never interprets the content and never
// resends; an in-flight cancellation is reported CANCELLED only when the
// gateway confirms it, otherwise DELIVERY_UNKNOWN (WBS 12.36/12.37).
func (w *GatewayWorker) Send(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	greq, err := Translate(req, w.cfg)
	if err != nil {
		return worker.WorkerResponse{}, err
	}
	sess, sctx, done, we := w.begin(ctx, req)
	if we != nil {
		return worker.WorkerResponse{}, we
	}
	defer done()
	key := worker.DispatchKey(req.Metadata.RuntimeStateID, req.WorkUnitID, req.Prompt.ID, req.Metadata.Attempt)
	gr, raw, err := w.client.Send(sctx, sess.ID, greq, key)
	if err != nil {
		return worker.WorkerResponse{}, w.sendFailure(sess.ID, greq.RequestID, err)
	}
	resp, err := Normalize(req.RequestID, sess.ID, gr, raw, w.secrets()...)
	if err != nil {
		return worker.WorkerResponse{}, err
	}
	w.mu.Lock()
	if w.session != nil && w.session.ID == sess.ID {
		if resp.Usage.HasAny() {
			w.session.Usage = resp.Usage
		}
		sess = *w.session
	}
	w.mu.Unlock()
	resp.Session = sess
	return resp, nil
}

func (w *GatewayWorker) sendFailure(sessionID, requestID string, err error) *worker.WorkerError {
	we := worker.AsWorkerError(err).Sanitize(w.secrets()...)
	if we.Delivery == worker.DeliveryUnknown && (we.Code == worker.CodeCancelled || we.Code == worker.CodeTimeout) {
		// Best-effort provider cancellation; only a confirmed cancel turns
		// an in-flight cancellation into CANCELLED. A timeout stays
		// ambiguous: the worker may have executed (WBS 12.36.3).
		confirmed := false
		w.mu.Lock()
		canCancel := w.caps != nil && w.caps.Cancellation
		w.mu.Unlock()
		if canCancel {
			cctx, cancel := context.WithTimeout(context.Background(), w.CancelTimeout)
			confirmed, _ = w.client.Cancel(cctx, sessionID, requestID)
			cancel()
		}
		if confirmed && we.Code == worker.CodeCancelled {
			we.Delivery = worker.DeliveryReceived
			we.Message = "request cancelled (confirmed by gateway)"
		}
		we.Retryable = we.RetryClass() == worker.RetrySafe
		return we
	}
	if we.Code == worker.CodeSessionLost {
		w.dropSession(sessionID)
	}
	w.noteTransport(we)
	return we
}

// Reconnect restores the transport with the bounded reconnect policy
// (WBS 12.40). It never resends a request and never creates a session.
func (w *GatewayWorker) Reconnect(ctx context.Context) error {
	w.mu.Lock()
	if w.state == worker.ConnClosing || w.state == worker.ConnClosed {
		w.mu.Unlock()
		return worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker closed", nil)
	}
	n := w.setLocked(worker.ConnReconnecting, nil)
	w.mu.Unlock()
	n()
	attempts := w.hc.ReconnectPolicy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 1; i <= attempts; i++ {
		h, err := w.client.Connect(ctx)
		if err == nil {
			caps := capabilitiesFrom(h)
			w.mu.Lock()
			w.caps = &caps
			n := w.setLocked(worker.ConnConnected, nil)
			w.mu.Unlock()
			n()
			return nil
		}
		last = err
		for _, o := range w.observersCopy() {
			o(worker.ConnReconnecting, worker.ConnReconnecting, err)
		}
		if i < attempts {
			if err := w.Sleep(ctx, w.hc.ReconnectPolicy.Delay(i)); err != nil {
				last = err
				break
			}
		}
	}
	we := worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "reconnect failed", last).Sanitize(w.secrets()...)
	w.set(worker.ConnFailed, we)
	return we
}

func (w *GatewayWorker) observersCopy() []worker.ConnectionObserver {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]worker.ConnectionObserver(nil), w.observers...)
}

// ReconcileSession checks a persisted session with the gateway (WBS
// 12.41/12.42): live → CONFIRMED and adopted as current (with the
// gateway-reported directory); missing → LOST; unreachable → UNKNOWN. No
// session is ever created here.
func (w *GatewayWorker) ReconcileSession(ctx context.Context, persisted worker.SessionInfo) (worker.SessionReconciliation, error) {
	w.mu.Lock()
	e := w.usableLocked()
	w.mu.Unlock()
	if e != nil {
		return worker.SessionReconciliation{Result: worker.ReconcileUnknown, Session: persisted, Err: e}, nil
	}
	gs, err := w.client.Get(ctx, persisted.ID)
	if err != nil {
		we := worker.AsWorkerError(err).Sanitize(w.secrets()...)
		if we.Code == worker.CodeSessionLost {
			w.dropSession(persisted.ID)
			return worker.SessionReconciliation{Result: worker.ReconcileLost, Session: persisted, Err: we}, nil
		}
		w.noteTransport(we)
		return worker.SessionReconciliation{Result: worker.ReconcileUnknown, Session: persisted, Err: we}, nil
	}
	s := sessionFrom(gs)
	if !persisted.CreatedAt.IsZero() {
		s.CreatedAt = persisted.CreatedAt
	}
	w.mu.Lock()
	w.session = &s
	w.mu.Unlock()
	return worker.SessionReconciliation{Result: worker.ReconcileConfirmed, Session: s}, nil
}

// Close shuts the adapter down (WBS 12.66): new requests are rejected,
// in-flight requests are cancelled (their outcome stays truthful), the
// gateway connection is closed and the state becomes CLOSED. The external
// session is not destroyed.
func (w *GatewayWorker) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.state == worker.ConnClosed || w.state == worker.ConnClosing {
		w.mu.Unlock()
		return nil
	}
	n := w.setLocked(worker.ConnClosing, nil)
	for _, cancel := range w.inflight {
		cancel()
	}
	w.mu.Unlock()
	n()
	drained := make(chan struct{})
	go func() { w.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-ctx.Done():
	}
	err := w.client.Close(ctx)
	w.set(worker.ConnClosed, nil)
	return err
}

// --- streaming -----------------------------------------------------------------

type stream struct {
	w      *GatewayWorker
	rd     *EventReader
	req    GatewayRequest
	sess   worker.SessionInfo
	done   func()
	ended  bool
	closed bool
}

// Stream sends the prompt with streaming (WBS 12.45). Only available when
// the gateway advertises streaming.
func (w *GatewayWorker) Stream(ctx context.Context, req worker.WorkerRequest) (worker.WorkerStream, error) {
	w.mu.Lock()
	streaming := w.caps != nil && w.caps.Streaming
	w.mu.Unlock()
	if !streaming {
		return nil, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "gateway does not support streaming", nil)
	}
	greq, err := Translate(req, w.cfg)
	if err != nil {
		return nil, err
	}
	sess, sctx, done, we := w.begin(ctx, req)
	if we != nil {
		return nil, we
	}
	key := worker.DispatchKey(req.Metadata.RuntimeStateID, req.WorkUnitID, req.Prompt.ID, req.Metadata.Attempt)
	rd, err := w.client.Stream(sctx, sess.ID, greq, key)
	if err != nil {
		e := w.sendFailure(sess.ID, greq.RequestID, err)
		done()
		return nil, e
	}
	return &stream{w: w, rd: rd, req: greq, sess: sess, done: done}, nil
}

// Next translates the next gateway event (WBS 12.46).
func (s *stream) Next(ctx context.Context) (worker.StreamEvent, error) {
	if s.ended {
		return worker.StreamEvent{}, worker.ErrStreamDone
	}
	name, ev, raw, err := s.rd.Next()
	if err != nil {
		s.finish()
		if errors.Is(err, io.EOF) {
			return worker.StreamEvent{}, worker.NewError(worker.CodeDeliveryUnknown, worker.DeliveryUnknown, "stream ended without completion", nil)
		}
		return worker.StreamEvent{}, err
	}
	out := worker.StreamEvent{Sequence: ev.Seq, At: time.Now().UTC()}
	switch name {
	case "started":
		out.Kind = worker.StreamStarted
	case "delta":
		out.Kind, out.Delta = worker.StreamContentDelta, ev.Delta
	case "tool":
		out.Kind = worker.StreamToolActivity
		if ev.Tool != nil {
			t := worker.ToolActivity{Name: ev.Tool.Name, State: worker.ToolState(toolState(ev.Tool.State)), StartedAt: ev.Tool.StartedAt, FinishedAt: ev.Tool.FinishedAt}
			if ev.Tool.DurationMS != nil {
				d := time.Duration(*ev.Tool.DurationMS) * time.Millisecond
				t.Duration = &d
			}
			out.Tool = &t
		}
	case "usage":
		u := usageFrom(ev.Usage)
		out.Kind, out.Usage = worker.StreamUsageUpdate, &u
	case "completed":
		s.finish()
		if ev.Response == nil {
			return worker.StreamEvent{}, protocolError("completion without response", raw, s.w.secrets()...)
		}
		r, err := Normalize(mustUUID(s.req.RequestID), s.sess.ID, *ev.Response, raw, s.w.secrets()...)
		if err != nil {
			return worker.StreamEvent{}, err
		}
		r.Session = s.sess
		out.Kind, out.Response = worker.StreamCompleted, &r
	case "error":
		s.finish()
		var ge GatewayError
		if ev.Error != nil {
			ge = *ev.Error
		}
		body, _ := jsonMarshal(ge)
		out.Kind, out.Err = worker.StreamError, NormalizeError(500, nil, body, s.w.secrets()...)
	default:
		return worker.StreamEvent{}, protocolError("unknown stream event "+name, raw, s.w.secrets()...)
	}
	return out, nil
}

func toolState(s string) string {
	switch s {
	case "started", "STARTED":
		return string(worker.ToolStarted)
	case "completed", "COMPLETED":
		return string(worker.ToolCompleted)
	}
	return string(worker.ToolFailed)
}

func (s *stream) finish() {
	if !s.ended {
		s.ended = true
		s.done()
	}
}

// Close releases the stream.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.finish()
	return s.rd.Close()
}

func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// sessionAdmin is the gateway client's optional Hermes session API.
type sessionAdmin interface {
	SessionStats(ctx context.Context, sessionID string) (HermesSessionStats, error)
	DeleteSession(ctx context.Context, sessionID string) error
}

// SessionStats reports a Hermes session's counters (worker.SessionManager).
func (w *GatewayWorker) SessionStats(ctx context.Context, sessionID string) (worker.SessionStats, error) {
	a, ok := w.client.(sessionAdmin)
	if !ok {
		return worker.SessionStats{}, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, "session stats not supported by this gateway client", nil)
	}
	s, err := a.SessionStats(ctx, sessionID)
	if err != nil {
		return worker.SessionStats{}, err
	}
	return worker.SessionStats{Messages: s.MessageCount, ToolCalls: s.ToolCallCount, ModelCalls: s.APICallCount,
		InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CacheReadTokens: s.CacheReadTokens}, nil
}

// DeleteSession deletes a Hermes session (worker.SessionManager).
func (w *GatewayWorker) DeleteSession(ctx context.Context, sessionID string) error {
	a, ok := w.client.(sessionAdmin)
	if !ok {
		return nil
	}
	return a.DeleteSession(ctx, sessionID)
}

var _ worker.SessionManager = (*GatewayWorker)(nil)
