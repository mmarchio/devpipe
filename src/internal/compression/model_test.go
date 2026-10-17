package compression

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/prompt"
)

// --- 9.4 component model ---

func TestT941PolicyRetained(t *testing.T) {
	pcs := fxPromptComponents(t)
	cs, err := FromPromptComponents(pcs)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cs {
		if c.Policy != pcs[i].Policy {
			t.Fatalf("component %d policy %s, want %s", i, c.Policy, pcs[i].Policy)
		}
	}
}

func TestT942SourceMetadataRetained(t *testing.T) {
	pcs := fxPromptComponents(t)
	cs, _ := FromPromptComponents(pcs)
	for i, c := range cs {
		p := pcs[i]
		if c.SourceID != p.SourceID || c.SourceVersion != p.SourceVersion || c.SourceHash != p.SourceHash || c.Type != p.Type || c.Content != p.Content {
			t.Fatalf("component %d metadata not retained", i)
		}
	}
}

func TestT943InvalidPolicyRejected(t *testing.T) {
	pcs := fxPromptComponents(t)
	pcs[1].Policy = "SHRINK_A_LOT"
	if _, err := FromPromptComponents(pcs); err == nil {
		t.Fatal("unknown policy accepted")
	}
	cs := fxComponents(t)
	cs[0].Policy = "BAD"
	if ValidateComponents(cs) == nil {
		t.Fatal("ValidateComponents accepted unknown policy")
	}
}

func TestT944OrderDeterministic(t *testing.T) {
	pcs := fxPromptComponents(t)
	rev := make(prompt.Components, len(pcs))
	for i := range pcs {
		rev[len(pcs)-1-i] = pcs[i]
	}
	a, _ := FromPromptComponents(pcs)
	b, _ := FromPromptComponents(rev)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("conversion depends on input slice order")
	}
	for i, c := range a {
		if c.Ordinal != i {
			t.Fatalf("ordinal %d at %d", c.Ordinal, i)
		}
	}
	if ValidateComponents(a) != nil {
		t.Fatal("valid components rejected")
	}
	before := pcs[0].Content
	_, _ = FromPromptComponents(pcs)
	if pcs[0].Content != before {
		t.Fatal("input mutated")
	}
}

// --- 9.3 request model ---

func TestT931To935Request(t *testing.T) {
	comps := fxComponents(t)
	base := CompressionRequest{CanonicalPromptID: uuid.New(), Content: "x", Components: comps, TargetTokens: 100, Model: "m", Attempt: 1}
	r, err := NewCompressionRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	// T9.3.4 metadata retained, independent copy.
	comps[0].Content = "mutated"
	if r.Components[0].Content == "mutated" || len(r.Components) != len(comps) {
		t.Fatal("components not retained independently")
	}
	// T9.3.2 canonical required.
	bad := base
	bad.CanonicalPromptID = uuid.Nil
	if _, err := NewCompressionRequest(bad); !errors.Is(err, ErrMissingCanonicalPrompt) {
		t.Fatalf("missing canonical: %v", err)
	}
	bad = base
	bad.Content = "  "
	if _, err := NewCompressionRequest(bad); !errors.Is(err, ErrMissingCanonicalPrompt) {
		t.Fatalf("empty content: %v", err)
	}
	// T9.3.3 target validated.
	bad = base
	bad.TargetTokens = 0
	if _, err := NewCompressionRequest(bad); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("target: %v", err)
	}
	bad = base
	bad.Attempt = 0
	if _, err := NewCompressionRequest(bad); !errors.Is(err, ErrInvalidAttempt) {
		t.Fatalf("attempt: %v", err)
	}
	// T9.3.5 no settings/credential field is representable.
	typ := reflect.TypeOf(CompressionRequest{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		for _, bad := range []string{"key", "secret", "password", "token" + "s_", "credential", "settings", "project"} {
			if strings.Contains(n, bad) {
				t.Fatalf("request field %s looks like a setting/credential", typ.Field(i).Name)
			}
		}
	}
}

// --- 9.1 / 9.12 / 9.37 compressor contract ---

type ctxCompressor struct{ seen context.Context }

func (c *ctxCompressor) Compress(ctx context.Context, req CompressionRequest) (CompressionResponse, error) {
	c.seen = ctx
	<-ctx.Done()
	return CompressionResponse{}, ctx.Err()
}

func TestT911To915CompressorContract(t *testing.T) {
	var c Compressor = CompressorFunc(func(ctx context.Context, req CompressionRequest) (CompressionResponse, error) {
		return CompressionResponse{Content: "short", Model: req.Model, Provider: "mock", Duration: time.Millisecond}, nil
	})
	req, _ := NewCompressionRequest(CompressionRequest{CanonicalPromptID: uuid.New(), Content: "abc", TargetTokens: 1, Model: "m", Attempt: 1})
	resp, err := c.Compress(context.Background(), req)
	if err != nil || resp.Content != "short" || resp.Model != "m" || resp.Duration != time.Millisecond {
		t.Fatalf("mock compressor: %+v %v", resp, err)
	}
	// T9.12.5 optional metadata absent is fine (zero values).
	if resp.InputTokens != 0 || resp.ProviderRequestID != "" {
		t.Fatal("unexpected metadata")
	}
	// T9.1.4 generic error.
	e := NewError(ErrorRateLimit, "slow down")
	if KindOf(e) != ErrorRateLimit || !IsRetryable(e) {
		t.Fatal("generic error classification")
	}
	// T9.1.5 cancellation propagated.
	cc := &ctxCompressor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cc.Compress(ctx, req); !errors.Is(err, context.Canceled) || KindOf(err) != ErrorCancelled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestT937ErrorClassification(t *testing.T) {
	cases := map[ErrorKind]bool{
		ErrorNetwork: true, ErrorUnavailable: true, ErrorRateLimit: true, ErrorTimeout: true,
		ErrorMalformed: true, ErrorEmpty: true,
		ErrorAuth: false, ErrorContextOverflow: false, ErrorCancelled: false, ErrorConfiguration: false, ErrorUnknown: false,
	}
	for k, want := range cases {
		if k.Retryable() != want {
			t.Fatalf("%s retryable=%v want %v", k, k.Retryable(), want)
		}
	}
	if KindOf(context.DeadlineExceeded) != ErrorTimeout || KindOf(errors.New("x")) != ErrorUnknown {
		t.Fatal("KindOf")
	}
}

// --- 9.2 settings ---

func validSettings() Settings {
	s := DefaultSettings()
	s.Provider, s.Model = "local", "qwen"
	s.ContextWindow, s.ContextThreshold = 64000, 0.8
	s.TargetTokens = 12000
	return s
}

func TestT921To925Settings(t *testing.T) {
	s := validSettings()
	if err := s.Validate(); err != nil {
		t.Fatalf("T9.2.1 valid rejected: %v", err)
	}
	if s.EffectiveTarget() != 12000 {
		t.Fatal("target")
	}
	bad := s
	bad.TargetRatio = 1.5
	if bad.Validate() == nil {
		t.Fatal("T9.2.2 invalid ratio accepted")
	}
	bad = s
	bad.TargetTokens = -1
	if bad.Validate() == nil {
		t.Fatal("T9.2.3 negative target accepted")
	}
	bad = s
	bad.MaxAttempts = 0
	if bad.Validate() == nil {
		t.Fatal("T9.2.4 zero attempts accepted")
	}
	bad.MaxAttempts = MaxAttemptsLimit + 1
	if bad.Validate() == nil {
		t.Fatal("T9.2.4 unbounded attempts accepted")
	}
	off := s
	off.Mode = ModeNever
	if off.Enabled() || off.Validate() != nil {
		t.Fatal("T9.2.5 disabled compression")
	}
	r := s
	r.TargetTokens, r.TargetRatio = 0, 0.25
	if r.EffectiveTarget() != 16000 {
		t.Fatal("ratio target")
	}
	r.TargetRatio = 0
	if r.EffectiveTarget() != 51200 {
		t.Fatal("threshold target")
	}
	var se *SettingsError
	bad = s
	bad.Mode, bad.MinimumSavings = "SOMETIMES", 2
	if err := bad.Validate(); !errors.As(err, &se) || se.Fields["mode"] == "" || se.Fields["minimum_savings"] == "" {
		t.Fatalf("multiple failures not reported: %v", err)
	}
}

// --- 9.15 / 9.16 metrics + savings ---

func TestT915Metrics(t *testing.T) {
	m := ComputeMetrics(18200, 7400, time.Second, 2)
	if m.TokensSaved != 10800 {
		t.Fatal("T9.15.1 saved")
	}
	if got := int(m.PercentSaved*100 + 0.5); got != 5934 {
		t.Fatalf("T9.15.2 percent %v", m.PercentSaved)
	}
	z := ComputeMetrics(0, 0, 0, 1)
	if z.Ratio != 0 || z.PercentSaved != 0 {
		t.Fatal("T9.15.3 zero")
	}
	e := ComputeMetrics(7000, 8200, 0, 1)
	if e.TokensSaved >= 0 || e.PercentSaved >= 0 {
		t.Fatal("T9.15.4 expansion must be negative savings")
	}
	if ClassifySavings(7000, 8200, 0.15) != SavingsExpanded {
		t.Fatal("expansion class")
	}
}

func TestT916MinimumSavings(t *testing.T) {
	if ClassifySavings(10000, 8000, 0.15) != SavingsEffective {
		t.Fatal("T9.16.1")
	}
	if ClassifySavings(10000, 9500, 0.15) != SavingsIneffective {
		t.Fatal("T9.16.2")
	}
	if ClassifySavings(10000, 9500, 0.04) != SavingsEffective {
		t.Fatal("T9.16.3 configurable")
	}
}

// --- 9.10 instruction ---

func TestT9102And9104Instruction(t *testing.T) {
	d := DefaultInstruction()
	if d.Version != 1 || d.Hash != InstructionHash(DefaultInstructionContent) || len(d.Hash) != 64 {
		t.Fatal("default instruction")
	}
	v2, err := NewInstruction(2, DefaultInstructionContent+"\nKeep headings.")
	if err != nil || v2.Hash == d.Hash {
		t.Fatal("changed instruction must change hash")
	}
	if _, err := NewInstruction(1, " "); !errors.Is(err, ErrEmptyInstruction) {
		t.Fatal("empty instruction accepted")
	}
	for _, rule := range []string{"PRESERVE_EXACT", "Do not add requirements.", "Do not change the response schema.", "<preserve></preserve>"} {
		if !strings.Contains(d.Content, rule) {
			t.Fatalf("instruction lacks %q", rule)
		}
	}
}

// --- 9.14 / 9.33 / 9.43 / 9.44 hashes ---

func TestT914CompressedHash(t *testing.T) {
	a := CompressedPromptHash("abc")
	if a == "" || a != CompressedPromptHash("abc") || a == CompressedPromptHash("abd") {
		t.Fatal("compressed hash")
	}
	canon, _ := prompt.CanonicalPromptHash(prompt.CanonicalPromptHashInput{Content: "abc"})
	if canon == a {
		t.Fatal("T9.14.4 domains collide")
	}
}

func TestT933WorkerHash(t *testing.T) {
	a := WorkerPromptHash("x", SourceCompressed)
	if a != WorkerPromptHash("x", SourceCompressed) || a == WorkerPromptHash("x", SourceCanonical) || a == WorkerPromptHash("y", SourceCompressed) {
		t.Fatal("worker hash")
	}
}

func TestT943AndT944Identity(t *testing.T) {
	s := validSettings()
	in := DefaultInstruction()
	h := ConfigHashFor(s, in)
	if h != ConfigHashFor(s, in) {
		t.Fatal("T9.44.1")
	}
	m := s
	m.Model = "other"
	if ConfigHashFor(m, in) == h {
		t.Fatal("T9.44.2")
	}
	in2, _ := NewInstruction(2, "different")
	if ConfigHashFor(s, in2) == h {
		t.Fatal("T9.44.3")
	}
	tg := s
	tg.TargetTokens = 8000
	if ConfigHashFor(tg, in) == h {
		t.Fatal("T9.44.4")
	}
	cw := s
	cw.ContextWindow = 32000
	if ConfigHashFor(cw, in) == h {
		t.Fatal("context window (fallback policy input) must change the hash")
	}
	op := s
	op.Timeout = time.Hour
	if ConfigHashFor(op, in) != h {
		t.Fatal("operational timeout must not change the semantic hash")
	}
	id := RequestIdentity("canon", h)
	if id != RequestIdentity("canon", h) || id == RequestIdentity("canon", ConfigHashFor(m, in)) || id == RequestIdentity("canon", ConfigHashFor(s, in2)) {
		t.Fatal("T9.43.1/3/4")
	}
}

// --- 9.13 / 9.26 / 9.32 artifacts ---

func TestT913AndT932Artifacts(t *testing.T) {
	cid := uuid.New()
	cp, err := NewCompressedPrompt(CompressedPrompt{CanonicalPromptID: cid, Attempt: 2, Content: "compressed", Model: "m", ValidationStatus: ValidationFailed})
	if err != nil || cp.Hash != CompressedPromptHash("compressed") || cp.Attempt != 2 || cp.Model != "m" {
		t.Fatal("compressed prompt")
	}
	if _, err := WorkerPromptFromCompressed(cp, uuid.New(), time.Now()); !errors.Is(err, ErrUnvalidatedContent) {
		t.Fatal("T9.32.3 failed validation produced a worker prompt")
	}
	cp.ValidationStatus = ValidationPassed
	wp, err := WorkerPromptFromCompressed(cp, uuid.New(), time.Now())
	if err != nil || wp.Source != SourceCompressed || wp.CompressedPromptID == nil || *wp.CompressedPromptID != cp.ID || wp.Content != "compressed" {
		t.Fatal("T9.32.1")
	}
	cw, err := WorkerPromptFromCanonical(cid, uuid.New(), "canonical", time.Now())
	if err != nil || cw.Source != SourceCanonical || cw.CompressedPromptID != nil || cw.Hash == wp.Hash {
		t.Fatal("T9.32.2/T9.32.4")
	}
	if _, err := NewCompressedPrompt(CompressedPrompt{CanonicalPromptID: cid, Attempt: 0, Content: "x", ValidationStatus: ValidationPassed}); err == nil {
		t.Fatal("attempt 0 accepted")
	}
}

// --- 9.30 / 9.31 / 9.29 decisions ---

func TestT930AndT931Decision(t *testing.T) {
	s := validSettings() // target 12000, window 64000
	d, err := Decide(s, 5000)
	if err != nil || d.Action != ActionBypass || d.Reason == "" {
		t.Fatal("T9.30.1/T9.31.1 small bypass")
	}
	d, _ = Decide(s, 20000)
	if d.Action != ActionCompress {
		t.Fatal("T9.31.1 large compress")
	}
	a := s
	a.Mode = ModeAlways
	if d, _ := Decide(a, 10); d.Action != ActionCompress {
		t.Fatal("T9.31.2")
	}
	n := s
	n.Mode = ModeNever
	if d, _ := Decide(n, 50000); d.Action != ActionBypass {
		t.Fatal("T9.31.3")
	}
	if _, err := Decide(n, 70000); !errors.Is(err, ErrCanonicalExceedsContext) {
		t.Fatal("T9.31.4")
	}
}

func TestT929Fallback(t *testing.T) {
	o, r, err := FallbackDecision(10000, 64000, "compression failed")
	if err != nil || o != RunFallbackCanonical || r == "" {
		t.Fatal("T9.29.1")
	}
	o, _, err = FallbackDecision(70000, 64000, "compression failed")
	if o != RunFailed || !errors.Is(err, ErrPromptPreparationFailed) {
		t.Fatal("T9.29.3")
	}
}
