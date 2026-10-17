package statefile

import "testing"

// T7.2.1/T7.2.2 unit half — The six default definitions exist with the WBS
// 7.2 keys, names, and default filenames, in stable declaration order. The
// scope of each default follows the WBS 7.3 examples: PROJECT_STATE /
// ARCHITECTURE / TASK_ARCHIVE are PROJECT-scoped, PHASE_STATE is
// PHASE-scoped, and ACTIVE_TASK is WORK_UNIT-scoped; TEST_STATE is legal at
// PHASE or WORK_UNIT scope (WBS 7.3) and the global default pins PHASE.
func TestDefaultFileTypes(t *testing.T) {
	want := []struct {
		key, name, filename string
		scope               StateFileScope
	}{
		{"PROJECT_STATE", "Project State", "PROJECT_STATE.md", ScopeProject},
		{"PHASE_STATE", "Phase State", "PHASE_STATE.md", ScopePhase},
		{"ACTIVE_TASK", "Active Task", "ACTIVE_TASK.md", ScopeWorkUnit},
		{"TASK_ARCHIVE", "Task Archive", "TASK_ARCHIVE.md", ScopeProject},
		{"ARCHITECTURE", "Architecture", "ARCHITECTURE.md", ScopeProject},
		{"TEST_STATE", "Test State", "TEST_STATE.md", ScopePhase},
	}
	got := DefaultFileTypes()
	if len(got) != len(want) {
		t.Fatalf("len(DefaultFileTypes()) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Key != w.key || g.Name != w.name || g.DefaultFilename != w.filename || g.Scope != w.scope {
			t.Errorf("default[%d] = %+v, want key %q name %q filename %q scope %q", i, g, w.key, w.name, w.filename, w.scope)
		}
		if !g.Scope.Known() {
			t.Errorf("default[%d] scope %q is not a known scope", i, g.Scope)
		}
	}
}
