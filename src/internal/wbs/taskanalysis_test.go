package wbs

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseTaskAnalysis(t *testing.T) {
	md := "# Task analysis — 4.2.10\n\nTask: 4.2.10\nFixable: yes\n\n## Findings\n- go test ./... fails: TestT17 uses chmod,\n  which root ignores.\n- The criterion is vague.\n\n" +
		"## Optimizations\n- Mock the permission error in TestT17.\n- Scope validation to Test_4_2_10.\n"
	kv := "title: Task analysis — 4.2.10\ntask: 4.2.10\nfixable: yes\nfinding: go test ./... fails: TestT17 uses chmod, which root ignores.\n" +
		"finding: The criterion is vague.\noptimization: Mock the permission error in TestT17.\noptimization: Scope validation to Test_4_2_10.\n"
	want := &TaskAnalysis{Task: "4.2.10", Fixable: true,
		Findings:      []string{"go test ./... fails: TestT17 uses chmod, which root ignores.", "The criterion is vague."},
		Optimizations: []string{"Mock the permission error in TestT17.", "Scope validation to Test_4_2_10."}}
	for name, in := range map[string]string{"markdown": md, "key:value": kv} {
		got, err := ParseTaskAnalysis(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	// "yes" without optimizations, and "no", are not fixable.
	for _, in := range []string{"Task: A\nFixable: yes\n\n## Findings\n- nothing to change\n", "Task: A\nFixable: no\n\n## Optimizations\n- x\n"} {
		if a, err := ParseTaskAnalysis(in); err != nil || a.Fixable {
			t.Fatalf("%q: %+v %v", in, a, err)
		}
	}
	for _, in := range []string{"Task: A\n", "Fixable: yes\n## Optimizations\n- x\n", ""} {
		if _, err := ParseTaskAnalysis(in); !errors.Is(err, ErrMalformedAnalysis) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}
