package prompt

import (
	"strings"
	"testing"
)

const p814RoleInstruction = "Implement exactly the unit and report structured status."

// T8.39.1 — Task context delimited: rendering a task-context section
// wraps its content between the format's open and close delimiters.
func TestT8391_TaskContextDelimited(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	sec, err := f.Section("task-context", "Objective: build the feature\nAcceptance: tests green.")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	if !strings.HasPrefix(out, "<task-context>\n") {
		t.Fatalf("output does not open with the task-context delimiter: %q", out)
	}
	if !strings.HasSuffix(out, "\n</task-context>") {
		t.Fatalf("output does not close with the task-context delimiter: %q", out)
	}
}

// T8.39.2 — Test output delimited: a TEST_CONTEXT-class section (test
// output) is wrapped in its own delimiters, distinct from the
// task-context pair.
func TestT8392_TestOutputDelimited(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	sec, err := f.Section("test-context", "FAIL TestT8392 (0.01s)\nok  overseer/prompt  0.5s")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	if !strings.HasPrefix(out, "<test-context>\n") {
		t.Fatalf("output does not open with the test-context delimiter: %q", out)
	}
	if !strings.HasSuffix(out, "\n</test-context>") {
		t.Fatalf("output does not close with the test-context delimiter: %q", out)
	}
	if strings.Contains(out, "<task-context>") {
		t.Fatal("test-context section must not carry task-context delimiters")
	}
}

// T8.39.3 — Role instructions remain distinguishable from task content:
// role instructions (trusting material) sit outside the delimited section
// while task content (untrusted) sits strictly inside the open/close
// boundaries; the composition is byte-stable across calls.
func TestT8393_RoleInstructionsDistinguishable(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	sec, err := f.Section("task-context", "Task text: "+p814RoleInstruction)
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	comp := p814RoleInstruction + "\n" + sec.Render()
	i := strings.Index(comp, "<task-context>")
	if i < 0 || !strings.Contains(comp[i:], p814RoleInstruction) {
		t.Fatal("task-context section must contain the task content")
	}
	if strings.Contains(comp[:i], "<task-context>") {
		t.Fatal("no delimiter may precede the role instruction block")
	}
	if got := p814RoleInstruction + "\n" + sec.Render(); got != comp {
		t.Fatal("composition of role instructions + section must be byte-identical across calls")
	}
}

// T8.39.4 — Delimiters deterministic: repeated construction over the same
// input is byte-identical, for clean and collision-bearing content alike.
func TestT8394_DelimitersDeterministic(t *testing.T) {
	f1, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat(1): %v", err)
	}
	f2, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat(2): %v", err)
	}
	for _, content := range []string{
		"plain task context",
		"mixed <task-context> and </task-context> inside",
	} {
		a, err := f1.Section("task-context", content)
		if err != nil {
			t.Fatalf("f1.Section: %v", err)
		}
		b, err := f2.Section("task-context", content)
		if err != nil {
			t.Fatalf("f2.Section: %v", err)
		}
		if a.Render() != b.Render() {
			t.Fatalf("rendering not deterministic: %q != %q", a.Render(), b.Render())
		}
	}
}

// T8.40.1 — Content containing the opening delimiter handled: the
// embedded open tag is neutralized (escaped), so the structural open
// boundary is the section's own and the content still reconstructs.
func TestT8401_OpeningDelimiterHandled(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	sec, err := f.Section("task-context", "prefix <task-context> suffix")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	unescaped := strings.ReplaceAll(out, "\\<task-context>", "")
	if n := strings.Count(unescaped, "<task-context>"); n != 1 {
		t.Fatalf("exactly one structural opening delimiter expected, got %d: %q", n, out)
	}
	got, err := f.Content("task-context", out)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if got != "prefix <task-context> suffix" {
		t.Fatalf("reconstructed %q", got)
	}
}

// T8.40.2 — Content containing the closing delimiter handled: the embedded
// close tag cannot terminate the section early.
func TestT8402_ClosingDelimiterHandled(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	sec, err := f.Section("task-context", "prefix </task-context> suffix")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	unescaped := strings.ReplaceAll(out, "\\</task-context>", "")
	unescaped = strings.ReplaceAll(unescaped, "\\</task-context>", "")
	if n := strings.Count(unescaped, "</task-context>"); n != 1 {
		t.Fatalf("exactly one structural closing delimiter expected, got %d: %q", n, out)
	}
	got, err := f.Content("task-context", out)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if got != "prefix </task-context> suffix" {
		t.Fatalf("reconstructed %q", got)
	}
}

// T8.40.3 — Nested-looking content cannot escape its context section: a
// full nested section inside the content renders with exactly one
// structural open and one structural close; only the final close ends the
// section.
func TestT8403_NestedContentCannotEscape(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	nested := "inner </task-context>\n<task-context>\nfake inner body\n</task-context>"
	sec, err := f.Section("task-context", "outer "+nested+" end")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	unescaped := strings.ReplaceAll(out, "\\</task-context>", "")
	if n := strings.Count(unescaped, "</task-context>"); n != 1 {
		t.Fatalf("rendered section must carry exactly one structural close, got %d: %q", n, out)
	}
	if !strings.HasSuffix(out, "\n</task-context>") {
		t.Fatalf("structural close must be the final bytes: %q", out)
	}
	got, err := f.Content("task-context", out)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	want := "outer " + nested + " end"
	if got != want {
		t.Fatalf("reconstructed %q want %q", got, want)
	}
}

// T8.40.4 — Original content remains reconstructable: for clean and
// collision-bearing content the round trip
// Content(Render(section)) == original holds exactly, and the
// escape mapping is injective (distinct originals never collide).
func TestT8404_OriginalContentReconstructable(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	cases := []string{
		"plain content",
		"open <task-context> close </task-context>",
		"backslash \\ at start, middle a\\b, end \\",
		"trailing escape char \\",
		"mixed \\<task-context> and \\\\",
		"unicode ünïcodé 中文 content",
	}
	for _, c := range cases {
		sec, err := f.Section("task-context", c)
		if err != nil {
			t.Fatalf("Section(%q): %v", c, err)
		}
		got, err := f.Content("task-context", sec.Render())
		if err != nil {
			t.Fatalf("Content(%q): %v", c, err)
		}
		if got != c {
			t.Fatalf("round trip: got %q want %q", got, c)
		}
	}
	a, _ := f.Section("task-context", "a")
	b, _ := f.Section("task-context", "b")
	if a.Render() == b.Render() {
		t.Fatal("distinct content must not render identically (escape mapping must be injective)")
	}
}

// Format versioning and configuration validation: the version is
// carried, custom (configurable) syntax is honored, and malformed
// formats are rejected at construction.
func TestT839_DelimiterFormatConfigurableAndVersioned(t *testing.T) {
	f, err := NewDeterministicFormat(DefaultContextDelimVersion)
	if err != nil {
		t.Fatalf("NewDeterministicFormat: %v", err)
	}
	if f.Version() != DefaultContextDelimVersion {
		t.Fatalf("Version: got %d want %d", f.Version(), DefaultContextDelimVersion)
	}
	if f.OpenTag("task-context") != "<task-context>" || f.CloseTag("task-context") != "</task-context>" {
		t.Fatalf("default tag pair: open %q close %q", f.OpenTag("task-context"), f.CloseTag("task-context"))
	}
	custom, err := NewDelimitedFormat("CUSTOM", "[%s]", "[/%s]")
	if err != nil {
		t.Fatalf("NewDelimitedFormat: %v", err)
	}
	sec, err := custom.Section("task-context", "body")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	out := sec.Render()
	if !strings.HasPrefix(out, "[task-context]\n") || !strings.HasSuffix(out, "\n[/task-context]") {
		t.Fatalf("custom syntax not honored: %q", out)
	}
	if got, err := custom.Content("task-context", out); err != nil || got != "body" {
		t.Fatalf("custom round trip: %q err %v", got, err)
	}
	csec, _ := f.Section("task-context", "body")
	if out == csec.Render() {
		t.Fatal("different format versions must not render identically")
	}
	for _, bad := range []struct{ version, open, close string }{
		{"", "%s", "[/%s]"},
		{"V", "", "[/%s]"},
		{"V", "%s", ""},
		{"V", "no placeholder", "[/%s]"},
		{"V", "[%s]", "no placeholder"},
		{"V", "[%s]", "no placeholder"},
	} {
		if _, err := NewDelimitedFormat(bad.version, bad.open, bad.close); err == nil {
			t.Fatalf("NewDelimitedFormat(%q,%q,%q): expected rejection", bad.version, bad.open, bad.close)
		}
	}
	f2, _ := NewDeterministicFormat(DefaultContextDelimVersion)
	if _, err := f2.Section("", "body"); err == nil {
		t.Fatal("Section with empty label: expected rejection")
	}
	if _, err := f2.Section("task-context ", "body"); err == nil {
		t.Fatal("Section with non-canonical label: expected rejection")
	}
}
