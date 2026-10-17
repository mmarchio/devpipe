package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// Hermes maintains its own role / skill definitions in the reserved
// .hermes directory of the project: the overseer never commits them,
// they are not working-tree changes in run evidence, and changing them
// never stops the execution. Project files are committed as usual.
func TestWorkerReservedPathsNeverCommitted(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	write := func(rel, content string) {
		p := filepath.Join(r.dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	units := 0
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if a != execution.ActionDispatch || s != "dispatch" {
			return nil
		}
		units++
		switch units {
		case 1: // Hermes refines its role definitions, and even stages them.
			write(".hermes/roles/ENGINEER.md", "learned: prefer table-driven tests\n")
			write(".hermes/skills/go-testing.md", "skill v1\n")
			p15Git(t, r.dir, "add", ".hermes")
		case 2: // Project work plus another self-learning update.
			write("app.txt", "project change\n")
			write(".hermes/skills/go-testing.md", "skill v2\n")
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 200)
	if units != 2 {
		t.Fatalf("unit dispatches %d", units)
	}
	tracked := p15Git(t, r.dir, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tracked, ".hermes") || !strings.Contains(tracked, "app.txt") {
		t.Fatalf("committed files:\n%s", tracked)
	}
	if all := p15Git(t, r.dir, "log", "--name-only", "--format="); strings.Contains(all, ".hermes") {
		t.Fatalf("a checkpoint touched .hermes:\n%s", all)
	}
	if b, err := os.ReadFile(filepath.Join(r.dir, ".hermes/skills/go-testing.md")); err != nil || string(b) != "skill v2\n" {
		t.Fatalf("reserved files lost: %q %v", b, err)
	}
	// The .hermes-only change is not a working-tree change in the first
	// unit's run evidence; the project change in the second is.
	rows, err := r.h.App.DB().Raw().Query(context.Background(), "SELECT payload FROM execution_events WHERE execution_id = $1 AND type = 'WORKER_RUN_REPOSITORY_EVIDENCE' ORDER BY exec_seq", r.execID)
	if err != nil {
		t.Fatal(err)
	}
	var changed []any
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var ev map[string]any
		_ = json.Unmarshal(raw, &ev)
		changed = append(changed, ev["working_tree_changed"])
	}
	rows.Close()
	// Planner run, unit a (.hermes only), unit b (project file).
	if len(changed) != 3 || changed[1] != false || changed[2] != true {
		t.Fatalf("working_tree_changed per run: %v", changed)
	}
}
