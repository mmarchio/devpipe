package service

import (
	"errors"
	"strings"
	"testing"

	"overseer/internal/continuation"
	"overseer/internal/execution"
	"overseer/internal/project"
	"overseer/internal/worker"
)

// sessionGathered is the minimum gathered state readiness evaluates.
func sessionGathered() *gathered {
	return &gathered{project: &project.Project{Status: project.StatusREADY},
		settings: &project.Settings{WorkerProvider: "hermes", WorkerModel: "m", CompressionProvider: "local", CompressionModel: "c",
			ContextWindow: 64000, ContextThreshold: 0.8},
		exec: execution.DefaultSettings(), workerSt: worker.DefaultSettings(), contSt: continuation.DefaultSettings()}
}

func sessionMessages(r *execution.ExecutionReadinessReport, c execution.ReadinessCategory) []string {
	var out []string
	for _, e := range r.Errors {
		if e.Category == c {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestReadinessSessionProbes(t *testing.T) {
	s := &BootstrapService{}

	g := sessionGathered()
	g.workerProbed, g.compProbed = true, true
	r := s.readiness(g)
	if m := sessionMessages(r, execution.CategoryWorkerSession); m != nil {
		t.Fatalf("ready worker reported: %v", m)
	}
	if m := sessionMessages(r, execution.CategoryCompressionSession); m != nil {
		t.Fatalf("ready compressor reported: %v", m)
	}

	g = sessionGathered()
	g.workerProbed, g.workerProbe = true, errors.New("gateway unreachable")
	g.compProbed, g.compProbe = true, errors.New(`model "c" is not served`)
	r = s.readiness(g)
	if m := sessionMessages(r, execution.CategoryWorkerSession); len(m) != 1 || !strings.Contains(m[0], "gateway unreachable") {
		t.Fatalf("worker session: %v", m)
	}
	if m := sessionMessages(r, execution.CategoryCompressionSession); len(m) != 1 || !strings.Contains(m[0], "not served") {
		t.Fatalf("compression session: %v", m)
	}
	if r.Ready {
		t.Fatal("failed session probes must block readiness")
	}
	v := execution.NewReadinessView(r)
	labels := map[string]bool{}
	for _, c := range v.Checks {
		if !c.OK {
			labels[c.Label] = true
		}
	}
	if !labels["Worker session ready"] || !labels["Compression session ready"] {
		t.Fatalf("checklist rows: %+v", v.Checks)
	}

	// Not probed (no probe wired / provider unconfigured): no row failure.
	g = sessionGathered()
	g.workerProbe, g.compProbe = errors.New("x"), errors.New("y")
	r = s.readiness(g)
	if sessionMessages(r, execution.CategoryWorkerSession) != nil || sessionMessages(r, execution.CategoryCompressionSession) != nil {
		t.Fatal("unprobed results must not be reported")
	}
}
