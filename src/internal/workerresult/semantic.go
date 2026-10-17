package workerresult

// validateSemantics applies the per-status rules (WBS 13.18-13.21, PLAN
// Decision 3). Contradictions are rejected, never converted.
func validateSemantics(d *draft) []ValidationError {
	var errs []ValidationError
	add := func(field, code, msg string) {
		errs = append(errs, ValidationError{Field: field, Code: code, Message: msg})
	}
	switch d.status {
	case StatusComplete:
		if len(d.remaining) > 0 {
			add("remaining", "MUST_BE_EMPTY_FOR_COMPLETE", "a complete result cannot report remaining work")
		}
		if len(d.blocking) > 0 {
			add("blocking", "MUST_BE_EMPTY_FOR_COMPLETE", "a complete result cannot report blockers")
		}
	case StatusIncomplete:
		if len(d.remaining) == 0 {
			add("remaining", "REQUIRED_FOR_INCOMPLETE", "an incomplete result must list remaining work")
		}
		if d.nextAction == "" {
			add("next_action", "REQUIRED_FOR_INCOMPLETE", "an incomplete result must suggest a next action")
		}
	case StatusBlocked:
		if len(d.blocking) == 0 {
			add("blocking", "REQUIRED_FOR_BLOCKED", "a blocked result must list its blockers")
		}
	case StatusFailed:
		if d.summary == "" && len(d.blocking) == 0 {
			add("summary", "REQUIRED_FOR_FAILED", "a failed result must explain the failure")
		}
	}
	return errs
}

// recognize marks acceptance claims whose id belongs to the dispatched
// unit (PLAN Decision 5); unknown ids are retained, never applied.
func recognize(claims []AcceptanceClaim, known []string) {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	for i := range claims {
		claims[i].Recognized = set[claims[i].ID]
	}
}
