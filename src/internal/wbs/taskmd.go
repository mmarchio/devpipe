package wbs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// State files. The team works in <working directory>/statefiles: the
// overseer loads each phase's WBS into statefiles/wbs, the ARCHITECT
// writes the phase plan to PLAN.md, the PLANNER breaks it into tasks in
// BACKLOG.md, the PROJECT_MANAGER moves the active task into
// ACTIVE_TASK.md (and finished tasks into TASK_ARCHIVE.md), the ENGINEER
// implements and the TESTER tests the active task. The overseer reads the
// files back to decide every transition.
const (
	StatefilesDir  = "statefiles"
	WBSDir         = "statefiles/wbs"
	PlanFile       = "statefiles/PLAN.md"
	BacklogFile    = "statefiles/BACKLOG.md"
	ActiveTaskFile = "statefiles/ACTIVE_TASK.md"
	ArchiveFile    = "statefiles/TASK_ARCHIVE.md"
	// PlanRejectedFile lists the problems of the last rejected plan; it
	// exists only until a plan is accepted.
	PlanRejectedFile = "statefiles/PLAN_REJECTED.md"
)

// WBSFile is the repository-relative file a phase's WBS is loaded into.
func WBSFile(phaseOrder int) string {
	return fmt.Sprintf("%s/phase-%02d.md", WBSDir, phaseOrder)
}

// Task statuses in the task files. Any other value is "not yet complete".
const (
	TaskStatusComplete = "COMPLETE" // the TESTER verified every acceptance criterion
	TaskStatusDone     = "DONE"     // the PROJECT_MANAGER accepted and archived it
)

// ErrMalformedTasks reports a task file that does not follow the task
// file format (docs/statefiles.md).
var ErrMalformedTasks = errors.New("wbs: malformed task file")

// Task is one task section of a task file.
type Task struct {
	Unit   CandidateUnit
	Status string // upper-cased; "" when the section has no Status line
	// Met marks each acceptance criterion checked ([x]).
	Met []bool
}

// AllMet reports whether every acceptance criterion is checked.
func (t Task) AllMet() bool {
	for _, m := range t.Met {
		if !m {
			return false
		}
	}
	return true
}

// TaskDoc is a parsed task file.
type TaskDoc struct {
	Summary string
	Tasks   []Task
}

// Find returns the task with key (nil when absent).
func (d *TaskDoc) Find(key string) *Task {
	for i := range d.Tasks {
		if d.Tasks[i].Unit.Key == key {
			return &d.Tasks[i]
		}
	}
	return nil
}

// Candidate returns the plan candidate of the file's tasks.
func (d *TaskDoc) Candidate() *PlanCandidate {
	c := &PlanCandidate{Summary: d.Summary}
	for _, t := range d.Tasks {
		c.Units = append(c.Units, t.Unit)
	}
	return c
}

var (
	taskHeadingRE = regexp.MustCompile(`^##\s+([A-Za-z0-9._-]+)\s*(?:—|–|-|:)\s*(.+?)\s*$`)
	checkboxRE    = regexp.MustCompile(`^\[( |x|X)\]\s+(.+)$`)
	checkRE       = regexp.MustCompile(`^\[(TEST|COMMAND|MANUAL)\]\s*(.+)$`)
	testsSuffixRE = regexp.MustCompile(`\s*\(tests?:\s*([^)]*)\)\s*$`)
	cmdSuffixRE   = regexp.MustCompile("\\s*`([^`]+)`\\s*$")
	fieldRE       = regexp.MustCompile(`^([A-Za-z][A-Za-z ]*):\s*(.*)$`)
)

// ParseTasksMarkdown parses a task file with at least one task into a
// plan candidate. Only the layout is checked here; the deterministic plan
// checks (structure, atomic policy, scope) run on the result.
func ParseTasksMarkdown(md string) (*PlanCandidate, error) {
	d, err := ParseTasks(md)
	if err != nil {
		return nil, err
	}
	if len(d.Tasks) == 0 {
		return nil, fmt.Errorf("%w: no tasks (\"## <KEY> — <name>\" sections or \"task: <KEY> — <name>\" lines)", ErrMalformedTasks)
	}
	return d.Candidate(), nil
}

// ParseTasks parses a task file in either layout: the key:value format
// when IsKeyValueTasks, the Markdown layout otherwise. Both are always
// accepted, so a project switching formats keeps reading its existing
// files. A file without tasks (e.g. an empty ACTIVE_TASK.md) has none.
func ParseTasks(content string) (*TaskDoc, error) {
	if IsKeyValueTasks(content) {
		return ParseTasksKeyValue(content)
	}
	return parseTasksMarkdown(content)
}

// parseTasksMarkdown parses a task file in the Markdown layout.
func parseTasksMarkdown(md string) (*TaskDoc, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	d := &TaskDoc{}
	var t *Task
	var section string
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
		objective, instructions = nil, nil
	}
	seenTitle := false
	for i, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# ") && !seenTitle && t == nil:
			seenTitle = true
			continue
		case strings.HasPrefix(trimmed, "## "):
			flush()
			m := taskHeadingRE.FindStringSubmatch(trimmed)
			if m == nil {
				fail(i, "task heading must be \"## <KEY> — <name>\": %q", trimmed)
				t = nil
				section = "skip"
				continue
			}
			t = &Task{Unit: CandidateUnit{Key: m[1], Name: m[2]}}
			section = "fields"
			continue
		case strings.HasPrefix(trimmed, "### "):
			if t == nil {
				fail(i, "section %q outside a task", trimmed)
				section = "skip"
				continue
			}
			switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))) {
			case "objective":
				section = "objective"
			case "instructions":
				section = "instructions"
			case "acceptance criteria":
				section = "acceptance"
			case "required tests":
				section = "tests"
			case "validation":
				section = "validation"
			default:
				fail(i, "unknown section %q", trimmed)
				section = "skip"
			}
			continue
		}
		if t == nil {
			if section != "skip" {
				summary = append(summary, line)
			}
			continue
		}
		u := &t.Unit
		switch section {
		case "fields":
			if trimmed == "" {
				continue
			}
			m := fieldRE.FindStringSubmatch(trimmed)
			if m == nil {
				fail(i, "%s: expected \"Field: value\", got %q", u.Key, trimmed)
				continue
			}
			val := strings.TrimSpace(m[2])
			switch strings.ToLower(strings.TrimSpace(m[1])) {
			case "status":
				t.Status = strings.ToUpper(val)
			case "kind":
				u.Kind = strings.ToUpper(val)
			case "depends on":
				if !strings.EqualFold(val, "none") && val != "" {
					u.DependsOn = splitList(val)
				}
			case "paths":
				if !strings.EqualFold(val, "none") && val != "" {
					u.Paths = splitList(val)
				}
			case "single invocation":
				switch strings.ToLower(val) {
				case "yes", "true":
					u.SingleInvocation = true
				case "no", "false":
				default:
					fail(i, "%s: Single invocation must be yes or no", u.Key)
				}
			default:
				fail(i, "%s: unknown field %q", u.Key, m[1])
			}
		case "objective":
			objective = append(objective, line)
		case "instructions":
			instructions = append(instructions, line)
		case "acceptance", "tests", "validation":
			if trimmed == "" {
				continue
			}
			item, ok := listItem(trimmed)
			if !ok {
				fail(i, "%s: expected a \"- \" list item, got %q", u.Key, trimmed)
				continue
			}
			switch section {
			case "acceptance":
				met := false
				if m := checkboxRE.FindStringSubmatch(item); m != nil {
					met, item = m[1] != " ", m[2]
				}
				ch, err := parseCheck(item)
				if err != nil {
					fail(i, "%s: %v", u.Key, err)
					continue
				}
				u.AcceptanceChecks = append(u.AcceptanceChecks, ch)
				t.Met = append(t.Met, met)
			case "tests":
				u.RequiredTests = append(u.RequiredTests, strings.Trim(item, "`"))
			case "validation":
				m := cmdSuffixRE.FindStringSubmatch(item)
				if m == nil {
					fail(i, "%s: validation command must be in backticks", u.Key)
					continue
				}
				v, err := commandSpec(m[1])
				if err != nil {
					fail(i, "%s: %v", u.Key, err)
					continue
				}
				u.Validation = append(u.Validation, v)
			}
		}
	}
	flush()
	d.Summary = strings.TrimSpace(strings.Join(summary, "\n"))
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMalformedTasks, strings.Join(errs, "; "))
	}
	return d, nil
}

func listItem(s string) (string, bool) {
	for _, p := range []string{"- ", "* "} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):]), true
		}
	}
	return "", false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.Trim(strings.TrimSpace(p), "`"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseCheck(item string) (AcceptanceCheckSpec, error) {
	m := checkRE.FindStringSubmatch(item)
	if m == nil {
		return AcceptanceCheckSpec{}, fmt.Errorf("acceptance criterion must start with [TEST], [COMMAND] or [MANUAL]: %q", item)
	}
	ch := AcceptanceCheckSpec{Verification: m[1], Text: strings.TrimSpace(m[2])}
	switch ch.Verification {
	case VerifyTest:
		if t := testsSuffixRE.FindStringSubmatch(ch.Text); t != nil {
			ch.Tests = splitList(t[1])
			ch.Text = strings.TrimSpace(ch.Text[:len(ch.Text)-len(t[0])])
		}
	case VerifyCommand:
		if c := cmdSuffixRE.FindStringSubmatch(ch.Text); c != nil {
			spec, err := commandSpec(c[1])
			if err != nil {
				return ch, err
			}
			ch.Command = &spec
			ch.Text = strings.TrimSpace(ch.Text[:len(ch.Text)-len(c[0])])
		}
	}
	return ch, nil
}

// commandSpec splits a command into program and arguments; single and
// double quotes group words. No other shell syntax is interpreted.
func commandSpec(s string) (ValidationSpec, error) {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return ValidationSpec{}, fmt.Errorf("unterminated quote in command %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return ValidationSpec{}, fmt.Errorf("empty command")
	}
	v := ValidationSpec{Program: words[0]}
	if len(words) > 1 {
		v.Args = words[1:]
	}
	return v, nil
}
