package continuation

import (
	"strconv"
	"strings"
)

// RenderContinuationState renders the derived CONTINUATION_STATE document
// (WBS 14.73) from persisted values only; it carries no authority — the
// database row is the source of truth.
func RenderContinuationState(c *Continuation) string {
	var b strings.Builder
	b.WriteString("# CONTINUATION_STATE\n\n")
	b.WriteString("Derived from the overseer database; edits to this document have no effect.\n\n")
	b.WriteString("Work Unit: " + c.WorkUnitID.String() + "\n")
	b.WriteString("Attempt: " + strconv.Itoa(c.Attempt) + "\n")
	b.WriteString("Status: " + string(c.Status) + "\n")
	b.WriteString("Strategy: " + c.Strategy.Label() + "\n\n")
	summary := c.Captured.Summary
	if c.StatusUnknown {
		summary = "(no accepted worker report — progress unknown)"
	}
	b.WriteString("## Last Summary\n" + orNone(summary) + "\n\n")
	writeList(&b, "Completed", c.Captured.Completed)
	writeList(&b, "Remaining", c.Captured.Remaining)
	writeList(&b, "Blocking", c.Captured.Blocking)
	b.WriteString("## Next Action (worker-reported, advisory)\n" + orNone(c.Captured.NextAction) + "\n")
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

func writeList(b *strings.Builder, title string, items []string) {
	b.WriteString("## " + title + "\n")
	if len(items) == 0 {
		b.WriteString("None\n\n")
		return
	}
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
	b.WriteString("\n")
}
