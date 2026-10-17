package workerresult

import "strings"

// InstructionName is the global response protocol carrying these
// instructions (PLAN Decision 8).
const InstructionName = "worker-result-protocol"

// Instructions renders the reusable worker response-protocol instructions
// for a protocol version (WBS 13.7): version, statuses and their meaning,
// required fields, JSON-only, and honesty about work and tests. The text
// is deterministic; its version/hash enter the prompt identity.
func Instructions(v ProtocolVersion) string {
	var b strings.Builder
	b.WriteString("Return your final result using the worker response protocol " + v.String() + ".\n")
	b.WriteString("Respond with exactly one JSON object and nothing else: no explanatory text, no markdown outside the object.\n\n")
	b.WriteString("Required fields:\n")
	b.WriteString(`  "protocol_version": "` + v.String() + `"` + "\n")
	b.WriteString(`  "status": one of "complete", "incomplete", "blocked", "failed"` + "\n")
	b.WriteString(`  "summary": non-empty string` + "\n")
	b.WriteString(`  "completed": array of strings (work actually performed)` + "\n")
	b.WriteString(`  "remaining": array of strings (requested work not yet done)` + "\n")
	b.WriteString(`  "blocking": array of strings (conditions preventing completion)` + "\n")
	b.WriteString(`  "next_action": string (your suggestion; advisory only)` + "\n\n")
	b.WriteString("Optional fields:\n")
	b.WriteString(`  "tests": {"reported_run": bool, "reported_passed": bool or null, "summary": string}` + "\n")
	b.WriteString(`  "artifacts": [{"path": repository-relative path, "action": "created"|"modified"|"deleted"|"renamed", "previous_path": for renamed}]` + "\n")
	b.WriteString(`  "acceptance_claims": [{"id": criterion id, "status": "satisfied"|"unsatisfied"|"not_checked", "evidence": string}]` + "\n\n")
	b.WriteString("Status meaning:\n")
	b.WriteString("  complete   = you performed the requested scope; remaining and blocking must be empty.\n")
	b.WriteString("  incomplete = requested scope remains; list it in remaining and give a next_action.\n")
	b.WriteString("  blocked    = continuing requires resolving an external or dependency condition; list it in blocking.\n")
	b.WriteString("  failed     = this execution attempt failed and cannot meaningfully continue; explain in summary.\n")
	b.WriteString("None of these statuses means the work was validated or accepted; the overseer decides that.\n\n")
	b.WriteString("Report only work actually performed.\n")
	b.WriteString("Do not report tests as passing unless they were actually executed and passed.\n")
	b.WriteString("Do not include project, phase, work-unit, state or role identifiers; the overseer already knows them.\n")
	return b.String()
}

// AcceptanceIDs extracts criterion ids from a work unit's acceptance
// criteria ("AC-1: text" → "AC-1"; a criterion without an id prefix is
// its own trimmed text).
func AcceptanceIDs(criteria []string) []string {
	out := make([]string, 0, len(criteria))
	for _, c := range criteria {
		c = strings.TrimSpace(c)
		if i := strings.Index(c, ":"); i > 0 && !strings.ContainsAny(c[:i], " \t") {
			c = c[:i]
		}
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}
