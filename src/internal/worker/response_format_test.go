package worker

import (
	"regexp"
	"strings"
	"testing"
)

// Only key:value has a grammar; it defines every rule it references and
// starts at root.
func TestResponseFormatGrammar(t *testing.T) {
	if FormatMarkdown.Grammar() != "" || FormatXML.Grammar() != "" || ResponseFormat("").Grammar() != "" {
		t.Fatal("only key:value has a grammar")
	}
	g := FormatKeyValue.Grammar()
	if g != KeyValueGrammar || !strings.Contains(g, "\nroot ") {
		t.Fatalf("key:value grammar:\n%s", g)
	}
	defined := map[string]bool{}
	var bodies []string
	for _, line := range strings.Split(g, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, body, ok := strings.Cut(line, "::=")
		if !ok {
			t.Fatalf("not a rule: %q", line)
		}
		defined[strings.TrimSpace(name)] = true
		bodies = append(bodies, body)
	}
	// Rule references are the bare identifiers outside quotes and classes.
	strip := regexp.MustCompile(`"(\\.|[^"])*"|\[(\\.|[^\]])*\]`)
	for _, b := range bodies {
		for _, ref := range regexp.MustCompile(`[a-z][a-z0-9-]*`).FindAllString(strip.ReplaceAllString(b, " "), -1) {
			if !defined[ref] {
				t.Errorf("rule %q is referenced but not defined", ref)
			}
		}
	}
	// The documented reply shape matches the grammar's structure.
	shape := regexp.MustCompile(`^summary: [^\n]+\n(blocker: [^\n]+\n)+$`)
	if !shape.MatchString("summary: Implemented the parser.\nblocker: none\n") {
		t.Fatal("example reply does not match the grammar's shape")
	}
}
