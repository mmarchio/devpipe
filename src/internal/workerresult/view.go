package workerresult

import "strings"

// ValidationNotYetRun is shown until a deterministic validation record
// exists (WBS 13.71).
const ValidationNotYetRun = "NOT YET RUN"

// ResultView is the worker-result UI read model (WBS 13.70-13.72). Every
// worker-authored element is labelled as worker-reported.
type ResultView struct {
	StatusLabel     string
	Status          WorkerResultStatus
	ValidationLabel string
	Role            string
	State           string
	Summary         string
	Completed       []string
	Remaining       []string
	Blocking        []string
	BlockingLabel   string
	NextAction      string
	NextActionLabel string
	TestClaimLabel  string
}

// BuildView renders a result; validation is shown independently of the
// worker report and is never inferred from COMPLETE.
func BuildView(r WorkerResult, validationState string) ResultView {
	if validationState == "" {
		validationState = ValidationNotYetRun
	}
	v := ResultView{StatusLabel: "Worker Report: " + strings.ToUpper(string(r.Status)), Status: r.Status, ValidationLabel: "Validation: " + validationState,
		Role: r.Metadata.RoleKey, State: r.Metadata.StateKey, Summary: r.Summary, Completed: noneIfEmpty(r.Completed),
		Remaining: noneIfEmpty(r.Remaining), Blocking: noneIfEmpty(r.Blocking), NextAction: r.NextAction,
		NextActionLabel: "Suggested Next Action (worker-reported, advisory)", BlockingLabel: "Blocking (worker-reported)"}
	if r.IsBlocked() {
		v.BlockingLabel = "Reported Blocker (worker-reported; not the authoritative work-unit status)"
	}
	if r.Tests != nil {
		v.TestClaimLabel = "Tests (worker-reported claim, not a deterministic result)"
	}
	return v
}

func noneIfEmpty(xs []string) []string {
	if len(xs) == 0 {
		return []string{"None"}
	}
	return append([]string{}, xs...)
}
