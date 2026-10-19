package service

import (
	"strings"
	"testing"

	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// A key:value project is told to write key:value task files everywhere
// the format is described; a Markdown project keeps the Markdown layout.
func TestStatefileFormatText(t *testing.T) {
	kv := TaskFileFormat(worker.FormatKeyValue)
	for _, want := range []string{"task: <KEY> — <task name>", "criterion: [ ] [COMMAND] <criterion> `program arg arg`", "No Markdown", "`step: <planned step>`"} {
		if !strings.Contains(kv, want) {
			t.Errorf("key:value format lacks %q", want)
		}
	}
	if strings.Contains(kv, "### ") || strings.Contains(kv, "## <KEY>") {
		t.Errorf("key:value format describes Markdown:\n%s", kv)
	}
	if md := TaskFileFormat(worker.FormatMarkdown); !strings.Contains(md, "## <KEY> — <task name>") || strings.Contains(md, "task: <KEY>") {
		t.Errorf("Markdown format:\n%s", md)
	}
	if TaskFileFormat(worker.FormatXML) != TaskFileFormat(worker.FormatMarkdown) {
		t.Error("XML projects keep Markdown task files")
	}

	skill := OutputFormatSkill("P", worker.FormatKeyValue)
	if !strings.Contains(skill, "key:value lines only") || strings.Contains(skill, "starts with one `# <title>` line") {
		t.Errorf("key:value skill checklist:\n%s", skill)
	}

	if r := reasonTestNotRecorded(worker.FormatKeyValue); !strings.Contains(r, "Check criterion: [x]") || !strings.Contains(r, "status: COMPLETE") ||
		!strings.Contains(r, "in instructions: lines") {
		t.Errorf("key:value test-not-recorded reason: %s", r)
	}
	if r := reasonTestNotRecorded(worker.FormatMarkdown); !strings.Contains(r, "Status: COMPLETE") || !strings.Contains(r, "under ### Instructions") {
		t.Errorf("Markdown test-not-recorded reason: %s", r)
	}
}

// PLAN_REJECTED.md follows the project's format.
func TestRenderPlanRejectionFormats(t *testing.T) {
	problems := wbs.PlanProblems{{Code: "MISSING_PLAN", Message: "no plan"}, {Unit: "A", Code: "NO_TEST", Message: "needs a\ntest"}}
	kv := renderPlanRejection(problems, worker.FormatKeyValue)
	want := "title: Plan rejected\nnote: "
	if !strings.HasPrefix(kv, want) || !strings.Contains(kv, "\nproblem: MISSING_PLAN: no plan\n") || !strings.Contains(kv, "\nproblem: A NO_TEST: needs a test\n") {
		t.Fatalf("key:value rejection:\n%s", kv)
	}
	for _, line := range strings.Split(strings.TrimSpace(kv), "\n") {
		if k, _, ok := strings.Cut(line, ": "); !ok || strings.ContainsAny(k, " #-") {
			t.Fatalf("not a key:value line: %q", line)
		}
	}
	if md := renderPlanRejection(problems, worker.FormatMarkdown); !strings.HasPrefix(md, "# Plan rejected\n") || !strings.Contains(md, "- `A` NO_TEST:") {
		t.Fatalf("Markdown rejection:\n%s", md)
	}
}
