package wbs

import (
	"strings"
	"testing"
)

// ResetTask restarts one task in either layout: status TODO, criteria
// unchecked, every other task and line untouched.
func TestResetTask(t *testing.T) {
	for name, in := range map[string]string{"markdown": sampleTasks, "key:value": sampleTasksKV} {
		out, found := ResetTask(in, "T1")
		if !found {
			t.Fatalf("%s: T1 not found", name)
		}
		d, err := ParseTasks(out)
		if err != nil {
			t.Fatalf("%s: reset file does not parse: %v", name, err)
		}
		t1, t2 := d.Find("T1"), d.Find("T2")
		if t1.Status != "TODO" || len(t1.Met) != 3 || t1.Met[0] || t1.Met[1] || t1.Met[2] {
			t.Fatalf("%s: T1 not restarted: %+v", name, t1)
		}
		before, _ := ParseTasks(in)
		if t1.Unit.Objective != before.Find("T1").Unit.Objective || len(t1.Unit.AcceptanceChecks) != 3 {
			t.Fatalf("%s: T1 definition changed: %+v", name, t1.Unit)
		}
		// T2 had no status line and is untouched; T2's section text too.
		if t2.Status != before.Find("T2").Status {
			t.Fatalf("%s: T2 changed: %+v", name, t2)
		}
		if strings.Count(out, "\n") != strings.Count(in, "\n") {
			t.Fatalf("%s: lines added or removed:\n%s", name, out)
		}
	}
	// A task without a status line gets one; a missing task is reported.
	out, found := ResetTask(sampleTasks, "T2")
	if d, _ := ParseTasks(out); !found || d.Find("T2").Status != "TODO" || d.Find("T1").Status != "ACTIVE" {
		t.Fatalf("T2 reset:\n%s", out)
	}
	if out, found := ResetTask(sampleTasks, "T9"); found || out != sampleTasks {
		t.Fatal("missing task reported as found or file changed")
	}
}
