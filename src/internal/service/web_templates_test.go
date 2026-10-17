package service

import (
	"strings"
	"testing"
)

// P17-45 (WBS 17.49) — unknown variables are rejected (all reported);
// known component variables pass.
func TestP1745TemplateContentValidation(t *testing.T) {
	if issues := ValidateTemplateContent("# Objective\n{{work_unit.task}}\n{{response_protocol.schema}}"); len(issues) != 0 {
		t.Fatalf("valid template rejected: %v", issues)
	}
	issues := ValidateTemplateContent("{{work_unit.task}} {{project.secret_key}} {{os.env}}")
	if len(issues) != 2 || !strings.Contains(issues[0], "project.secret_key") || !strings.Contains(issues[1], "os.env") {
		t.Fatalf("issues: %v", issues)
	}
	if issues := ValidateTemplateContent("{{unterminated"); len(issues) != 1 || !strings.HasPrefix(issues[0], "SYNTAX") {
		t.Fatalf("syntax: %v", issues)
	}
}
