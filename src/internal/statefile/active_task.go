package statefile

import (
	"fmt"
	"strings"

	"overseer/internal/wbs"
)

// RenderActiveTask renders the ACTIVE_TASK content for one work unit (WBS
// 7.11, T7.11.1-T7.11.6): a deterministic markdown document carrying the
// unit id, its objective, and its acceptance criteria, required tests, and
// constraints. It is a PURE function of the unit — the derivation service
// (P7-10) is the only writer of this content — and a nil unit renders the
// empty string (no unit, nothing to derive).
func RenderActiveTask(u *wbs.WorkUnit) string {
	if u == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Active Task\n\n")
	b.WriteString("## Work Unit\n")
	fmt.Fprintf(&b, "%s\n\n", u.ID)
	b.WriteString("## Objective\n")
	if spec := u.Specification; spec != nil {
		b.WriteString(spec.Objective)
	}
	b.WriteString("\n")
	if spec := u.Specification; spec != nil {
		if len(spec.AcceptanceCriteria) > 0 {
			b.WriteString("\n## Acceptance Criteria\n")
			writeList(&b, spec.AcceptanceCriteria)
		}
		if len(spec.RequiredTests) > 0 {
			b.WriteString("\n## Required Tests\n")
			writeList(&b, spec.RequiredTests)
		}
		if len(spec.Constraints) > 0 {
			b.WriteString("\n## Constraints\n")
			writeList(&b, spec.Constraints)
		}
	}
	return b.String()
}

func writeList(b *strings.Builder, items []string) {
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}
