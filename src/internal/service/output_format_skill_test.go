package service

import (
	"strings"
	"testing"

	"overseer/internal/worker"
)

// The skill is named after the project ("<slug>-output-format") and
// carries the reply rule and the full task file format.
func TestOutputFormatSkill(t *testing.T) {
	for name, want := range map[string]string{
		"ComfyUI Environment Builder": "comfyui-environment-builder-output-format",
		"  my_app v2!! ":              "my-app-v2-output-format",
		"***":                         "project-output-format",
	} {
		if got := OutputFormatSkillName(name); got != want {
			t.Errorf("OutputFormatSkillName(%q) = %q, want %q", name, got, want)
		}
	}
	s := OutputFormatSkill("ComfyUI Environment Builder", worker.FormatMarkdown)
	for _, want := range []string{
		"---\nname: comfyui-environment-builder-output-format\ndescription: ",
		"\n---\n\n# ComfyUI Environment Builder output format\n",
		returnFormat,
		TaskFileFormat(worker.FormatMarkdown),
		"## Before you reply",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("skill lacks %q:\n%s", want, s)
		}
	}
}

// The Replies section and checklist follow the project's response format.
func TestOutputFormatSkillResponseFormat(t *testing.T) {
	for f, want := range map[worker.ResponseFormat][]string{
		worker.FormatMarkdown: {"Format every reply as Markdown", "## Blockers", "formatted as Markdown"},
		worker.FormatXML:      {"Format every reply as XML", "<reply>", "<summary>", "formatted as XML"},
		worker.FormatKeyValue: {"Format every reply as `key: value` lines", "summary: ", "blocker: none", "formatted as key:value", GrammarFile},
	} {
		s := OutputFormatSkill("P", f)
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("%s skill lacks %q:\n%s", f, w, s)
			}
		}
		if strings.Contains(s, "plain-text") {
			t.Errorf("%s skill still asks for a plain-text reply", f)
		}
	}
}
