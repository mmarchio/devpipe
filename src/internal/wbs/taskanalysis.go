package wbs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// AnalysisFile is where the ARCHITECT writes a task analysis requested
// from the recovery screen ("analyze task <KEY>").
const AnalysisFile = "statefiles/TASK_ANALYSIS.md"

// ErrMalformedAnalysis reports a task analysis that does not follow the
// analysis format.
var ErrMalformedAnalysis = errors.New("wbs: malformed task analysis")

// TaskAnalysis is a parsed task analysis: whether the analysis found
// optimizations that fix the recovery reason, and what they are.
type TaskAnalysis struct {
	Task          string
	Fixable       bool
	Findings      []string
	Optimizations []string
}

var analysisFieldRE = regexp.MustCompile(`^([A-Za-z][A-Za-z_ ]*):\s*(.*)$`)

// ParseTaskAnalysis parses a task analysis in either layout.
//
// Markdown:
//
//	# Task analysis — <KEY>
//	Task: <KEY>
//	Fixable: yes | no
//
//	## Findings
//	- <finding>
//
//	## Optimizations
//	- <optimization>
//
// key:value: "title:", "task:", "fixable:" lines and one "finding:" /
// "optimization:" line per item.
//
// Fixable is true only when the analysis says yes and lists at least one
// optimization.
func ParseTaskAnalysis(content string) (*TaskAnalysis, error) {
	a := &TaskAnalysis{}
	fixable := ""
	section := ""
	kv := IsKeyValueTasks(content)
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !kv {
			switch {
			case strings.HasPrefix(line, "# "):
				continue
			case strings.HasPrefix(line, "## "):
				section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
				continue
			case section == "findings" || section == "optimizations":
				item, ok := listItem(line)
				if !ok {
					// Continuation of the previous item.
					if section == "findings" && len(a.Findings) > 0 {
						a.Findings[len(a.Findings)-1] += " " + line
					} else if section == "optimizations" && len(a.Optimizations) > 0 {
						a.Optimizations[len(a.Optimizations)-1] += " " + line
					}
					continue
				}
				if section == "findings" {
					a.Findings = append(a.Findings, item)
				} else {
					a.Optimizations = append(a.Optimizations, item)
				}
				continue
			}
		}
		m := analysisFieldRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := strings.TrimSpace(m[2])
		switch strings.ToLower(strings.TrimSpace(m[1])) {
		case "task":
			a.Task = val
		case "fixable":
			fixable = strings.ToLower(val)
		case "finding":
			a.Findings = append(a.Findings, val)
		case "optimization":
			a.Optimizations = append(a.Optimizations, val)
		}
	}
	switch fixable {
	case "yes", "true":
		a.Fixable = len(a.Optimizations) > 0
	case "no", "false":
	default:
		return nil, fmt.Errorf("%w: a \"Fixable: yes\" or \"Fixable: no\" line is required", ErrMalformedAnalysis)
	}
	if a.Task == "" {
		return nil, fmt.Errorf("%w: a \"Task: <KEY>\" line is required", ErrMalformedAnalysis)
	}
	return a, nil
}
