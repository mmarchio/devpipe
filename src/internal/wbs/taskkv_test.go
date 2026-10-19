package wbs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// sampleTasksKV is sampleTasks in the key:value format.
const sampleTasksKV = `title: Tasks — Phase 2: Storage
summary: Split the storage layer into primitives first.

task: T1 — Key codec
status: ACTIVE
kind: ATOMIC
depends_on: none
paths: internal/store/codec.go, internal/store/codec_test.go
single_invocation: yes
objective: Encode and decode keys.
instructions: Use big-endian ordering.
criterion: [x] [TEST] Keys round-trip (tests: TestCodecRoundTrip, TestCodecOrder)
criterion: [ ] [COMMAND] Package vets clean ` + "`go vet ./internal/store/...`" + `
criterion: [MANUAL] Doc comment explains ordering
required_test: TestCodecRoundTrip
validation: ` + "`go test -run \"Codec Order\" ./internal/store/`" + `

task: T2: Store composition
kind: composition
depends_on: T1
single_invocation: yes
objective: Compose the store.
criterion: [TEST] Store works (tests: TestStore)
`

// Both layouts describe the same tasks: the key:value file parses to
// exactly the TaskDoc of its Markdown twin.
func TestParseTasksKeyValueMatchesMarkdown(t *testing.T) {
	if IsKeyValueTasks(sampleTasks) || !IsKeyValueTasks(sampleTasksKV) || !IsKeyValueTasks("\n\ntask: A — x\n") || IsKeyValueTasks("") {
		t.Fatal("format detection")
	}
	md, err := ParseTasks(sampleTasks)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := ParseTasks(sampleTasksKV)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(md, kv) {
		t.Fatalf("key:value parse differs from Markdown:\n md %+v\n kv %+v", md, kv)
	}
	if c, err := ParseTasksMarkdown(sampleTasksKV); err != nil || len(c.Units) != 2 {
		t.Fatalf("plan candidate from key:value: %+v %v", c, err)
	}
}

// Multi-line objective and instructions repeat their key; an empty file
// or a title-only file has no tasks.
func TestParseTasksKeyValueRepeatedAndEmpty(t *testing.T) {
	d, err := ParseTasksKeyValue("task: A — a\nobjective: one\nobjective: two\ninstructions: x\ninstructions: y\n")
	if err != nil || d.Tasks[0].Unit.Objective != "one\ntwo" || d.Tasks[0].Unit.Instructions != "x\ny" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, s := range []string{"", "title: Active task\n"} {
		if d, err := ParseTasks(s); err != nil || len(d.Tasks) != 0 {
			t.Fatalf("%q: %+v %v", s, d, err)
		}
	}
}

// Malformed key:value files are rejected with line numbers.
func TestParseTasksKeyValueErrors(t *testing.T) {
	for in, want := range map[string]string{
		"task: A — a\nStatus: DONE\n":             "line 2: expected \"key: value\"",
		"title: x\nstatus: TODO\n":                "line 2: status: line outside a task",
		"task: A — a\ncolour: red\n":              "line 2: A: unknown key \"colour\"",
		"task: A — a\nsingle_invocation: maybe\n": "single_invocation must be yes or no",
		"task: A — a\nvalidation: go test\n":      "validation command must be in backticks",
		"task: A — a\ncriterion: [ ] works\n":     "acceptance criterion must start with",
		"task: bad key here\n":                    "task line must be",
		"title: a\ntitle: b\n":                    "line 2: second title line",
	} {
		_, err := ParseTasks(in)
		if !errors.Is(err, ErrMalformedTasks) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
}
