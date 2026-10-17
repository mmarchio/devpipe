package prompt

import (
	"errors"
	"reflect"
	"testing"
)

// t86ValidTemplate is a valid multiline template covering text, variables,
// unicode, and a literal single '{' run.
const t86ValidTemplate = "# Objective\n{{work_unit.objective}}\n\n# Instructions\n{{state.instructions}}\n\n# Context (Unicode: über)\n{{project.name}}\n\nLiteral: { not an expression"

// T8.6.1 — A valid template parses to a deterministic IR.
func TestT861ValidTemplateDeterministicIR(t *testing.T) {
	ir1, err := ParseTemplate(t86ValidTemplate)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantNodes := []IRNode{
		{Kind: NodeText, Text: "# Objective\n"},
		{Kind: NodeVariable, Path: "work_unit.objective"},
		{Kind: NodeText, Text: "\n\n# Instructions\n"},
		{Kind: NodeVariable, Path: "state.instructions"},
		{Kind: NodeText, Text: "\n\n# Context (Unicode: über)\n"},
		{Kind: NodeVariable, Path: "project.name"},
		{Kind: NodeText, Text: "\n\nLiteral: { not an expression"},
	}
	if !reflect.DeepEqual(ir1.Nodes, wantNodes) {
		t.Fatalf("IR = %+v, want %+v", ir1.Nodes, wantNodes)
	}
	// Round-trip: the IR reconstructs the source byte-for-byte.
	if got := ir1.String(); got != t86ValidTemplate {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, t86ValidTemplate)
	}
	if vars := ir1.Variables(); !reflect.DeepEqual(vars, []string{"work_unit.objective", "state.instructions", "project.name"}) {
		t.Fatalf("Variables() = %v", vars)
	}
	// Determinism: repeated parses of the same source are byte-identical.
	for i := 0; i < 3; i++ {
		ir2, err := ParseTemplate(t86ValidTemplate)
		if err != nil {
			t.Fatalf("reparsing: %v", err)
		}
		if !reflect.DeepEqual(ir1, ir2) {
			t.Fatalf("parse %d differs: %+v vs %+v", i, ir1, ir2)
		}
	}
}

// T8.6.2 — Invalid syntax is rejected with ErrInvalidSyntax.
func TestT862InvalidSyntaxRejected(t *testing.T) {
	for _, src := range []string{
		"{{}}",               // empty expression
		"{{project}}",        // no dot
		"{{project.}}",       // empty field
		"{{.name}}",          // empty namespace
		"{{a.b.c}}",          // more than one dot
		"{{project name}}",   // space in expression
		"{{a b}}",            // whitespace in expression
		"{{ project.name }}", // padding
		"{{project.Name}}",   // uppercase namespace
		"{{Project.name}}",   // uppercase namespace
	} {
		if _, err := ParseTemplate(src); !errors.Is(err, ErrInvalidSyntax) {
			t.Fatalf("source %q: err = %v, want ErrInvalidSyntax", src, err)
		}
	}
}

// T8.6.3 — Unknown variables are detected against the registry.
func TestT863UnknownVariableDetected(t *testing.T) {
	ir, err := ParseTemplate("{{project.name}} and {{work_unit.objective}} and {{state.instructions}} and {{work_unit.objective}}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register("project.name", "Overseer"); err != nil {
		t.Fatalf("register: %v", err)
	}
	unknown := CheckVariables(ir, reg)
	if !reflect.DeepEqual(unknown, []string{"work_unit.objective", "state.instructions"}) {
		t.Fatalf("CheckVariables = %v, want [work_unit.objective state.instructions]", unknown)
	}
	// Registering everything clears the finding; dedup keeps source order.
	for _, p := range []string{"work_unit.objective", "state.instructions"} {
		if err := reg.Register(p, "x"); err != nil {
			t.Fatalf("register %q: %v", p, err)
		}
	}
	if got := CheckVariables(ir, reg); len(got) != 0 {
		t.Fatalf("CheckVariables after registering all = %v, want empty", got)
	}
}

// T8.6.4 — Unterminated expressions are detected.
func TestT864UnterminatedExpressionDetected(t *testing.T) {
	for _, src := range []string{
		"prefix {{project.name",
		"{{project.name",
		"{{",
		"text {{a.b}} trailing {{c.d",
	} {
		if _, err := ParseTemplate(src); !errors.Is(err, ErrUnterminatedExpression) {
			t.Fatalf("source %q: err = %v, want ErrUnterminatedExpression", src, err)
		}
	}
}

// T8.6.5 — The parser has no external side effects: a pure function over the
// source string. Repeated parses never observe prior parses (no global
// state) and the shared registry namespace set is untouched.
func TestT865ParserNoSideEffects(t *testing.T) {
	nsBefore := append([]VariableNamespace(nil), AllNamespaces...)
	for i := 0; i < 5; i++ {
		_, err := ParseTemplate(t86ValidTemplate)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
	}
	if !reflect.DeepEqual(AllNamespaces, nsBefore) {
		t.Fatalf("AllNamespaces mutated by ParseTemplate: %v", AllNamespaces)
	}
	if _, err := ParseTemplate("{{project.name}}"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := ParseTemplate("{{ unterminated"); !errors.Is(err, ErrUnterminatedExpression) {
		t.Fatalf("later parse misbehaves after prior parse: %v", err)
	}
	reg := NewRegistry()
	_ = reg.Register("project.name", "Overseer")
	ir, err := ParseTemplate("{{project.name}}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := CheckVariables(ir, reg); len(got) != 0 {
		t.Fatalf("CheckVariables = %v, want empty", got)
	}
}
