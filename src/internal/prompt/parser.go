package prompt

import (
	"errors"
	"fmt"
	"strings"
)

// IR node kinds (WBS 8.6: deterministic internal representation).
const (
	NodeText     = 1
	NodeVariable = 2
)

// ErrUnterminatedExpression is returned by ParseTemplate when a "{{" has
// no matching "}}" before the source ends (WBS 8.6, T8.6.4).
var ErrUnterminatedExpression = errors.New("prompt: unterminated expression")

// ErrInvalidSyntax is returned by ParseTemplate for any other malformed
// template syntax (WBS 8.6, T8.6.2).
var ErrInvalidSyntax = errors.New("prompt: invalid syntax")

// IRNode is one element of the parsed template representation. Text nodes
// carry verbatim source text; variable nodes carry the namespace.field
// path. The source is never reconstructed or altered: a text run between
// two expressions keeps every byte, including newlines and unicode.
type IRNode struct {
	Kind int
	Text string
	Path string
}

// IsText reports whether n is a text node.
func (n IRNode) IsText() bool { return n.Kind == NodeText }

// IsVariable reports whether n is a variable node.
func (n IRNode) IsVariable() bool { return n.Kind == NodeVariable }

// TemplateIR is the deterministic parse result of a template source: an
// ordered, complete split of the source into text and variable nodes. The
// same source always yields a byte-identical IR (T8.6.1).
type TemplateIR struct {
	Nodes []IRNode
}

// String concatenates the nodes back into the original source: text verbatim,
// variables as "{{path}}". It is the round-trip check for parsing.
func (t *TemplateIR) String() string {
	var b strings.Builder
	for _, n := range t.Nodes {
		if n.IsVariable() {
			b.WriteString("{{" + n.Path + "}}")
		} else {
			b.WriteString(n.Text)
		}
	}
	return b.String()
}

// Variables lists the variable paths in source order.
func (t *TemplateIR) Variables() []string {
	out := make([]string, 0, len(t.Nodes))
	for _, n := range t.Nodes {
		if n.IsVariable() {
			out = append(out, n.Path)
		}
	}
	return out
}

// ParseTemplate parses template source into a deterministic IR (WBS 8.6).
// It is a pure function over the source string: no registry, no I/O, no
// global state (T8.6.5), and syntax errors are detected before rendering
// (T8.6.2, T8.6.4).
//
// Grammar: a template is text with "{{path}}" expressions. A path is
// "namespace.field": exactly one dot, a non-empty namespace of lowercase
// ASCII letters/digits/underscores, a dot, and a non-empty field of the
// same character set (e.g. "work_unit.name"). Any deviation is an
// ErrInvalidSyntax; a "{{" without a matching "}}" is an
// ErrUnterminatedExpression.
func ParseTemplate(source string) (*TemplateIR, error) {
	ir := &TemplateIR{}
	i := 0
	for i < len(source) {
		start := i
		open := strings.Index(source[i:], "{{")
		if open < 0 {
			appendText(ir, source[start:])
			return ir, nil
		}
		i = start + open
		// source[i:i+2] == "{{"
		close := strings.Index(source[i+2:], "}}")
		if close < 0 {
			return nil, fmt.Errorf("%w at offset %d: no \"}}\" before end of source", ErrUnterminatedExpression, i)
		}
		if i > start {
			appendText(ir, source[start:i])
		}
		path := source[i+2 : i+2+close]
		if err := checkPath(path); err != nil {
			return nil, fmt.Errorf("%w at offset %d: %q", ErrInvalidSyntax, i, err)
		}
		ir.Nodes = append(ir.Nodes, IRNode{Kind: NodeVariable, Path: path})
		i = i + 2 + close + 2
	}
	return ir, nil
}

func appendText(ir *TemplateIR, text string) {
	if text == "" {
		return
	}
	ir.Nodes = append(ir.Nodes, IRNode{Kind: NodeText, Text: text})
}

func checkPath(path string) error {
	if path == "" {
		return errors.New("empty expression")
	}
	for _, r := range path {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return fmt.Errorf("expression must not contain whitespace: %q", path)
		}
	}
	dot := strings.Index(path, ".")
	if dot < 0 {
		return fmt.Errorf("path %q must be namespace.field", path)
	}
	if strings.Index(path[dot+1:], ".") >= 0 {
		return fmt.Errorf("path %q has more than one dot", path)
	}
	ns, field := path[:dot], path[dot+1:]
	if !nameOK(ns) {
		return fmt.Errorf("namespace %q is not namespace characters", ns)
	}
	if !nameOK(field) {
		return fmt.Errorf("field %q is not name characters", field)
	}
	return nil
}

func nameOK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// CheckVariables reports the template's variable paths that are unknown to
// the registry (WBS 8.6, T8.6.3), in source order, deduplicated. The
// namespace set stays single-sourced in the P8-03 registry: a path is known
// only if it was explicitly registered. An empty result is a valid
// template.
func CheckVariables(ir *TemplateIR, reg *Registry) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ir.Variables() {
		if seen[p] {
			continue
		}
		seen[p] = true
		if !reg.Has(p) {
			out = append(out, p)
		}
	}
	return out
}
