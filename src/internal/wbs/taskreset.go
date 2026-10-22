package wbs

import (
	"regexp"
	"strings"
)

var (
	mdCheckedRE = regexp.MustCompile(`^(\s*[-*]\s+)\[[xX]\]`)
	kvCheckedRE = regexp.MustCompile(`^(\s*criterion:\s*)\[[xX]\]`)
	mdStatusRE  = regexp.MustCompile(`^(\s*)[Ss]tatus:.*$`)
	kvStatusRE  = regexp.MustCompile(`^(\s*)status:.*$`)
)

// ResetTask returns a task file (either layout) with task key restarted
// from the beginning: its status set to TODO (a status line is added when
// it has none) and every acceptance criterion unchecked. Every other line
// is kept byte for byte. found is false when the file holds no such task.
func ResetTask(content, key string) (out string, found bool) {
	kv := IsKeyValueTasks(content)
	lines := strings.Split(content, "\n")
	res := make([]string, 0, len(lines)+1)
	in, hasStatus, headAt := false, false, -1
	finish := func() {
		if in && !hasStatus {
			status := "Status: TODO"
			if kv {
				status = "status: TODO"
			}
			res = append(res[:headAt+1], append([]string{status}, res[headAt+1:]...)...)
		}
		in = false
	}
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		var start bool
		var taskKey string
		if kv {
			if m := kvLineRE.FindStringSubmatch(trimmed); m != nil && m[1] == "task" {
				start = true
				if tm := kvTaskRE.FindStringSubmatch(strings.TrimSpace(m[2])); tm != nil {
					taskKey = tm[1]
				}
			}
		} else if strings.HasPrefix(trimmed, "## ") {
			start = true
			if m := taskHeadingRE.FindStringSubmatch(trimmed); m != nil {
				taskKey = m[1]
			}
		}
		if start {
			finish()
			if taskKey == key {
				in, found, hasStatus, headAt = true, true, false, len(res)
			}
			res = append(res, raw)
			continue
		}
		if in {
			switch {
			case kv && kvStatusRE.MatchString(line):
				raw, hasStatus = kvStatusRE.ReplaceAllString(line, "${1}status: TODO"), true
			case !kv && mdStatusRE.MatchString(line):
				raw, hasStatus = mdStatusRE.ReplaceAllString(line, "${1}Status: TODO"), true
			case kv:
				raw = kvCheckedRE.ReplaceAllString(raw, "${1}[ ]")
			default:
				raw = mdCheckedRE.ReplaceAllString(raw, "${1}[ ]")
			}
		}
		res = append(res, raw)
	}
	finish()
	return strings.Join(res, "\n"), found
}
