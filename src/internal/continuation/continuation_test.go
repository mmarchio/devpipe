package continuation_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	c "overseer/internal/continuation"
	"overseer/internal/project"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

func i64(v int64) *int64 { return &v }

func reading(t *testing.T, tokens, window int64, src worker.UsageSource) c.ContextReading {
	t.Helper()
	r, err := c.ReadingFromUsage(worker.WorkerUsage{ContextTokens: i64(tokens), ContextWindow: i64(window)}, src)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestP1401Status(t *testing.T) {
	for _, s := range c.AllStatuses() {
		got, err := c.ParseStatus(string(s))
		if err != nil || got != s {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, bad := range []string{"", "pending", "UNKNOWN"} {
		if _, err := c.ParseStatus(bad); !errors.Is(err, c.ErrInvalidStatus) {
			t.Fatalf("%q accepted", bad)
		}
	}
	terminal := map[c.Status]bool{c.StatusCompleted: true, c.StatusBlocked: true, c.StatusFailed: true, c.StatusCancelled: true, c.StatusRecoveryRequired: true}
	for _, s := range c.AllStatuses() {
		if s.IsTerminal() != terminal[s] {
			t.Fatalf("%s terminal=%v", s, s.IsTerminal())
		}
	}
	if len(c.AllStatuses()) != 15 {
		t.Fatal("15 statuses expected")
	}
}

func TestP1402Transitions(t *testing.T) {
	allowed := [][2]c.Status{
		{c.StatusPending, c.StatusDeciding},
		{c.StatusDeciding, c.StatusSameSession}, {c.StatusDeciding, c.StatusResetRequired}, {c.StatusDeciding, c.StatusRecoveryRequired},
		{c.StatusSameSession, c.StatusRestoringCWD}, {c.StatusSameSession, c.StatusResetRequired},
		{c.StatusResetRequired, c.StatusCreatingSession},
		{c.StatusCreatingSession, c.StatusRestoringContext}, {c.StatusCreatingSession, c.StatusRecoveryRequired},
		{c.StatusRestoringContext, c.StatusRestoringCWD},
		{c.StatusRestoringCWD, c.StatusReady}, {c.StatusRestoringCWD, c.StatusRecoveryRequired},
		{c.StatusReady, c.StatusDispatching}, {c.StatusReady, c.StatusRestoringCWD},
		{c.StatusDispatching, c.StatusWaiting}, {c.StatusDispatching, c.StatusRecoveryRequired},
		{c.StatusWaiting, c.StatusCompleted}, {c.StatusWaiting, c.StatusBlocked}, {c.StatusWaiting, c.StatusRecoveryRequired},
		{c.StatusWaiting, c.StatusFailed}, {c.StatusPending, c.StatusCancelled}, {c.StatusReady, c.StatusCancelled},
	}
	for _, e := range allowed {
		if err := c.ValidateTransition(e[0], e[1]); err != nil {
			t.Fatalf("%v rejected: %v", e, err)
		}
	}
	for _, s := range c.AllStatuses() {
		if !s.IsTerminal() && (!c.CanTransition(s, c.StatusFailed) || !c.CanTransition(s, c.StatusCancelled)) {
			t.Fatalf("%s cannot fail/cancel", s)
		}
	}
	forbidden := [][2]c.Status{
		{c.StatusCompleted, c.StatusDispatching}, {c.StatusFailed, c.StatusReady}, {c.StatusCancelled, c.StatusWaiting},
		{c.StatusPending, c.StatusDispatching}, {c.StatusSameSession, c.StatusReady}, {c.StatusResetRequired, c.StatusReady},
		{c.StatusCreatingSession, c.StatusReady}, {c.StatusRestoringContext, c.StatusReady}, {c.StatusWaiting, c.StatusDispatching},
		{c.StatusRecoveryRequired, c.StatusReady}, {c.StatusBlocked, c.StatusPending}, {c.StatusRecoveryRequired, c.StatusCancelled},
	}
	for _, e := range forbidden {
		if err := c.ValidateTransition(e[0], e[1]); !errors.Is(err, c.ErrIllegalTransition) {
			t.Fatalf("%v allowed", e)
		}
	}
	for _, e := range c.Edges() {
		if e[0].IsTerminal() {
			t.Fatalf("terminal edge %v", e)
		}
	}
}

func TestP1403Enums(t *testing.T) {
	for _, s := range []string{"same_session", "fresh_session"} {
		if _, err := c.ParseStrategy(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ParseStrategy("new_session"); !errors.Is(err, c.ErrInvalidStrategy) {
		t.Fatal("unknown strategy accepted")
	}
	for _, r := range []string{"below_threshold", "at_threshold", "above_threshold", "context_unknown", "context_limit", "session_lost", "operator_override"} {
		if _, err := c.ParseDecisionReason(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ParseDecisionReason("worker_asked"); err == nil {
		t.Fatal("unknown reason accepted")
	}
}

func TestP1404Enums(t *testing.T) {
	for _, s := range []string{"RESULT_INCOMPLETE", "CONTEXT_LIMIT", "SESSION_LOST", "MANUAL_RETRY"} {
		if _, err := c.ParseTrigger(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ParseTrigger("x"); err == nil {
		t.Fatal("bad trigger")
	}
	for _, s := range []string{"DELIVERY_UNKNOWN", "NO_PROGRESS", "CONTINUATION_LIMIT", "RESET_LIMIT", "SESSION_CREATE_FAILED", "CWD_RESTORE_FAILED", "BINDING_CHANGED", "DISPATCH_REJECTED"} {
		if _, err := c.ParseRecoveryReason(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ParseRecoveryReason(""); err == nil {
		t.Fatal("empty recovery reason")
	}
	if !c.TriggerResultIncomplete.RequiresResult() || c.TriggerContextLimit.RequiresResult() {
		t.Fatal("RequiresResult")
	}
}

func TestP1405Settings(t *testing.T) {
	d := c.DefaultSettings()
	if d.UnknownContextPolicy != c.ContinueFreshSession || !d.EstimatedMayReset || d.MaxContinuationsPerWorkUnit != 8 ||
		d.MaxSessionResetsPerWorkUnit != 4 || d.MaxNoProgressContinuations != 3 || d.Validate() != nil {
		t.Fatalf("defaults %+v", d)
	}
	bad := c.Settings{UnknownContextPolicy: "x", MaxContinuationsPerWorkUnit: 0, MaxSessionResetsPerWorkUnit: -2,
		MaxNoProgressContinuations: 0, SessionCreateRetries: -1, LeaseDuration: 0}
	err := bad.Validate()
	if !errors.Is(err, c.ErrInvalidSettings) || strings.Count(err.Error(), "\n") != 5 {
		t.Fatalf("all problems expected: %v", err)
	}
	for _, th := range []float64{0.8, 1} {
		if !c.ValidThreshold(th) {
			t.Fatal(th)
		}
	}
	for _, th := range []float64{0, -0.1, 1.01} {
		if c.ValidThreshold(th) {
			t.Fatal(th)
		}
	}
}

func TestP1406Reading(t *testing.T) {
	if r := reading(t, 32000, 64000, worker.SourceProviderActual); *r.Utilization != 0.5 || !r.Known || r.Estimated {
		t.Fatalf("T14.6.1 %+v", r)
	}
	if r := reading(t, 51200, 64000, worker.SourceProviderActual); *r.Utilization != 0.8 {
		t.Fatalf("T14.6.2 %v", *r.Utilization)
	}
	if _, err := c.ReadingFromUsage(worker.WorkerUsage{ContextTokens: i64(1), ContextWindow: i64(0)}, worker.SourceProviderActual); !errors.Is(err, c.ErrInvalidWindow) {
		t.Fatal("T14.6.4 invalid window accepted")
	}
	if r, _ := c.ReadingFromUsage(worker.WorkerUsage{ContextWindow: i64(64000)}, worker.SourceProviderActual); r.Known || r.Utilization != nil {
		t.Fatal("missing tokens must be unknown")
	}
	if r := reading(t, 100, 1000, worker.SourceEstimated); !r.Estimated {
		t.Fatal("estimate flag")
	}
}

func TestP1407SelectReading(t *testing.T) {
	now := time.Now()
	obs := []c.Observation{
		{Usage: worker.WorkerUsage{ContextTokens: i64(60000), ContextWindow: i64(64000), Estimated: true}, Source: worker.SourceEstimated, Seq: 5, ObservedAt: now},
		{Usage: worker.WorkerUsage{ContextTokens: i64(20000), ContextWindow: i64(64000)}, Source: worker.SourceProviderActual, Seq: 1, ObservedAt: now},
		{Usage: worker.WorkerUsage{ContextTokens: i64(30000), ContextWindow: i64(64000)}, Source: worker.SourceProviderDerived, Seq: 3, ObservedAt: now},
	}
	if r := c.SelectReading(obs); *r.Tokens != 20000 || r.Source != worker.SourceProviderActual {
		t.Fatalf("actual must win: %+v", r)
	}
	obs = append(obs, c.Observation{Usage: worker.WorkerUsage{ContextTokens: i64(25000), ContextWindow: i64(64000)}, Source: worker.SourceProviderActual, Seq: 2, ObservedAt: now})
	if r := c.SelectReading(obs); *r.Tokens != 25000 {
		t.Fatal("latest actual wins")
	}
	if r := c.SelectReading(nil); r.Known || r.Source != worker.SourceUnknown {
		t.Fatal("none → unknown")
	}
}

func src(p, ph, wu uuid.UUID) c.Source {
	return c.Source{ID: uuid.New(), ProjectID: p, PhaseID: &ph, WorkUnitID: &wu}
}

func newInput() c.NewInput {
	p, ph, wu, rt := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	res := src(p, ph, wu)
	return c.NewInput{ProjectID: p, PhaseID: ph, WorkUnitID: wu, RuntimeStateID: rt, RuntimeStateVersion: 3,
		Trigger: c.TriggerResultIncomplete, Attempt: c.FirstAttempt, SourceDispatch: src(p, ph, wu), SourceResult: &res,
		Captured: c.CapturedStatus{Completed: []string{"A"}, Remaining: []string{"B"}}}
}

func TestP1408Entity(t *testing.T) {
	in := newInput()
	k, err := c.NewContinuation(in)
	if err != nil || k.Status != c.StatusPending || k.Attempt != 1 || k.Version != 0 || k.StatusHash == "" || k.SourceResultID == nil {
		t.Fatalf("T14.1.1 %+v %v", k, err)
	}
	for name, mut := range map[string]func(*c.NewInput){
		"project": func(i *c.NewInput) { i.ProjectID = uuid.Nil },
		"phase":   func(i *c.NewInput) { i.PhaseID = uuid.Nil },
		"unit":    func(i *c.NewInput) { i.WorkUnitID = uuid.Nil },
		"runtime": func(i *c.NewInput) { i.RuntimeStateID = uuid.Nil },
	} {
		i := newInput()
		mut(&i)
		if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrMissingBinding) {
			t.Fatalf("T14.1.2 %s: %v", name, err)
		}
	}
	i := newInput()
	i.SourceResult = nil
	if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrMissingSource) {
		t.Fatal("T14.1.3 source result required")
	}
	i.Trigger = c.TriggerContextLimit
	if _, err := c.NewContinuation(i); err != nil {
		t.Fatalf("context-limit without result: %v", err)
	}
	i = newInput()
	i.Attempt = 0
	if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrInvalidAttempt) {
		t.Fatal("T14.1.4")
	}
	if c.NextAttempt(0) != 1 || c.NextAttempt(2) != 3 {
		t.Fatal("T14.1.4 next attempt")
	}
	i = newInput()
	other := src(uuid.New(), i.PhaseID, i.WorkUnitID)
	i.SourceResult = &other
	if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrCrossBinding) {
		t.Fatal("T14.1.5 cross-project source")
	}
	i = newInput()
	i.SourceDispatch.WorkUnitID = ptr(uuid.New())
	if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrCrossBinding) {
		t.Fatal("14.67 other unit")
	}
	i = newInput()
	i.SourceDispatch.RuntimeStateID = uuid.New()
	if _, err := c.NewContinuation(i); !errors.Is(err, c.ErrCrossBinding) {
		t.Fatal("14.66 cross runtime")
	}
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }

func dctx(r c.ContextReading) c.DecisionContext {
	return c.DecisionContext{ResultStatus: workerresult.StatusIncomplete, Trigger: c.TriggerResultIncomplete, Reading: r,
		Threshold: 0.8, SessionStatus: worker.SessionActive, Settings: c.DefaultSettings()}
}

func decide(t *testing.T, dc c.DecisionContext) c.Decision {
	t.Helper()
	d, err := c.DefaultPolicy{}.Decide(dc)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestP1409Policy(t *testing.T) {
	p := c.DefaultPolicy{}
	dc := dctx(reading(t, 32000, 64000, worker.SourceProviderActual))
	d := decide(t, dc)
	if d.Strategy != c.ContinueSameSession || d.Reason != c.ReasonBelowThreshold || *d.Utilization != 0.5 {
		t.Fatalf("14.7 %+v", d)
	}
	if d2 := decide(t, dc); string(c.EncodeDecision(d2)) != string(c.EncodeDecision(d)) {
		t.Fatal("14.4 determinism")
	}
	cases := []struct {
		tokens int64
		want   c.Strategy
		reason c.DecisionReason
	}{{7999, c.ContinueSameSession, c.ReasonBelowThreshold}, {8000, c.ContinueFreshSession, c.ReasonAtThreshold},
		{8001, c.ContinueFreshSession, c.ReasonAboveThreshold}, {51200 * 10000 / 64000, c.ContinueFreshSession, c.ReasonAtThreshold}}
	for _, tc := range cases {
		d := decide(t, dctx(reading(t, tc.tokens, 10000, worker.SourceProviderActual)))
		if d.Strategy != tc.want || d.Reason != tc.reason {
			t.Fatalf("T14.8 %d: %+v", tc.tokens, d)
		}
	}
	if d := decide(t, dctx(reading(t, 51200, 64000, worker.SourceProviderActual))); d.Reason != c.ReasonAtThreshold {
		t.Fatal("T14.8.2 51,200/64,000")
	}
	if d := decide(t, dctx(reading(t, 58880, 64000, worker.SourceProviderActual))); d.Strategy != c.ContinueFreshSession || d.Reason != c.ReasonAboveThreshold {
		t.Fatal("14.9 92%")
	}
	for _, st := range []workerresult.WorkerResultStatus{workerresult.StatusComplete, workerresult.StatusBlocked, workerresult.StatusFailed} {
		dc := dctx(reading(t, 1, 10, worker.SourceProviderActual))
		dc.ResultStatus = st
		var nc *c.NoContinuationError
		if _, err := p.Decide(dc); !errors.As(err, &nc) || nc.Status != st {
			t.Fatalf("14.10 %s: %v", st, err)
		}
	}
	// 14.12 estimated
	if d := decide(t, dctx(reading(t, 7000, 10000, worker.SourceEstimated))); d.Strategy != c.ContinueSameSession || !d.Estimated {
		t.Fatal("T14.12.1/3")
	}
	if d := decide(t, dctx(reading(t, 8000, 10000, worker.SourceEstimated))); d.Strategy != c.ContinueFreshSession || !d.Estimated {
		t.Fatal("T14.12.2")
	}
	dc = dctx(reading(t, 9000, 10000, worker.SourceEstimated))
	dc.Settings.EstimatedMayReset = false
	dc.Settings.UnknownContextPolicy = c.ContinueSameSession
	if d := decide(t, dc); d.Reason != c.ReasonContextUnknown || d.Strategy != c.ContinueSameSession {
		t.Fatal("estimate not allowed → unknown policy")
	}
	// 14.13 unknown
	if d := decide(t, dctx(c.UnknownReading())); d.Strategy != c.ContinueFreshSession || d.Reason != c.ReasonContextUnknown || d.Utilization != nil {
		t.Fatal("T14.13.2/3")
	}
	dc = dctx(c.UnknownReading())
	dc.Settings.UnknownContextPolicy = c.ContinueSameSession
	if d := decide(t, dc); d.Strategy != c.ContinueSameSession {
		t.Fatal("T14.13.1")
	}
	// context limit / session lost
	dc = dctx(reading(t, 10, 100, worker.SourceProviderActual))
	dc.Trigger, dc.ResultStatus = c.TriggerContextLimit, ""
	if d := decide(t, dc); d.Reason != c.ReasonContextLimit || d.Strategy != c.ContinueFreshSession {
		t.Fatal("context limit")
	}
	dc = dctx(reading(t, 10, 100, worker.SourceProviderActual))
	dc.SessionStatus = worker.SessionLost
	if d := decide(t, dc); d.Reason != c.ReasonSessionLost || d.Strategy != c.ContinueFreshSession {
		t.Fatal("session lost")
	}
	// 14.121 / 14.122: the decision context has no worker-text input; 30% / 40% stay same session.
	if d := decide(t, dctx(reading(t, 30, 100, worker.SourceProviderActual))); d.Strategy != c.ContinueSameSession {
		t.Fatal("14.121")
	}
	if d := decide(t, dctx(reading(t, 40, 100, worker.SourceProviderActual))); d.Strategy != c.ContinueSameSession {
		t.Fatal("14.122")
	}
	dc = dctx(reading(t, 1, 10, worker.SourceProviderActual))
	dc.Threshold = 0
	if _, err := p.Decide(dc); !errors.Is(err, c.ErrInvalidThreshold) {
		t.Fatal("threshold")
	}
	// overrides
	dc = dctx(reading(t, 1, 10, worker.SourceProviderActual))
	dc.Override = c.ContinueFreshSession
	if d := decide(t, dc); d.Reason != c.ReasonOperatorOverride || d.Strategy != c.ContinueFreshSession {
		t.Fatal("override fresh")
	}
	dc = dctx(reading(t, 9, 10, worker.SourceProviderActual))
	dc.Override = c.ContinueSameSession
	if _, err := p.Decide(dc); !errors.Is(err, c.ErrUnsafeOverride) {
		t.Fatal("unsafe same override")
	}
}

func TestP1410DecisionCodec(t *testing.T) {
	for _, r := range []c.ContextReading{reading(t, 5, 10, worker.SourceEstimated), c.UnknownReading()} {
		d := decide(t, dctx(r))
		b := c.EncodeDecision(d)
		back, err := c.DecodeDecision(b)
		if err != nil || string(c.EncodeDecision(back)) != string(b) || back.Estimated != d.Estimated || (back.Utilization == nil) != (d.Utilization == nil) {
			t.Fatalf("14.15 round trip %s %v", b, err)
		}
	}
	if _, err := c.DecodeDecision([]byte(`{"strategy":"x","reason":"below_threshold"}`)); err == nil {
		t.Fatal("bad strategy decoded")
	}
}

func TestP1411Captured(t *testing.T) {
	src := workerresult.ContinuationStatus{Summary: "s", Completed: []string{"A", "B"}, Remaining: []string{"C"}, NextAction: "do C"}
	cs := c.Capture(src)
	src.Completed[0], src.Remaining[0] = "X", "Y"
	if cs.Completed[0] != "A" || cs.Remaining[0] != "C" || cs.NextAction != "do C" || len(cs.Completed) != 2 {
		t.Fatal("T14.16.1-4")
	}
	h := cs.Hash()
	if cs.Clone().Hash() != h {
		t.Fatal("14.17 same hash")
	}
	ch := cs.Clone()
	ch.Remaining = []string{"D"}
	if ch.Hash() == h {
		t.Fatal("14.17 changed remaining")
	}
	sum := cs.Clone()
	sum.Summary = "other words"
	if sum.Hash() == h || sum.ProgressKey() != cs.ProgressKey() {
		t.Fatal("progress key ignores summary")
	}
	back, err := c.DecodeCaptured(cs.Canonical())
	if err != nil || back.Hash() != h {
		t.Fatal("decode")
	}
	if (c.CapturedStatus{}).Hash() != (c.CapturedStatus{Completed: []string{}}).Hash() {
		t.Fatal("nil vs empty list")
	}
}

func TestP1412Progress(t *testing.T) {
	a := c.CapturedStatus{Completed: []string{"A"}, Remaining: []string{"B"}, NextAction: "Implement B"}
	same := a.Clone()
	same.Summary = "reworded"
	cmp := c.CompareProgress(&a, same)
	if !cmp.NoProgress || cmp.Progress {
		t.Fatal("14.42 summary-only is no progress")
	}
	streak := c.NextNoProgressStreak(1, c.CompareProgress(nil, a))
	streak = c.NextNoProgressStreak(streak, c.CompareProgress(&a, a))
	streak = c.NextNoProgressStreak(streak, c.CompareProgress(&a, a))
	if streak != 3 {
		t.Fatalf("14.41 three identical reports → 3, got %d", streak)
	}
	b := c.CapturedStatus{Completed: []string{"A", "B"}, Remaining: []string{"C"}}
	if cmp := c.CompareProgress(&a, b); !cmp.Progress || c.NextNoProgressStreak(5, cmp) != 1 {
		t.Fatal("14.111 progress resets")
	}
	grow := c.CapturedStatus{Completed: []string{"A"}, Remaining: []string{"B", "C", "D"}}
	if !c.CompareProgress(&a, grow).ScopeExpanded {
		t.Fatal("14.43 expansion flagged")
	}
}

func TestP1413Limits(t *testing.T) {
	s := c.DefaultSettings()
	if _, hit := c.CheckLimits(8, c.Counts{}, s, c.ContinueSameSession); hit {
		t.Fatal("T14.37.1 attempt 8 of 8 allowed")
	}
	if r, hit := c.CheckLimits(9, c.Counts{}, s, c.ContinueSameSession); !hit || r != c.RecoveryContinuationLimit {
		t.Fatal("T14.37.2")
	}
	if r, _ := c.CheckLimits(2, c.Counts{SuccessfulResets: 4}, s, c.ContinueFreshSession); r != c.RecoveryResetLimit {
		t.Fatal("14.38 reset limit")
	}
	if _, hit := c.CheckLimits(2, c.Counts{SuccessfulResets: 4}, s, c.ContinueSameSession); hit {
		t.Fatal("14.38 reset limit independent of same session")
	}
	if r, _ := c.CheckLimits(2, c.Counts{NoProgressStreak: 3}, s, c.ContinueSameSession); r != c.RecoveryNoProgress {
		t.Fatal("14.41")
	}
	if _, hit := c.CheckLimits(2, c.Counts{NoProgressStreak: 2}, s, c.ContinueSameSession); hit {
		t.Fatal("14.41 below")
	}
}

func TestUnlimitedLimits(t *testing.T) {
	s := c.DefaultSettings()
	s.MaxContinuationsPerWorkUnit, s.MaxSessionResetsPerWorkUnit = c.Unlimited, c.Unlimited
	if err := s.Validate(); err != nil {
		t.Fatalf("-1 must be valid: %v", err)
	}
	if _, hit := c.CheckLimits(1000, c.Counts{SuccessfulResets: 1000}, s, c.ContinueFreshSession); hit {
		t.Fatal("unlimited continuations / resets must never hit a limit")
	}
	if r, _ := c.CheckLimits(2, c.Counts{NoProgressStreak: 3}, s, c.ContinueFreshSession); r != c.RecoveryNoProgress {
		t.Fatal("no-progress limit still applies")
	}
	bad := c.DefaultSettings()
	bad.MaxContinuationsPerWorkUnit, bad.MaxSessionResetsPerWorkUnit = -2, -2
	if err := bad.Validate(); err == nil || strings.Count(err.Error(), "\n") != 1 {
		t.Fatalf("-2 must be rejected for both: %v", err)
	}
}

func TestP1414Outcome(t *testing.T) {
	want := map[workerresult.WorkerResultStatus]c.ConsumeOutcome{
		workerresult.StatusComplete:   {Target: c.StatusCompleted},
		workerresult.StatusIncomplete: {Target: c.StatusCompleted, CreateNext: true},
		workerresult.StatusBlocked:    {Target: c.StatusBlocked},
		workerresult.StatusFailed:     {Target: c.StatusFailed},
	}
	for st, w := range want {
		got, err := c.OutcomeFor(st)
		if err != nil || got != w {
			t.Fatalf("%s: %+v", st, got)
		}
	}
}

func TestP1415Failure(t *testing.T) {
	if f := c.ClassifyDispatchFailure(worker.CodeContextLimit, worker.DeliveryReceived); f.Kind != c.FailureFresh || f.Reason != c.ReasonContextLimit {
		t.Fatal("14.44")
	}
	if f := c.ClassifyDispatchFailure(worker.CodeSessionLost, worker.DeliveryNotSent); f.Kind != c.FailureFresh || f.Reason != c.ReasonSessionLost {
		t.Fatal("14.46")
	}
	if f := c.ClassifyDispatchFailure(worker.CodeSessionLost, worker.DeliveryReceived); f.Kind != c.FailureFresh {
		t.Fatal("14.46 gateway-rejected lost session")
	}
	for _, code := range []worker.WorkerErrorCode{worker.CodeDeliveryUnknown, worker.CodeTimeout, worker.CodeContextLimit, worker.CodeSessionLost} {
		if f := c.ClassifyDispatchFailure(code, worker.DeliveryUnknown); f.Kind != c.FailureRecovery || f.Recovery != c.RecoveryDeliveryUnknown {
			t.Fatalf("T14.47.1 %s", code)
		}
	}
	if f := c.ClassifyDispatchFailure(worker.CodeAuthFailed, worker.DeliveryNotSent); f.Kind != c.FailureTerminal {
		t.Fatal("terminal")
	}
	if f := c.ClassifyDispatchFailure(worker.CodeUnavailable, worker.DeliveryNotSent); f.Kind != c.FailureRetry {
		t.Fatal("retry")
	}
}

func TestP1416Readiness(t *testing.T) {
	ph, wu, rt, p := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cwd := "/repo/a"
	ok := c.ReadinessInput{ProjectStatus: project.StatusRUNNING, ContinuationPhaseID: ph, ContinuationWorkUnitID: wu,
		CurrentPhaseID: &ph, CurrentWorkUnitID: &wu, ContinuationRuntimeID: rt, CurrentRuntimeID: rt, RoleSnapshotValid: true,
		StatusCaptured: true, SessionProjectID: p, ProjectID: p, SessionStatus: worker.SessionActive, SessionCWD: &cwd,
		DesiredCWD: cwd, PromptReady: true, ProtocolValid: true}
	if err := c.ValidateReadiness(ok); err != nil {
		t.Fatal(err)
	}
	err := c.ValidateReadiness(c.ReadinessInput{LimitExceeded: c.RecoveryNoProgress, ProjectStatus: project.StatusPAUSED})
	var re *c.ReadinessError
	if !errors.As(err, &re) || len(re.Problems) != 10 || !errors.Is(err, c.ErrNotReady) || !re.Has(c.ProblemLimits) {
		t.Fatalf("14.126 all problems: %v", err)
	}
}

func TestP1417Override(t *testing.T) {
	k, _ := c.NewContinuation(newInput())
	k.Status = c.StatusReady
	if err := c.ValidateOverride(c.ContinueFreshSession, k, worker.SessionActive); err != nil {
		t.Fatal("14.85")
	}
	if err := c.ValidateOverride(c.ContinueSameSession, k, worker.SessionLost); !errors.Is(err, c.ErrUnsafeOverride) {
		t.Fatal("14.86 lost")
	}
	k.Decision = &c.Decision{Strategy: c.ContinueFreshSession, Reason: c.ReasonAboveThreshold}
	if err := c.ValidateOverride(c.ContinueSameSession, k, worker.SessionActive); !errors.Is(err, c.ErrUnsafeOverride) {
		t.Fatal("14.86 threshold")
	}
	k.Decision.Reason = c.ReasonContextUnknown
	if err := c.ValidateOverride(c.ContinueSameSession, k, worker.SessionActive); err != nil {
		t.Fatal("same override with unknown context")
	}
	k.Status = c.StatusWaiting
	if err := c.ValidateOverride(c.ContinueFreshSession, k, worker.SessionActive); !errors.Is(err, c.ErrOverrideNotAllowed) {
		t.Fatal("no override after dispatch")
	}
}

func TestP1418Retry(t *testing.T) {
	k, _ := c.NewContinuation(newInput())
	if err := c.CanRetry(k, true); !errors.Is(err, c.ErrRetryNotAllowed) {
		t.Fatal("pending retry")
	}
	k.Status, k.RecoveryReason = c.StatusRecoveryRequired, c.RecoveryDeliveryUnknown
	if err := c.CanRetry(k, false); !errors.Is(err, c.ErrReconciliationAck) {
		t.Fatal("14.84 blind retry")
	}
	if err := c.CanRetry(k, true); err != nil {
		t.Fatal(err)
	}
	k.RecoveryReason = c.RecoveryCWDRestoreFailed
	if err := c.CanRetry(k, false); err != nil {
		t.Fatal(err)
	}
}

func TestP1419Planner(t *testing.T) {
	intent := time.Now()
	old := uuid.New()
	if p := c.PlanRecovery(c.Facts{Status: c.StatusDeciding}); p.Action != c.ActionDecide {
		t.Fatal("14.94")
	}
	if p := c.PlanRecovery(c.Facts{Status: c.StatusReady}); p.Action != c.ActionCheckCWD {
		t.Fatal("14.95 READY re-checks CWD")
	}
	s2 := c.SessionFact{ID: uuid.New(), Status: worker.SessionActive, CreatedAt: intent.Add(time.Second)}
	if p := c.PlanRecovery(c.Facts{Status: c.StatusCreatingSession, IntentAt: intent, OldSessionID: &old, ActiveSession: &s2}); p.Action != c.ActionAdoptSession || *p.SessionID != s2.ID {
		t.Fatal("14.96 adopt created session")
	}
	s1 := c.SessionFact{ID: old, Status: worker.SessionActive, CreatedAt: intent.Add(-time.Hour)}
	if p := c.PlanRecovery(c.Facts{Status: c.StatusCreatingSession, IntentAt: intent, OldSessionID: &old, ActiveSession: &s1}); p.Action != c.ActionCreateSession {
		t.Fatal("14.96 none created → create")
	}
	if p := c.PlanRecovery(c.Facts{Status: c.StatusRestoringCWD}); p.Action != c.ActionCheckCWD {
		t.Fatal("14.97")
	}
}

func TestP1420Planner(t *testing.T) {
	if p := c.PlanRecovery(c.Facts{Status: c.StatusDispatching}); p.Action != c.ActionAdmitDispatch {
		t.Fatal("no row → admit")
	}
	unk := worker.DeliveryUnknown
	notSent := worker.DeliveryNotSent
	cases := []struct {
		d    c.DispatchFact
		want c.Action
	}{
		{c.DispatchFact{Status: worker.DispatchWaiting}, c.ActionWait},
		{c.DispatchFact{Status: worker.DispatchSending}, c.ActionWait},
		{c.DispatchFact{Status: worker.DispatchCompleted, Parsed: true}, c.ActionConsume},
		{c.DispatchFact{Status: worker.DispatchCompleted}, c.ActionParse},
		{c.DispatchFact{Status: worker.DispatchCompleted, Rejected: true}, c.ActionRecovery},
		{c.DispatchFact{Status: worker.DispatchUnknown, Delivery: &unk}, c.ActionRecovery},
		{c.DispatchFact{Status: worker.DispatchFailed, Delivery: &notSent, ErrorCode: string(worker.CodeSessionLost)}, c.ActionFresh},
		{c.DispatchFact{Status: worker.DispatchFailed, Delivery: &unk, ErrorCode: string(worker.CodeTimeout)}, c.ActionRecovery},
	}
	for _, st := range []c.Status{c.StatusDispatching, c.StatusWaiting} {
		for _, tc := range cases {
			d := tc.d
			if p := c.PlanRecovery(c.Facts{Status: st, Dispatch: &d}); p.Action != tc.want {
				t.Fatalf("%s %+v → %s want %s", st, tc.d, p.Action, tc.want)
			}
		}
	}
	if p := c.PlanRecovery(c.Facts{Status: c.StatusWaiting}); p.Action != c.ActionRecovery {
		t.Fatal("waiting without dispatch")
	}
}

func TestP1421View(t *testing.T) {
	k, _ := c.NewContinuation(newInput())
	k.Attempt, k.Strategy = 2, c.ContinueSameSession
	u := 34560.0 / 64000
	k.Decision = &c.Decision{Strategy: c.ContinueSameSession, Reason: c.ReasonBelowThreshold, ContextTokens: i64(34560), ContextWindow: i64(64000), Utilization: &u, Threshold: 0.8}
	v := c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings(), TaskLabel: "14.77 Implement continuation controller", TaskStatus: "EXECUTING"})
	if v.AttemptLabel != "Attempt 2 of 8" || v.Strategy != "Same Session" || v.Reason != "Context 54% < 80%" || v.Context.Label != "54%" ||
		v.Context.Tokens != "34,560 / 64,000" || len([]rune(v.Context.Bar)) != 25 || v.Reset != nil {
		t.Fatalf("14.77 %+v", v)
	}
	u2 := 0.86
	k.Attempt, k.Strategy = 3, c.ContinueFreshSession
	k.Decision = &c.Decision{Strategy: c.ContinueFreshSession, Reason: c.ReasonAboveThreshold, ContextTokens: i64(55040), ContextWindow: i64(64000), Utilization: &u2, Threshold: 0.8}
	old, nw := uuid.New(), uuid.New()
	k.OldSessionID, k.NewSessionID = &old, &nw
	v = c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings(), OldSessionSt: worker.SessionReset, NewSessionSt: worker.SessionActive})
	if v.Reason != "Context 86% >= 80%" || v.Reset.NewSession != "Not yet created" || v.Reset.WorkingDirectory != "Not yet restored" {
		t.Fatalf("14.78 intended state shown as confirmed: %+v", v.Reset)
	}
	now := time.Now()
	k.SessionConfirmedAt, k.CWDConfirmedAt = &now, &now
	v = c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings(), OldSessionSt: worker.SessionReset, NewSessionSt: worker.SessionActive})
	if v.Reset.OldSession != "RESET" || v.Reset.NewSession != "ACTIVE" || v.Reset.WorkingDirectory != "Restored" {
		t.Fatalf("14.78 %+v", v.Reset)
	}
	k.Decision = &c.Decision{Strategy: c.ContinueFreshSession, Reason: c.ReasonContextUnknown, Threshold: 0.8}
	v = c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings()})
	if v.Context.Known || v.Context.Label != "Unknown" || v.Context.Percent != nil || v.Reason != "Context telemetry unavailable" {
		t.Fatalf("14.79 %+v", v.Context)
	}
	k.NoProgressStreak = 2
	if v := c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings()}); v.NoProgress != nil {
		t.Fatal("14.80 below threshold")
	}
	k.NoProgressStreak = 3
	if v := c.BuildProgressView(c.ViewInput{Continuation: k, Settings: c.DefaultSettings()}); v.NoProgress == nil ||
		v.NoProgress.Detail != "3 consecutive continuation reports contained no meaningful change." {
		t.Fatal("14.80")
	}
}

func TestP1422Render(t *testing.T) {
	k, _ := c.NewContinuation(newInput())
	k.Strategy = c.ContinueFreshSession
	k.Captured = c.CapturedStatus{Summary: "half", Completed: []string{"parser"}, Remaining: []string{"handler"}, NextAction: "write handler"}
	out := c.RenderContinuationState(k)
	for _, want := range []string{"Attempt: 1", "Strategy: Fresh Session", "half", "- parser", "- handler", "write handler", "## Blocking\nNone"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if c.RenderContinuationState(k) != out {
		t.Fatal("deterministic")
	}
}

func TestP1423Metrics(t *testing.T) {
	wu := uuid.New()
	now := time.Now()
	done := now.Add(time.Minute)
	rows := []c.Continuation{
		{WorkUnitID: wu, Strategy: c.ContinueSameSession, Status: c.StatusCompleted, CreatedAt: now, CompletedAt: &done},
		{WorkUnitID: wu, Strategy: c.ContinueFreshSession, Status: c.StatusRecoveryRequired, RecoveryReason: c.RecoveryNoProgress, NewSessionID: &wu, SessionConfirmedAt: &now},
		{WorkUnitID: uuid.New(), Status: c.StatusBlocked},
		{WorkUnitID: wu, Status: c.StatusFailed},
	}
	m := c.Aggregate(rows)
	if m.ContinuationsTotal != 4 || m.SameSessionContinuations != 1 || m.FreshSessionContinuations != 1 || m.ContinuationFailures != 1 ||
		m.ContinuationBlocks != 1 || m.ContinuationNoProgress != 1 || m.ContinuationRecoveryRequired != 1 ||
		m.PerWorkUnit[wu].Continuations != 3 || m.PerWorkUnit[wu].SessionResets != 1 || m.CompletedDurationAvg != time.Minute {
		t.Fatalf("%+v", m)
	}
}
