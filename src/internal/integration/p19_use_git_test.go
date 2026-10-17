package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// A project that uses git (checkpoint policy REQUIRED) whose working
// directory is not a git repository fails bootstrap with a configuration
// error naming the fix; with git turned off the same directory bootstraps.
func TestP19UseGitRequiresRepository(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true, noBootstrap: true})
	if err := os.RemoveAll(filepath.Join(r.dir, ".git")); err != nil {
		t.Fatal(err)
	}
	_, be := r.bootstrapErr(t)
	if be.Code != execution.CodeRepositoryInvalid || be.Class != execution.ClassConfiguration ||
		!strings.Contains(be.Message, "not a git repository") || !strings.Contains(be.Message, "Use git") {
		t.Fatalf("bootstrap error %+v", be)
	}

	r.settings.CheckpointPolicy = execution.CheckpointPolicyFor(false)
	r.hard.GroupCheckpointPolicy = string(execution.CheckpointPolicyFor(false))
	if err := r.h.App.Engine().UpdateEngineSettings(context.Background(), r.pid, r.settings, r.hard); err != nil {
		t.Fatalf("engine settings: %v", err)
	}
	r.boot(t)
}
