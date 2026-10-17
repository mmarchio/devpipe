package continuation

import (
	"fmt"

	"overseer/internal/workerresult"
)

// ConsumeOutcome is what a continuation dispatch's parsed result does to
// the continuation (WBS 14.60).
type ConsumeOutcome struct {
	Target     Status
	CreateNext bool
}

// OutcomeFor maps a worker result status: COMPLETE ends the loop toward
// validation, INCOMPLETE completes this continuation and creates the next
// decision, BLOCKED and FAILED never continue automatically.
func OutcomeFor(s workerresult.WorkerResultStatus) (ConsumeOutcome, error) {
	switch s {
	case workerresult.StatusComplete:
		return ConsumeOutcome{Target: StatusCompleted}, nil
	case workerresult.StatusIncomplete:
		return ConsumeOutcome{Target: StatusCompleted, CreateNext: true}, nil
	case workerresult.StatusBlocked:
		return ConsumeOutcome{Target: StatusBlocked}, nil
	case workerresult.StatusFailed:
		return ConsumeOutcome{Target: StatusFailed}, nil
	}
	return ConsumeOutcome{}, fmt.Errorf("continuation: unknown result status %q", s)
}
