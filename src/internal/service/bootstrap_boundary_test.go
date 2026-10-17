package service

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
)

// TestP1578BootstrapHasNoWorkerTransport — WBS 15.77 / 15.78: the bootstrap
// sources never reference the worker transport or a concrete adapter.
func TestP1578BootstrapHasNoWorkerTransport(t *testing.T) {
	files, _ := filepath.Glob("bootstrap_*.go")
	if len(files) == 0 {
		t.Fatal("no bootstrap sources")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, banned := range []string{"*WorkerService", ".Dispatch(", ".NewSession(", "ChangeDirectory(", "gateway/hermes", "HermesGateway"} {
			if strings.Contains(src, banned) {
				t.Fatalf("%s references %q", f, banned)
			}
		}
	}
}

// TestP15105LogFields — WBS 15.105: the structured bootstrap log carries the
// listed fields and never prompt / cache / failure text.
func TestP15105LogFields(t *testing.T) {
	var buf bytes.Buffer
	s := NewBootstrapService(BootstrapDeps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	rec := &persistence.ExecutionRecord{Execution: execution.Execution{ID: uuid.New(), Number: 3, StateMachineVersion: 2, InitialPhaseID: uuid.New()},
		WBSVersion: 4, Stage: execution.StageFinalizing, FirstActionableStateKey: "PHASE_PLAN", PlanHash: "h",
		InitialCacheKey: "prompt-cache:SECRET-PROMPT-KEY", FailureReason: "token=SECRET-VALUE"}
	s.logEvent(rec, uuid.New(), "execution bootstrap completed", s.d.Now())
	out := buf.String()
	for _, f := range []string{"project_id", "execution_id", "execution_number", "phase_id", "wbs_version", "state_machine_version", "bootstrap_stage", "runtime_state", "duration"} {
		if !strings.Contains(out, `"`+f+`"`) {
			t.Fatalf("missing %s in %s", f, out)
		}
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("log leaks content: %s", out)
	}
}
