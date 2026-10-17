package wbs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const sampleTasks = `# Tasks — Phase 2: Storage

Split the storage layer into primitives first.

## T1 — Key codec
Status: ACTIVE
Kind: ATOMIC
Depends on: none
Paths: internal/store/codec.go, internal/store/codec_test.go
Single invocation: yes

### Objective
Encode and decode keys.

### Instructions
Use big-endian ordering.

### Acceptance criteria
- [x] [TEST] Keys round-trip (tests: TestCodecRoundTrip, TestCodecOrder)
- [ ] [COMMAND] Package vets clean ` + "`go vet ./internal/store/...`" + `
- [MANUAL] Doc comment explains ordering

### Required tests
- TestCodecRoundTrip

### Validation
- ` + "`go test -run \"Codec Order\" ./internal/store/`" + `

## T2: Store composition
Kind: composition
Depends on: T1
Single invocation: yes

### Objective
Compose the store.

### Acceptance criteria
- [TEST] Store works (tests: TestStore)
`

func TestParseTasksMarkdown(t *testing.T) {
	c, err := ParseTasksMarkdown(sampleTasks)
	if err != nil {
		t.Fatal(err)
	}
	if c.Summary != "Split the storage layer into primitives first." || len(c.Units) != 2 {
		t.Fatalf("summary/units: %+v", c)
	}
	u := c.Units[0]
	want := CandidateUnit{Key: "T1", Name: "Key codec", Kind: "ATOMIC", Objective: "Encode and decode keys.", Instructions: "Use big-endian ordering.",
		AcceptanceChecks: []AcceptanceCheckSpec{
			{Text: "Keys round-trip", Verification: VerifyTest, Tests: []string{"TestCodecRoundTrip", "TestCodecOrder"}},
			{Text: "Package vets clean", Verification: VerifyCommand, Command: &ValidationSpec{Program: "go", Args: []string{"vet", "./internal/store/..."}}},
			{Text: "Doc comment explains ordering", Verification: VerifyManual},
		},
		RequiredTests:    []string{"TestCodecRoundTrip"},
		Validation:       []ValidationSpec{{Program: "go", Args: []string{"test", "-run", "Codec Order", "./internal/store/"}}},
		Paths:            []string{"internal/store/codec.go", "internal/store/codec_test.go"},
		SingleInvocation: true}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("unit\n got %+v\nwant %+v", u, want)
	}
	if v := c.Units[1]; v.Key != "T2" || v.Kind != "COMPOSITION" || !reflect.DeepEqual(v.DependsOn, []string{"T1"}) {
		t.Fatalf("second unit %+v", v)
	}
	// The parsed candidate passes the deterministic plan checks.
	if p := ValidatePlanStructure(c); len(p) > 0 {
		t.Fatal(p)
	}
	if p := ValidateAtomicPolicy(c, DefaultAtomicPolicy()); len(p) > 0 {
		t.Fatal(p)
	}
}

func TestParseTasksMarkdownRejects(t *testing.T) {
	for name, md := range map[string]string{
		"bad heading":    "## no separator here\n",
		"unknown field":  "## T1 — x\nOwner: me\n",
		"unknown sect":   "## T1 — x\n### Notes\n",
		"bad check":      "## T1 — x\n### Acceptance criteria\n- works\n",
		"no backticks":   "## T1 — x\n### Validation\n- go test ./...\n",
		"open quote":     "## T1 — x\n### Validation\n- `go test \"x`\n",
		"single invoc":   "## T1 — x\nSingle invocation: maybe\n",
		"not list item":  "## T1 — x\n### Required tests\nTestX\n",
		"orphan section": "### Objective\nx\n",
	} {
		if _, err := ParseTasksMarkdown(md); !errors.Is(err, ErrMalformedTasks) {
			t.Errorf("%s: want ErrMalformedTasks, got %v", name, err)
		}
	}
	_, err := ParseTasksMarkdown("## T1 — x\nOwner: me\n### Notes\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("every problem reported with its line: %v", err)
	}
}

func TestParseTasksStatusAndChecks(t *testing.T) {
	d, err := ParseTasks(sampleTasks)
	if err != nil {
		t.Fatal(err)
	}
	t1 := d.Find("T1")
	if t1 == nil || t1.Status != "ACTIVE" || !reflect.DeepEqual(t1.Met, []bool{true, false, false}) || t1.AllMet() {
		t.Fatalf("T1 %+v", t1)
	}
	if t2 := d.Find("T2"); t2 == nil || t2.Status != "" || !reflect.DeepEqual(t2.Met, []bool{false}) {
		t.Fatalf("T2 %+v", t2)
	}
	// A file without task sections (an empty ACTIVE_TASK.md) has no tasks.
	empty, err := ParseTasks("# Active task\n\nNo active task.\n")
	if err != nil || len(empty.Tasks) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	if _, err := ParseTasksMarkdown("# Backlog\n"); !errors.Is(err, ErrMalformedTasks) {
		t.Fatalf("empty backlog accepted: %v", err)
	}
}

func TestStatefilePaths(t *testing.T) {
	if WBSFile(3) != "statefiles/wbs/phase-03.md" || BacklogFile != "statefiles/BACKLOG.md" || ActiveTaskFile != "statefiles/ACTIVE_TASK.md" {
		t.Fatal("paths")
	}
}
