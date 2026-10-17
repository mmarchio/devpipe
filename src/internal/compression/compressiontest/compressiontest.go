// Package compressiontest provides deterministic Compressor and
// SemanticValidator doubles for tests (WBS 9.1 T9.1.2: a mock compressor can
// be substituted). Nothing here calls a model.
package compressiontest

import (
	"context"
	"strings"
	"sync"

	"overseer/internal/compression"
	"overseer/internal/prompt"
)

// Step produces the response for one call.
type Step func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error)

// Scripted is a mock Compressor that replays Steps in order (the last step
// repeats) and records every request.
type Scripted struct {
	mu       sync.Mutex
	Steps    []Step
	requests []compression.CompressionRequest
}

// NewScripted builds a scripted compressor.
func NewScripted(steps ...Step) *Scripted { return &Scripted{Steps: steps} }

// Compress implements compression.Compressor.
func (s *Scripted) Compress(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
	s.mu.Lock()
	i := len(s.requests)
	s.requests = append(s.requests, req)
	var step Step
	if len(s.Steps) > 0 {
		if i >= len(s.Steps) {
			i = len(s.Steps) - 1
		}
		step = s.Steps[i]
	}
	s.mu.Unlock()
	if step == nil {
		return Heuristic(ctx, req)
	}
	return step(ctx, req)
}

// Calls returns the number of Compress calls.
func (s *Scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Requests returns a copy of the recorded requests.
func (s *Scripted) Requests() []compression.CompressionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]compression.CompressionRequest(nil), s.requests...)
}

// Respond returns a step that answers with fixed content.
func Respond(content string) Step {
	return func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		return compression.CompressionResponse{Content: content, Model: req.Model, Provider: "mock"}, nil
	}
}

// Fail returns a step that fails with a generic compression error.
func Fail(kind compression.ErrorKind) Step {
	return func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		return compression.CompressionResponse{}, compression.NewError(kind, "scripted failure")
	}
}

// Block returns a step that blocks until ctx is done (timeouts).
func Block() Step {
	return func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		<-ctx.Done()
		return compression.CompressionResponse{}, ctx.Err()
	}
}

// Mutate returns a step that runs the heuristic compressor and then edits
// its output (e.g. removes an acceptance criterion, adds a requirement).
func Mutate(edit func(string) string) Step {
	return func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		resp, err := Heuristic(ctx, req)
		if err != nil {
			return resp, err
		}
		resp.Content = edit(resp.Content)
		return resp, nil
	}
}

// Heuristic is a deterministic, model-free compressor that obeys the
// compression contract: PRESERVE_EXACT sections verbatim, PRESERVE_SEMANTICS
// sections verbatim with blank lines removed, COMPRESSIBLE sections reduced
// to their first non-empty line, OPTIONAL sections omitted.
func Heuristic(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
	if err := ctx.Err(); err != nil {
		return compression.CompressionResponse{}, err
	}
	out, err := HeuristicText(req.Content)
	if err != nil {
		return compression.CompressionResponse{}, compression.NewError(compression.ErrorMalformed, err.Error())
	}
	return compression.CompressionResponse{
		Content: out, Provider: "heuristic", Model: req.Model,
		InputTokens: compression.EstimateTokens(req.Content), OutputTokens: compression.EstimateTokens(out),
		FinishReason: "stop",
	}, nil
}

// HeuristicText applies the heuristic to a structured input text.
func HeuristicText(structured string) (string, error) {
	in, err := compression.ParseStructuredInput(structured)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, s := range in.Sections {
		switch s.Policy {
		case prompt.PolicyPreserveExact:
			parts = append(parts, s.Content)
		case prompt.PolicyPreserveSemantics:
			var lines []string
			for _, l := range strings.Split(s.Content, "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
			parts = append(parts, strings.Join(lines, "\n"))
		case prompt.PolicyCompressible:
			for _, l := range strings.Split(s.Content, "\n") {
				if t := strings.TrimSpace(l); t != "" {
					parts = append(parts, t)
					break
				}
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

// Semantic is a mock SemanticValidator returning a fixed result/error.
type Semantic struct {
	Result compression.SemanticValidationResult
	Err    error
	mu     sync.Mutex
	calls  int
}

// Validate implements compression.SemanticValidator.
func (m *Semantic) Validate(ctx context.Context, canonical []compression.CompressionComponent, compressed string) (compression.SemanticValidationResult, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.Err != nil {
		return compression.SemanticValidationResult{}, m.Err
	}
	return m.Result, nil
}

// Calls returns the number of Validate calls.
func (m *Semantic) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
