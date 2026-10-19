package wbs

import (
	"fmt"
	"regexp"
	"strings"
)

// Task files in the key:value format (projects whose worker response
// format is key:value): one "key: value" line per field, list fields
// repeated, a "task:" line starting each task. Blank lines are ignored;
// anything else is rejected with its line number.
//
//	title: <title>
//	summary: <optional summary line>
//
//	task: <KEY> — <task name>
//	status: TODO | ACTIVE | COMPLETE | DONE
//	kind: ATOMIC | COMPOSITION | INTEGRATION | VALIDATION
//	depends_on: none | <KEY>, <KEY>
//	paths: <relative path>, <relative path>
//	single_invocation: yes | no
//	objective: <what the task delivers>
//	instructions: <optional notes>
//	criterion: [ ] [TEST] <criterion> (tests: TestName, TestOther)
//	criterion: [ ] [COMMAND] <criterion> `program arg arg`
//	criterion: [ ] [MANUAL] <criterion>
//	required_test: <TestName>
//	validation: `program arg arg`
//
// Repeated objective, instructions and summary lines are joined with
// newlines. The result is the same TaskDoc the Markdown layout produces.

var (
	kvLineRE    = regexp.MustCompile(`^([a-z][a-z_]*):(?:\s(.*))?$`)
	kvTaskRE    = regexp.MustCompile(`^([A-Za-z0-9._-]+)\s*(?:—|–|-|:)\s*(.+?)\s*$`)
	kvDetectRE  = regexp.MustCompile(`^[a-z][a-z_]*:(\s|$)`)
	kvTaskField = map[string]bool{"status": true, "kind": true, "depends_on": true, "paths": true, "single_invocation": true,
		"objective": true, "instructions": true, "criterion": true, "required_test": true, "validation": true}
)

// IsKeyValueTasks reports whether a task file is in the key:value format:
// its first non-blank line is a lower-case "key:" line. Markdown task
// files start with a "# " title or a "## " task heading.
func IsKeyValueTasks(content string) bool {
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			return kvDetectRE.MatchString(line)
		}
	}
	return false
}

// ParseTasksKeyValue parses a task file in the key:value format. A file
// without task: lines (e.g. an empty ACTIVE_TASK.md) has no tasks.
func ParseTasksKeyValue(content string) (*TaskDoc, error) {
	d := &TaskDoc{}
	var t *Task
	var summary, objective, instructions []string
	var errs []string
	fail := func(i int, format string, a ...any) {
		errs = append(errs, fmt.Sprintf("line %d: %s", i+1, fmt.Sprintf(format, a...)))
	}
	flush := func() {
		if t == nil {
			return
		}
		t.Unit.Objective = strings.TrimSpace(strings.Join(objective, "\n"))
		t.Unit.Instructions = strings.TrimSpace(strings.Join(instructions, "\n"))
		d.Tasks = append(d.Tasks, *t)
		t, objective, instructions = nil, nil, nil
	}
	seenTitle := false
	for i, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		m := kvLineRE.FindStringSubmatch(line)
		if m == nil {
			fail(i, "expected \"key: value\", got %q", line)
			continue
		}
		key, val := m[1], strings.TrimSpace(m[2])
		switch {
		case key == "task":
			flush()
			tm := kvTaskRE.FindStringSubmatch(val)
			if tm == nil {
				fail(i, "task line must be \"task: <KEY> — <name>\": %q", line)
				continue
			}
			t = &Task{Unit: CandidateUnit{Key: tm[1], Name: tm[2]}}
			continue
		case t == nil:
			switch key {
			case "title":
				if seenTitle {
					fail(i, "second title line")
				}
				seenTitle = true
			case "summary":
				summary = append(summary, val)
			default:
				if kvTaskField[key] {
					fail(i, "%s: line outside a task (a task starts with \"task: <KEY> — <name>\")", key)
				} else {
					fail(i, "unknown key %q", key)
				}
			}
			continue
		}
		u := &t.Unit
		switch key {
		case "status":
			t.Status = strings.ToUpper(val)
		case "kind":
			u.Kind = strings.ToUpper(val)
		case "depends_on":
			if !strings.EqualFold(val, "none") && val != "" {
				u.DependsOn = splitList(val)
			}
		case "paths":
			if !strings.EqualFold(val, "none") && val != "" {
				u.Paths = splitList(val)
			}
		case "single_invocation":
			switch strings.ToLower(val) {
			case "yes", "true":
				u.SingleInvocation = true
			case "no", "false":
			default:
				fail(i, "%s: single_invocation must be yes or no", u.Key)
			}
		case "objective":
			objective = append(objective, val)
		case "instructions":
			instructions = append(instructions, val)
		case "criterion":
			met := false
			item := val
			if cm := checkboxRE.FindStringSubmatch(item); cm != nil {
				met, item = cm[1] != " ", cm[2]
			}
			ch, err := parseCheck(item)
			if err != nil {
				fail(i, "%s: %v", u.Key, err)
				continue
			}
			u.AcceptanceChecks = append(u.AcceptanceChecks, ch)
			t.Met = append(t.Met, met)
		case "required_test":
			if val != "" {
				u.RequiredTests = append(u.RequiredTests, strings.Trim(val, "`"))
			}
		case "validation":
			cm := cmdSuffixRE.FindStringSubmatch(val)
			if cm == nil {
				fail(i, "%s: validation command must be in backticks", u.Key)
				continue
			}
			v, err := commandSpec(cm[1])
			if err != nil {
				fail(i, "%s: %v", u.Key, err)
				continue
			}
			u.Validation = append(u.Validation, v)
		default:
			fail(i, "%s: unknown key %q", u.Key, key)
		}
	}
	flush()
	d.Summary = strings.TrimSpace(strings.Join(summary, "\n"))
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMalformedTasks, strings.Join(errs, "; "))
	}
	return d, nil
}
