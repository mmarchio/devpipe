package project

// allowedTransitions is the project lifecycle state machine (WBS 3.6),
// implemented independently of the development execution state machine.
// Terminal states (COMPLETED, FAILED, CANCELLED) have no outgoing edges.
var allowedTransitions = map[ProjectStatus]map[ProjectStatus]bool{
	StatusDRAFT:     {StatusREADY: true},
	StatusREADY:     {StatusRUNNING: true},
	StatusRUNNING:   {StatusPAUSED: true, StatusCOMPLETED: true, StatusFAILED: true, StatusCANCELLED: true},
	StatusPAUSED:    {StatusRUNNING: true, StatusCANCELLED: true},
	StatusCOMPLETED: {},
	StatusFAILED:    {},
	StatusCANCELLED: {},
}

// CanTransitionTo reports whether moving a project from status to status
// is an allowed lifecycle transition (WBS 3.6, T3.6.1–T3.6.11).
func CanTransitionTo(from, to ProjectStatus) bool {
	return allowedTransitions[from][to]
}
