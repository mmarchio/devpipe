package compression_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	ct "overseer/internal/compression/compressiontest"
	"overseer/internal/prompt"
)

type recSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *recSleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	return ctx.Err()
}

const (
	lAC1 = "Validator rejects missing criteria"
	lAC2 = "Validator is deterministic"
	lAC3 = "Modified command fails"
	lT1  = "go test ./internal/compression/..."
	lT2  = "go test ./..."
)

func loopInput(t *testing.T) compression.LoopInput {
	t.Helper()
	mk := func(typ prompt.PromptComponentType, order int, content string) prompt.PromptComponent {
		c, err := prompt.NewPromptComponent(typ, prompt.SourceSystemDefault, order, content, uuid.New(), 1, "")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	task := "work unit 9.17\nacceptance criteria: " + lAC1 + "; " + lAC2 + "; " + lAC3 + "\nrequired tests: " + lT1 + "; " + lT2
	history := strings.Repeat("Earlier phases built persistence, roles, state machines and canonical prompts.\n", 40)
	pcs := prompt.Components{
		mk(prompt.ComponentSystem, 0, "You are the ENGINEER.\nDo not change unrelated code."),
		mk(prompt.ComponentProjectContext, 1, history),
		mk(prompt.ComponentTaskContext, 2, task),
	}
	cs, err := compression.FromPromptComponents(pcs)
	if err != nil {
		t.Fatal(err)
	}
	cs = compression.ApplyEffectivePolicies(cs)
	m := compression.BuildManifest(compression.ManifestInput{
		WorkUnitName: "9.17", Acceptance: []string{lAC1, lAC2, lAC3}, Tests: []string{lT1, lT2},
	}, cs)
	canon := compression.ComponentTokens(cs)
	return compression.LoopInput{
		CanonicalPromptID: uuid.New(), CanonicalTokens: canon,
		Components: cs, Canonical: cs, Manifest: m,
		Instruction: compression.DefaultInstruction(), TargetTokens: canon / 2, MinimumSavings: 0.15,
	}
}

func newLoop(c compression.Compressor, max int, s compression.Sleeper) compression.AttemptLoop {
	return compression.AttemptLoop{
		Compressor: c,
		Pipeline:   compression.Pipeline{Semantic: []compression.SemanticValidator{compression.DeterministicSemanticValidator{}}},
		Sleeper:    s, Backoff: compression.Backoff{Base: 100 * time.Millisecond, Max: 250 * time.Millisecond},
		MaxAttempts: max, Timeout: time.Second, Provider: "mock", Model: "m",
	}
}

func dropAC3AndTest2(s string) string {
	s = strings.Replace(s, "; "+lAC3, "", 1)
	return strings.Replace(s, "; "+lT2, "", 1)
}

func TestHeuristicCompressorPassesPipeline(t *testing.T) {
	in := loopInput(t)
	res, err := newLoop(ct.NewScripted(), 1, &recSleeper{}).Run(context.Background(), in, nil)
	if err != nil || res.Accepted == nil {
		t.Fatalf("heuristic rejected: %v %+v", err, res.Attempts[0].Validation)
	}
}

func TestT926AndT927ValidationGuidedRetry(t *testing.T) {
	in := loopInput(t)
	sc := ct.NewScripted(ct.Mutate(dropAC3AndTest2), nil)
	var sunk []int
	res, err := newLoop(sc, 2, &recSleeper{}).Run(context.Background(), in, func(ctx context.Context, a *compression.Attempt) error {
		sunk = append(sunk, a.Number)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Number != 1 || res.Attempts[1].Number != 2 {
		t.Fatalf("T9.26.1/T9.26.3 numbering %+v", res.Attempts)
	}
	if res.Attempts[0].Status != compression.AttemptValidationFailed || res.Attempts[0].Output == "" {
		t.Fatal("T9.26.2/T9.26.5 failed attempt + output retained")
	}
	if res.Accepted == nil || res.Accepted.Number != 2 || !res.Attempts[1].Accepted() {
		t.Fatal("T9.26.4 accepted attempt identifiable")
	}
	if len(sunk) != 2 {
		t.Fatal("every attempt reaches the sink")
	}
	reqs := sc.Requests()
	if reqs[0].Feedback != "" || !strings.Contains(reqs[1].Feedback, "AC-3") || !strings.Contains(reqs[1].Feedback, "TEST-2") {
		t.Fatalf("T9.27.1 feedback: %q", reqs[1].Feedback)
	}
	// T9.27.2/T9.27.3: retry source is the canonical structured input; the
	// failed output never appears in the retry request.
	if reqs[1].Content != reqs[0].Content || strings.Contains(reqs[1].Feedback, res.Attempts[0].Output) {
		t.Fatal("retry not sourced from canonical")
	}
}

func TestT928RetryLimit(t *testing.T) {
	in := loopInput(t)
	sc := ct.NewScripted(ct.Mutate(dropAC3AndTest2))
	res, err := newLoop(sc, 3, &recSleeper{}).Run(context.Background(), in, nil)
	if err != nil || res.Accepted != nil || len(res.Attempts) != 3 || sc.Calls() != 3 {
		t.Fatalf("T9.28.2/T9.28.3: calls=%d attempts=%d", sc.Calls(), len(res.Attempts))
	}
	sc1 := ct.NewScripted(ct.Mutate(dropAC3AndTest2))
	res, _ = newLoop(sc1, 1, &recSleeper{}).Run(context.Background(), in, nil)
	if sc1.Calls() != 1 || len(res.Attempts) != 1 {
		t.Fatal("max_attempts=1 retried")
	}
}

func TestT938BackoffAndProviderFailures(t *testing.T) {
	in := loopInput(t)
	sl := &recSleeper{}
	sc := ct.NewScripted(ct.Fail(compression.ErrorRateLimit), ct.Fail(compression.ErrorUnavailable), nil)
	res, err := newLoop(sc, 3, sl).Run(context.Background(), in, nil)
	if err != nil || res.Accepted == nil || res.Accepted.Number != 3 {
		t.Fatalf("T9.38.1 retryable recovery: %v %+v", err, res)
	}
	if len(sl.delays) != 2 || sl.delays[0] != 100*time.Millisecond || sl.delays[1] != 200*time.Millisecond {
		t.Fatalf("backoff delays %v", sl.delays)
	}
	if res.Attempts[0].Status != compression.AttemptProviderError || res.Attempts[0].ErrorKind != compression.ErrorRateLimit {
		t.Fatal("provider error attempt recorded")
	}
	// T9.38.2 nonretryable stops.
	sc2 := ct.NewScripted(ct.Fail(compression.ErrorAuth))
	res, _ = newLoop(sc2, 3, &recSleeper{}).Run(context.Background(), in, nil)
	if sc2.Calls() != 1 || !res.Permanent {
		t.Fatal("T9.38.2 auth retried")
	}
	// T9.38.3 max retries respected.
	sc3 := ct.NewScripted(ct.Fail(compression.ErrorNetwork))
	res, _ = newLoop(sc3, 2, &recSleeper{}).Run(context.Background(), in, nil)
	if sc3.Calls() != 2 || res.Accepted != nil {
		t.Fatal("T9.38.3")
	}
	b := compression.Backoff{Base: 100 * time.Millisecond, Max: 250 * time.Millisecond}
	if b.Delay(1) != 100*time.Millisecond || b.Delay(3) != 250*time.Millisecond || b.Delay(10) != 250*time.Millisecond {
		t.Fatal("backoff bound")
	}
}

func TestT9384CancellationInterruptsBackoff(t *testing.T) {
	in := loopInput(t)
	ctx, cancel := context.WithCancel(context.Background())
	l := newLoop(ct.NewScripted(ct.Fail(compression.ErrorNetwork)), 3, compression.RealSleeper{})
	l.Backoff = compression.Backoff{Base: time.Hour, Max: time.Hour}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := l.Run(ctx, in, nil)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation did not interrupt backoff: %v", err)
	}
}

func TestT936Timeout(t *testing.T) {
	in := loopInput(t)
	var released atomic.Int32
	block := func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		<-ctx.Done()
		released.Add(1)
		return compression.CompressionResponse{}, ctx.Err()
	}
	sc := ct.NewScripted(block, nil)
	l := newLoop(sc, 2, &recSleeper{})
	l.Timeout = 20 * time.Millisecond
	res, err := l.Run(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts[0].ErrorKind != compression.ErrorTimeout {
		t.Fatalf("T9.36.1/T9.36.2 %+v", res.Attempts[0])
	}
	if res.Accepted == nil || res.Accepted.Number != 2 {
		t.Fatal("T9.36.3 retry after timeout")
	}
	if released.Load() != 1 {
		t.Fatal("T9.36.4 provider request not cancelled")
	}
}

func TestEmptyResponseRejected(t *testing.T) {
	in := loopInput(t)
	res, _ := newLoop(ct.NewScripted(ct.Respond("   ")), 1, &recSleeper{}).Run(context.Background(), in, nil)
	if res.Attempts[0].ErrorKind != compression.ErrorEmpty || res.Accepted != nil {
		t.Fatal("T9.37.4 empty response accepted")
	}
}

func TestT942SingleFlight(t *testing.T) {
	var g compression.SingleFlight
	var calls atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]any, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, _, _ := g.Do("same", func() (any, error) {
				calls.Add(1)
				time.Sleep(50 * time.Millisecond)
				return "result", nil
			})
			results[i] = v
		}(i)
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("T9.42.1 calls=%d", calls.Load())
	}
	for _, r := range results {
		if r != "result" {
			t.Fatal("T9.42.2")
		}
	}
	boom := errors.New("boom")
	var errs atomic.Int32
	var wg2 sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			<-gate
			if _, err, _ := g.Do("fail", func() (any, error) { time.Sleep(30 * time.Millisecond); return nil, boom }); errors.Is(err, boom) {
				errs.Add(1)
			}
		}()
	}
	close(gate)
	wg2.Wait()
	if errs.Load() != 3 {
		t.Fatal("T9.42.3")
	}
	var a, b atomic.Int32
	g.Do("k1", func() (any, error) { a.Add(1); return nil, nil })
	g.Do("k2", func() (any, error) { b.Add(1); return nil, nil })
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatal("T9.42.4")
	}
}

func TestT948Diff(t *testing.T) {
	canon := "keep exact line\nlong narrative one\nlong narrative two\ncommand: go test ./..."
	comp := "keep exact line\nshort narrative\ncommand: go test ./..."
	ca, cb := canon, comp
	d := compression.LineDiff(canon, comp)
	if d.Removed != 2 || d.Added != 1 || d.Equal != 2 {
		t.Fatalf("T9.48.1/T9.48.2 %+v", d)
	}
	for _, l := range d.Lines {
		if (l.Text == "keep exact line" || l.Text == "command: go test ./...") && l.Op != compression.DiffEqual {
			t.Fatal("T9.48.3 exact line differs")
		}
	}
	if canon != ca || comp != cb {
		t.Fatal("T9.48.4")
	}
	if !equalDiff(d, compression.LineDiff(canon, comp)) {
		t.Fatal("non-deterministic diff")
	}
}

func equalDiff(a, b compression.Diff) bool {
	if len(a.Lines) != len(b.Lines) {
		return false
	}
	for i := range a.Lines {
		if a.Lines[i] != b.Lines[i] {
			return false
		}
	}
	return true
}
