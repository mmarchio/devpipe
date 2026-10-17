package statefile

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"overseer/internal/wbs"
)

// RenderTaskArchive renders the TASK_ARCHIVE content (WBS 7.13): the
// project's COMPLETED work units (status DONE) as a deterministic markdown
// document, each entry carrying the unit id, name, completion status, and
// completion timestamp. Pending / non-DONE units are excluded (T7.13.2) and
// the content is a PURE function of the unit records — the database
// remains the authoritative history (T7.13.5), the rendering never writes
// back.
//
// Ordering rule (T7.13.3, stated per the task NOTES): ascending
// completed_at — the completion timestamp set once by the unit lifecycle —
// tie-broken by ascending (sequence, id) so the output is byte-stable when
// completion timestamps coincide (T7.13.4). A DONE unit with a nil
// completed_at sorts last.
func RenderTaskArchive(units []*wbs.WorkUnit) string {
	completed := make([]*wbs.WorkUnit, 0, len(units))
	for _, u := range units {
		if u != nil && u.Status == wbs.WorkUnitStatusDONE {
			completed = append(completed, u)
		}
	}
	sort.SliceStable(completed, func(i, j int) bool {
		a, b := completed[i], completed[j]
		aT, aHas := a.CompletedAt, a.CompletedAt != nil
		bT, bHas := b.CompletedAt, b.CompletedAt != nil
		switch {
		case aHas && bHas:
			if !aT.Equal(*bT) {
				return aT.Before(*bT)
			}
		case aHas != bHas:
			return aHas // a nil completed_at sorts last
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.ID.String() < b.ID.String()
	})

	var b strings.Builder
	b.WriteString("# Task Archive\n\n")
	if len(completed) == 0 {
		return b.String()
	}
	for _, u := range completed {
		fmt.Fprintf(&b, "## Work Unit\n")
		fmt.Fprintf(&b, "%s\n", u.ID)
		if u.Name != "" {
			fmt.Fprintf(&b, "Name: %s\n", u.Name)
		}
		b.WriteString("Status: DONE\n")
		if u.CompletedAt != nil {
			fmt.Fprintf(&b, "Completed: %s\n", u.CompletedAt.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	return b.String()
}
