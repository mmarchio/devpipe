package service

import (
	"strings"
	"testing"
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
	s := OutputFormatSkill("ComfyUI Environment Builder")
	for _, want := range []string{
		"---\nname: comfyui-environment-builder-output-format\ndescription: ",
		"\n---\n\n# ComfyUI Environment Builder output format\n",
		returnFormat,
		TaskFileFormat(),
		"## Before you reply",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("skill lacks %q:\n%s", want, s)
		}
	}
}
