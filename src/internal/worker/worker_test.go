package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/worker"
)

type stub struct{}

func (stub) Send(context.Context, worker.WorkerRequest) (worker.WorkerResponse, error) {
	return worker.WorkerResponse{}, nil
}
func (stub) NewSession(context.Context, worker.NewSessionRequest) (worker.SessionInfo, error) {
	return worker.SessionInfo{}, nil
}
func (stub) ChangeDirectory(context.Context, worker.ChangeDirectoryRequest) error { return nil }
func (stub) Session(context.Context) (worker.SessionInfo, error)                  { return worker.SessionInfo{}, nil }
func (stub) Capabilities(context.Context) (worker.WorkerCapabilities, error) {
	return worker.WorkerCapabilities{}, nil
}
func (stub) Close(context.Context) error { return nil }

// T12.1.3: a mock can satisfy the interface.
var _ worker.Worker = (*stub)(nil)

func prompt(content string) compression.WorkerPrompt {
	return compression.WorkerPrompt{ID: uuid.New(), Content: content, Hash: compression.WorkerPromptHash(content, compression.SourceCanonical), Source: compression.SourceCanonical}
}

func TestP1202RequestValidate(t *testing.T) {
	ok := worker.WorkerRequest{RequestID: uuid.New(), ProjectID: uuid.New(), Prompt: prompt("do it")}
	if err := ok.Validate(); err != nil {
		t.Fatalf("T12.9.1/T12.9.4 valid request without phase/unit: %v", err)
	}
	ph, wu := uuid.New(), uuid.New()
	ok.PhaseID, ok.WorkUnitID = &ph, &wu
	if ok.Validate() != nil {
		t.Fatal("T12.9.4 optional phase/unit")
	}
	bad := ok
	bad.RequestID = uuid.Nil
	if !errors.Is(bad.Validate(), worker.ErrRequestIDRequired) {
		t.Fatal("T12.9.2")
	}
	bad = ok
	bad.Prompt = compression.WorkerPrompt{}
	if !errors.Is(bad.Validate(), worker.ErrPromptRequired) {
		t.Fatal("T12.9.3")
	}
}

func TestP1203Utilization(t *testing.T) {
	u := worker.WorkerUsage{ContextTokens: worker.Int64(32000), ContextWindow: worker.Int64(64000)}
	got, err := u.Utilization()
	if err != nil || !got.Known || got.Percent != 50 {
		t.Fatalf("T12.14.1 %+v %v", got, err)
	}
	if _, err := (worker.WorkerUsage{ContextTokens: worker.Int64(1), ContextWindow: worker.Int64(0)}).Utilization(); !errors.Is(err, worker.ErrInvalidContextWindow) {
		t.Fatal("T12.14.2")
	}
	if got, _ := (worker.WorkerUsage{}).Utilization(); got.Known || got.Percent != 0 {
		t.Fatal("T12.14.3 missing telemetry must be unknown")
	}
	over, _ := (worker.WorkerUsage{ContextTokens: worker.Int64(70000), ContextWindow: worker.Int64(64000)}).Utilization()
	if !over.OverLimit || over.Percent <= 100 {
		t.Fatalf("T12.14.4 %+v", over)
	}
	if (worker.WorkerUsage{InputTokens: worker.Int64(1)}).Estimated {
		t.Fatal("T12.13.1")
	}
}

func TestP1204SelectUsage(t *testing.T) {
	actual := &worker.WorkerUsage{ContextTokens: worker.Int64(48000), ContextWindow: worker.Int64(64000)}
	prov := &worker.WorkerUsage{InputTokens: worker.Int64(100), OutputTokens: worker.Int64(50)}
	est := &worker.WorkerUsage{ContextTokens: worker.Int64(900)}
	if u, s := worker.SelectUsage(actual, prov, est); s != worker.SourceProviderActual || *u.ContextTokens != 48000 || u.Estimated {
		t.Fatal("T12.15.1")
	}
	if u, s := worker.SelectUsage(nil, prov, est); s != worker.SourceProviderDerived || *u.ContextTokens != 150 || u.Estimated {
		t.Fatal("provider-derived")
	}
	if u, s := worker.SelectUsage(nil, nil, est); s != worker.SourceEstimated || !u.Estimated {
		t.Fatal("T12.15.2/T12.15.3")
	}
	if _, s := worker.SelectUsage(nil, nil, nil); s != worker.SourceUnknown {
		t.Fatal("unknown")
	}
}

func TestP1205SessionInfo(t *testing.T) {
	id := "  Sess-ABC/ü "
	s := worker.SessionInfo{ID: id}
	if s.Validate(true) != nil || s.ID != id {
		t.Fatal("T12.16.1")
	}
	if !errors.Is((worker.SessionInfo{}).Validate(true), worker.ErrSessionIDRequired) {
		t.Fatal("T12.16.2")
	}
	if (worker.SessionInfo{ID: "x"}).Validate(true) != nil || (worker.SessionInfo{}).Validate(false) != nil {
		t.Fatal("T12.16.3")
	}
}

func TestP1206Capabilities(t *testing.T) {
	var c worker.WorkerCapabilities
	if c.Streaming || c.NewSession || c.ProviderVersion != "" {
		t.Fatal("T12.7.2/T12.8.3")
	}
}

func TestP1207Config(t *testing.T) {
	c := worker.WorkerConfig{Provider: worker.ProviderHermesGateway, Model: "m", RequestTimeout: time.Minute, ContextWindow: 64000}
	if c.Validate() != nil {
		t.Fatal("T12.3.1")
	}
	c.RequestTimeout = 0
	if c.Validate() == nil {
		t.Fatal("T12.3.3")
	}
	c = worker.WorkerConfig{Provider: "NOPE", ContextWindow: -1, ProviderConfig: json.RawMessage("{")}
	var ce *worker.ConfigError
	if !errors.As(c.Validate(), &ce) || len(ce.Problems) != 4 {
		t.Fatalf("all problems: %v", c.Validate())
	}
}

func TestP1208Connection(t *testing.T) {
	var s worker.ConnectionState = worker.ConnDisconnected
	for _, to := range []worker.ConnectionState{worker.ConnConnecting, worker.ConnConnected, worker.ConnReconnecting, worker.ConnConnected, worker.ConnClosing, worker.ConnClosed} {
		var err error
		if s, err = s.Transition(to); err != nil {
			t.Fatalf("T12.6.2/T12.6.3 %v", err)
		}
	}
	if _, err := worker.ConnClosed.Transition(worker.ConnConnected); !errors.Is(err, worker.ErrIllegalConnectionTransition) {
		t.Fatal("T12.6.4")
	}
	if _, err := worker.ConnDisconnected.Transition(worker.ConnConnected); err == nil {
		t.Fatal("T12.6.4 skip CONNECTING")
	}
	if worker.ConnFailed.Healthy() {
		t.Fatal("T12.5.4")
	}
}

func TestP1209RetryClassification(t *testing.T) {
	if worker.ClassifyRetry(worker.DeliveryNotSent, worker.CodeUnavailable) != worker.RetrySafe {
		t.Fatal("T12.35.1")
	}
	if worker.ClassifyRetry(worker.DeliveryUnknown, worker.CodeUnavailable) != worker.RetryRecovery {
		t.Fatal("T12.35.2")
	}
	if worker.ClassifyRetry(worker.DeliveryReceived, worker.CodeContextLimit) != worker.RetryUnsafe {
		t.Fatal("received")
	}
	e := worker.NewError(worker.CodeUnavailable, worker.DeliveryUnknown, "x", nil)
	if e.Retryable || e.RetryClass() != worker.RetryRecovery {
		t.Fatal("T12.35.3")
	}
}

func TestP1210WorkerError(t *testing.T) {
	pc := "auth_failed: key sk-SECRET"
	ra := 3 * time.Second
	e := &worker.WorkerError{Code: worker.CodeAuthFailed, Message: "bad key sk-SECRET", ProviderCode: &pc, Diagnostic: "Bearer sk-SECRET",
		RetryAfter: &ra, Cause: errors.New("sk-SECRET rejected")}
	if !errors.Is(e, &worker.WorkerError{Code: worker.CodeAuthFailed}) {
		t.Fatal("T12.39.1")
	}
	s := e.Sanitize("sk-SECRET")
	for _, v := range []string{s.Message, s.Diagnostic, *s.ProviderCode, s.Cause.Error(), s.Error()} {
		if strings.Contains(v, "sk-SECRET") {
			t.Fatalf("T12.39.3 secret leaked: %q", v)
		}
	}
	if *s.RetryAfter != ra || s.ProviderCode == nil {
		t.Fatal("T12.39.2/T12.59.2")
	}
	if w := worker.AsWorkerError(errors.New("x")); w.Code != worker.CodeInternal || w.Delivery != worker.DeliveryUnknown {
		t.Fatal("unclassified")
	}
}

func TestP1211SessionStatus(t *testing.T) {
	if worker.CheckSessionTransition(worker.SessionActive, worker.SessionReset) != nil {
		t.Fatal("active→reset")
	}
	if worker.CheckSessionTransition(worker.SessionReset, worker.SessionActive) == nil || worker.CheckSessionTransition(worker.SessionLost, worker.SessionClosed) == nil {
		t.Fatal("terminal reopened")
	}
	if worker.DefaultSessionPolicy != worker.PolicySingleActive || !worker.PolicyMultipleActive.Valid() {
		t.Fatal("T12.22.2")
	}
}

func TestP1212DispatchModel(t *testing.T) {
	path := []worker.DispatchStatus{worker.DispatchPending, worker.DispatchSending, worker.DispatchWaiting, worker.DispatchCompleted}
	for i := 1; i < len(path); i++ {
		if err := worker.CheckDispatchTransition(path[i-1], path[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, term := range []worker.DispatchStatus{worker.DispatchCompleted, worker.DispatchFailed, worker.DispatchUnknown, worker.DispatchCancelled} {
		if worker.CheckDispatchTransition(term, worker.DispatchPending) == nil || worker.CheckDispatchTransition(term, worker.DispatchCompleted) == nil {
			t.Fatal("terminal reopened")
		}
	}
	if worker.CheckDispatchTransition(worker.DispatchPending, worker.DispatchUnknown) == nil {
		t.Fatal("PENDING (never sent) cannot be UNKNOWN")
	}
	if worker.NextAttempt(nil) != 1 || worker.NextAttempt([]int{1, 3, 2}) != 4 {
		t.Fatal("T12.58.1")
	}
	unk := worker.NewError(worker.CodeUnavailable, worker.DeliveryUnknown, "", nil)
	ns := worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "", nil)
	cn := worker.NewError(worker.CodeCancelled, worker.DeliveryNotSent, "", nil)
	if worker.OutcomeFor(unk) != worker.DispatchUnknown || worker.OutcomeFor(ns) != worker.DispatchFailed || worker.OutcomeFor(cn) != worker.DispatchCancelled {
		t.Fatal("T12.30.3 outcome mapping")
	}
}

func TestP1213DispatchKey(t *testing.T) {
	rt, wp, wu := uuid.New(), uuid.New(), uuid.New()
	if worker.DispatchKey(rt, &wu, wp, 1) != worker.DispatchKey(rt, &wu, wp, 1) {
		t.Fatal("T12.33.1 deterministic")
	}
	if worker.DispatchKey(rt, &wu, wp, 1) == worker.DispatchKey(rt, &wu, wp, 2) {
		t.Fatal("T12.33.2 new attempt distinct")
	}
	if worker.LogicalOperationID(rt, &wu, wp) != worker.LogicalOperationID(rt, &wu, wp) || worker.LogicalOperationID(rt, nil, wp) == worker.LogicalOperationID(rt, &wu, wp) {
		t.Fatal("T12.58.3")
	}
}

func TestP1214ContentHash(t *testing.T) {
	if worker.ContentHash("a\r\nb ") != worker.ContentHash("a\r\nb ") || worker.ContentHash("a") == worker.ContentHash("a ") {
		t.Fatal("T12.31.2")
	}
}

func TestP1215PromptIntegrity(t *testing.T) {
	p := prompt("exact content")
	orig := p
	if worker.VerifyPromptIntegrity(p) != nil {
		t.Fatal("T12.29.1")
	}
	p.Content += "!"
	if !errors.Is(worker.VerifyPromptIntegrity(p), worker.ErrPromptIntegrity) {
		t.Fatal("T12.29.2")
	}
	if orig.Content != "exact content" {
		t.Fatal("T12.29.3")
	}
}

func TestP1216Readiness(t *testing.T) {
	active, lost := worker.SessionActive, worker.SessionLost
	cwd, other := "/repos/a", "/repos/b"
	if !worker.EvaluateReadiness(true, &active, &cwd, "/repos/a").Ready() {
		t.Fatal("T12.27.1")
	}
	if worker.EvaluateReadiness(true, &active, &other, "/repos/a").Ready() {
		t.Fatal("T12.27.2")
	}
	if worker.EvaluateReadiness(true, &lost, &cwd, "/repos/a").Ready() {
		t.Fatal("T12.27.3")
	}
	if r := worker.EvaluateReadiness(true, &active, nil, "/repos/a"); r.CWDInitialized || r.Ready() {
		t.Fatal("T12.26.3")
	}
}

func TestP1217DirectoryValidation(t *testing.T) {
	if !errors.Is(worker.ValidateDirectoryRequest(" ", nil, nil), worker.ErrDirectoryEmpty) {
		t.Fatal("T12.25.1")
	}
	if worker.ValidateDirectoryRequest("/repos/a", []string{"/repos"}, func(string) bool { return true }) != nil {
		t.Fatal("T12.25.2")
	}
	for _, c := range []struct {
		p    string
		want error
	}{{"repos/a", worker.ErrDirectoryRelative}, {"/etc", worker.ErrDirectoryDisallowed}, {"/reposx", worker.ErrDirectoryDisallowed}} {
		if err := worker.ValidateDirectoryRequest(c.p, []string{"/repos"}, nil); !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.p) {
			t.Fatalf("T12.25.3 %s: %v", c.p, err)
		}
	}
	if !errors.Is(worker.ValidateDirectoryRequest("/repos/x", nil, func(string) bool { return false }), worker.ErrDirectoryInvalid) {
		t.Fatal("local check")
	}
}

func TestP1218StreamAssembler(t *testing.T) {
	var a worker.StreamAssembler
	evs := []worker.StreamEvent{{Kind: worker.StreamStarted, Sequence: 1}, {Kind: worker.StreamContentDelta, Sequence: 2, Delta: "hel"},
		{Kind: worker.StreamToolActivity, Sequence: 3, Tool: &worker.ToolActivity{Name: "bash", State: worker.ToolStarted}},
		{Kind: worker.StreamContentDelta, Sequence: 4, Delta: "lo"}, {Kind: worker.StreamCompleted, Sequence: 5}}
	for _, e := range evs {
		if err := a.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Add(worker.StreamEvent{Kind: worker.StreamCompleted, Sequence: 6}); !errors.Is(err, worker.ErrStreamDoubleComplete) {
		t.Fatal("T12.46.2")
	}
	r, tools, err := a.Result()
	if err != nil || r.Content != "hello" || len(tools) != 1 {
		t.Fatalf("T12.46.1 %q %v", r.Content, err)
	}
	var b worker.StreamAssembler
	b.Add(worker.StreamEvent{Kind: worker.StreamContentDelta, Sequence: 2})
	if !errors.Is(b.Add(worker.StreamEvent{Kind: worker.StreamContentDelta, Sequence: 1}), worker.ErrStreamOutOfOrder) {
		t.Fatal("ordering")
	}
	if _, _, err := b.Result(); !errors.Is(err, worker.ErrStreamIncomplete) {
		t.Fatal("incomplete")
	}
}

func TestP1220Health(t *testing.T) {
	lost := worker.SessionLost
	h := worker.BuildHealth(worker.ProviderHermesGateway, worker.ConnConnected, &lost, nil,
		&worker.WorkerErrorSummary{Code: worker.CodeSessionLost}, false)
	if !h.Liveness.TransportAlive || h.Liveness.SessionAlive || *h.SessionState != worker.SessionLost || h.LastError.Code != worker.CodeSessionLost {
		t.Fatalf("T12.49.1/T12.49.2/T12.48.3 %+v", h)
	}
}

func TestP1221ContextProgress(t *testing.T) {
	cp := worker.BuildContextProgress(worker.WorkerUsage{ContextTokens: worker.Int64(32840), ContextWindow: worker.Int64(64000)}, worker.SourceProviderActual, "Hermes")
	if !cp.Known || cp.Percent != 51.31 || cp.Label != "Hermes" || cp.Estimated {
		t.Fatalf("T12.51.1 %+v", cp)
	}
	est := worker.BuildContextProgress(worker.WorkerUsage{ContextTokens: worker.Int64(1000), ContextWindow: worker.Int64(64000), Estimated: true}, worker.SourceEstimated, "Hermes")
	if est.Label != "estimated" || !est.Estimated {
		t.Fatal("T12.51.2")
	}
	if u := worker.BuildContextProgress(worker.WorkerUsage{}, worker.SourceUnknown, "Hermes"); u.Known || u.Label != "unknown" {
		t.Fatal("T12.51.3")
	}
}

func TestP1222Metrics(t *testing.T) {
	now := time.Now()
	later := now.Add(2 * time.Second)
	m := worker.AggregateMetrics(worker.MetricsInput{
		Dispatches: []worker.WorkerDispatch{{Status: worker.DispatchCompleted, StartedAt: &now, CompletedAt: &later}, {Status: worker.DispatchFailed},
			{Status: worker.DispatchUnknown}, {Status: worker.DispatchCancelled}},
		Events:      []worker.EventType{worker.EventSessionCreated, worker.EventSessionCreated, worker.EventSessionReset, worker.EventSessionLost, worker.EventReconnected, worker.EventConnectionFailed},
		Utilization: []float64{50, 75},
	})
	if m.Requests != 4 || m.Successful != 1 || m.Failed != 1 || m.DeliveryUnknown != 1 || m.Cancelled != 1 {
		t.Fatalf("T12.54.1-T12.54.3 %+v", m)
	}
	if m.SessionResets != 1 || m.SessionsCreated != 2 || m.SessionsLost != 1 || m.Reconnections != 1 || m.ConnectionFailures != 1 {
		t.Fatalf("T12.54.4 %+v", m)
	}
	if m.AvgDuration != 2*time.Second || m.AvgUtilization != 62.5 || m.PeakUtilization != 75 {
		t.Fatalf("duration/utilization %+v", m)
	}
}

func TestP1223ConfigurationValidation(t *testing.T) {
	c := worker.WorkerConfig{Provider: worker.ProviderHermesGateway, RequestTimeout: 0, ContextWindow: 0}
	caps := &worker.WorkerCapabilities{}
	err := worker.ValidateConfiguration(c, caps, worker.WorkflowRequirements{NewSession: true, ChangeDirectory: true}, errors.New("endpoint invalid"), false)
	var ce *worker.ConfigError
	if !errors.As(err, &ce) || len(ce.Problems) != 7 {
		t.Fatalf("T12.96.1 %v", err)
	}
	if !strings.Contains(err.Error(), "new session") || !strings.Contains(err.Error(), "change working directory") {
		t.Fatal("T12.96.2/T12.96.3")
	}
	ok := worker.WorkerConfig{Provider: worker.ProviderLocalAgent, Model: "m", RequestTimeout: time.Second, ContextWindow: 1}
	if worker.ValidateConfiguration(ok, &worker.WorkerCapabilities{}, worker.WorkflowRequirements{}, nil, true) != nil {
		t.Fatal("T12.97.2 worker without CWD capability can exist")
	}
}

func TestP1224Factory(t *testing.T) {
	reg := worker.NewProviderRegistry()
	called := false
	reg.Register(worker.ProviderLocalAgent, func(context.Context, worker.WorkerConfig) (worker.Worker, error) {
		called = true
		return stub{}, nil
	})
	cfg := worker.WorkerConfig{Provider: worker.ProviderLocalAgent, Model: "m", RequestTimeout: time.Second, ContextWindow: 1}
	w, err := reg.Create(context.Background(), cfg)
	if err != nil || w == nil || !called {
		t.Fatal("T12.2.3")
	}
	cfg.Provider = worker.ProviderClaudeCode
	if _, err := reg.Create(context.Background(), cfg); !errors.Is(err, worker.ErrUnknownProvider) {
		t.Fatal("T12.2.2")
	}
	called = false
	bad := worker.WorkerConfig{Provider: worker.ProviderLocalAgent}
	if _, err := reg.Create(context.Background(), bad); err == nil || called {
		t.Fatal("T12.2.4 invalid config must fail before construction")
	}
}

func TestP1240ReconnectBackoff(t *testing.T) {
	p := worker.ReconnectPolicy{MaxAttempts: 5, InitialDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	want := []time.Duration{100, 200, 400, 800, 1000}
	for i, w := range want {
		if p.Delay(i+1) != w*time.Millisecond {
			t.Fatalf("T12.40.2 attempt %d: %v", i+1, p.Delay(i+1))
		}
	}
	if (worker.ReconnectPolicy{InitialDelay: 2 * time.Second, MaxDelay: time.Second}).Validate() == nil {
		t.Fatal("invalid policy")
	}
}
